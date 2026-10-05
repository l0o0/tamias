package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"time"
	"unicode"
	"unicode/utf8"
)

type localFile struct {
	Hash string
	Size int64
}
type Action struct {
	Size           int64  `json:"size,omitempty"`
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	Reason         string `json:"reason"`
	LocalHash      string `json:"-"`
	RemoteETag     string `json:"-"`
	BaselineMerge  bool   `json:"baselineMerge,omitempty"`
	ForgetBaseline bool   `json:"-"`
}
type Plan struct {
	Token                      string    `json:"token"`
	Actions                    []Action  `json:"actions"`
	Created                    time.Time `json:"created"`
	UploadBytes                int64     `json:"uploadBytes"`
	DownloadBytes              int64     `json:"downloadBytes"`
	JobID                      string    `json:"-"`
	ConfigFingerprint          string    `json:"-"`
	DeleteCount                int       `json:"deleteCount"`
	DeletePaths                []string  `json:"deletePaths"`
	RequiresDeleteConfirmation bool      `json:"requiresDeleteConfirmation"`
}
type baseline struct{ local, remote string }

type syncRunCounts struct {
	Completed   int
	Transferred int
	Deleted     int
	Skipped     int
	CreatedDirs int
	Maintenance int
	Other       int
}

func (c *syncRunCounts) record(a Action) {
	c.Completed++
	switch a.Kind {
	case "upload", "download":
		c.Transferred++
	case "delete-local", "delete-remote", "delete-local-dir":
		c.Deleted++
	case "skip":
		if a.ForgetBaseline {
			c.Maintenance++
		} else {
			c.Skipped++
		}
	case "mkdir-local", "mkdir-remote":
		c.CreatedDirs++
	case "baseline":
		c.Maintenance++
	default:
		c.Other++
	}
}

func syncCompletionDetail(total int, counts syncRunCounts) string {
	if total == 0 {
		return "已核对，无需同步"
	}
	var parts []string
	if counts.Transferred > 0 {
		parts = append(parts, fmt.Sprintf("已传输 %d 个变化文件", counts.Transferred))
	}
	if counts.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("已删除 %d 项", counts.Deleted))
	}
	if counts.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("已跳过 %d 项", counts.Skipped))
	}
	if counts.CreatedDirs > 0 {
		parts = append(parts, fmt.Sprintf("已创建 %d 个目录", counts.CreatedDirs))
	}
	if counts.Maintenance > 0 {
		parts = append(parts, fmt.Sprintf("已完成 %d 项同步状态维护", counts.Maintenance))
	}
	if counts.Other > 0 {
		parts = append(parts, fmt.Sprintf("已完成 %d 个其他同步操作", counts.Other))
	}
	return strings.Join(parts, "；")
}

type syncProgressReader struct {
	r         io.Reader
	progress  func(int64)
	bytes     int64
	lastAt    time.Time
	lastBytes int64
}

func (r *syncProgressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.bytes += int64(n)
		now := time.Now()
		if r.progress != nil && (now.Sub(r.lastAt) >= 200*time.Millisecond || r.bytes-r.lastBytes >= 1<<20) {
			r.progress(r.bytes)
			r.lastAt = now
			r.lastBytes = r.bytes
		}
	}
	return n, err
}

const defaultDeleteThreshold = 20

func (s *Service) ensureSyncSchema() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS sync_queue (
		job TEXT NOT NULL, path TEXT NOT NULL, kind TEXT NOT NULL,
		local_hash TEXT NOT NULL DEFAULT '', remote_etag TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL, bytes_done INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '', updated TEXT NOT NULL,
		PRIMARY KEY(job,path,kind))`)
	return err
}

func validSyncName(name string) error {
	if name == "" || !utf8.ValidString(name) {
		return storage.ErrInvalidPath
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return storage.ErrInvalidPath
	}
	for _, r := range name {
		if unicode.IsControl(r) || isUnicodeNoncharacter(r) || strings.ContainsRune(`<>:"|?*\\`, r) {
			return storage.ErrInvalidPath
		}
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return storage.ErrInvalidPath
	}
	return nil
}

var canonicalLatinDecompositions = map[rune]string{
	'À': "a\u0300", 'Á': "a\u0301", 'Â': "a\u0302", 'Ã': "a\u0303", 'Ä': "a\u0308", 'Å': "a\u030a",
	'Ç': "c\u0327", 'È': "e\u0300", 'É': "e\u0301", 'Ê': "e\u0302", 'Ë': "e\u0308", 'İ': "i\u0307",
	'Ì': "i\u0300", 'Í': "i\u0301", 'Î': "i\u0302", 'Ï': "i\u0308", 'Ñ': "n\u0303",
	'Ò': "o\u0300", 'Ó': "o\u0301", 'Ô': "o\u0302", 'Õ': "o\u0303", 'Ö': "o\u0308",
	'Ù': "u\u0300", 'Ú': "u\u0301", 'Û': "u\u0302", 'Ü': "u\u0308", 'Ý': "y\u0301",
	'à': "a\u0300", 'á': "a\u0301", 'â': "a\u0302", 'ã': "a\u0303", 'ä': "a\u0308", 'å': "a\u030a",
	'ç': "c\u0327", 'è': "e\u0300", 'é': "e\u0301", 'ê': "e\u0302", 'ë': "e\u0308",
	'ì': "i\u0300", 'í': "i\u0301", 'î': "i\u0302", 'ï': "i\u0308", 'ñ': "n\u0303",
	'ò': "o\u0300", 'ó': "o\u0301", 'ô': "o\u0302", 'õ': "o\u0303", 'ö': "o\u0308",
	'ù': "u\u0300", 'ú': "u\u0301", 'û': "u\u0302", 'ü': "u\u0308", 'ý': "y\u0301", 'ÿ': "y\u0308",
}

func portablePathIdentity(key string) string {
	// Normalize the common canonical Latin compositions without adding a module
	// dependency. This catches the NFC/NFD spellings most often encountered in
	// cross-platform filenames, then applies Unicode case folding.
	var out strings.Builder
	for _, r := range key {
		r = unicode.ToLower(r)
		if normalized, ok := canonicalLatinDecompositions[r]; ok {
			for _, decomposed := range normalized {
				out.WriteRune(unicodeCaseFoldRune(decomposed))
			}
		} else {
			out.WriteRune(unicodeCaseFoldRune(r))
		}
	}
	return out.String()
}

func unicodeCaseFoldRune(r rune) rune {
	min := r
	for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
		if folded < min {
			min = folded
		}
	}
	return min
}

func isUnicodeNoncharacter(r rune) bool {
	return r >= 0xFDD0 && r <= 0xFDEF || r&0xFFFF == 0xFFFE || r&0xFFFF == 0xFFFF
}

func validSyncPath(k string) error {
	if err := validKey(k); err != nil {
		return err
	}
	if k == "" {
		return storage.ErrInvalidPath
	}
	for _, part := range strings.Split(k, "/") {
		if err := validSyncName(part); err != nil {
			return err
		}
	}
	return nil
}

func validateExcludePatterns(patterns []string) error {
	if len(patterns) > 256 {
		return errors.New("排除规则最多 256 条")
	}
	for _, raw := range patterns {
		p := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
		if p == "" || len(p) > 256 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "../") || p == ".." {
			return fmt.Errorf("无效排除规则：%q", raw)
		}
		for _, part := range strings.Split(p, "/") {
			if part != "**" {
				if _, err := path.Match(part, "probe"); err != nil {
					return fmt.Errorf("无效排除规则 %q：%w", raw, err)
				}
			}
		}
	}
	return nil
}

func globPathMatch(pattern, key string) bool {
	pattern = strings.Trim(strings.ReplaceAll(pattern, "\\", "/"), "/")
	parts, names := strings.Split(pattern, "/"), strings.Split(key, "/")
	if len(parts) == 1 {
		for _, name := range names {
			if ok, _ := path.Match(parts[0], name); ok {
				return true
			}
		}
		return false
	}
	var match func(int, int) bool
	match = func(pi, ni int) bool {
		if pi == len(parts) {
			return ni == len(names)
		}
		if parts[pi] == "**" {
			if match(pi+1, ni) {
				return true
			}
			return ni < len(names) && match(pi, ni+1)
		}
		if ni == len(names) {
			return false
		}
		ok, err := path.Match(parts[pi], names[ni])
		return err == nil && ok && match(pi+1, ni+1)
	}
	return match(0, 0)
}

func excludedPath(patterns []string, key string) bool {
	for _, p := range patterns {
		if globPathMatch(strings.TrimSpace(p), key) {
			return true
		}
	}
	return false
}

func pathWithinProtected(prefix, key string) bool {
	prefix, key = strings.TrimSuffix(filepath.ToSlash(prefix), "/"), filepath.ToSlash(key)
	return key == prefix || strings.HasPrefix(key, prefix+"/")
}

func pathWithinPortablePrefix(prefix, key string) bool {
	if pathWithinProtected(prefix, key) {
		return true
	}
	want := portablePathIdentity(filepath.ToSlash(prefix))
	parts := strings.Split(filepath.ToSlash(key), "/")
	for i := range parts {
		if portablePathIdentity(strings.Join(parts[:i+1], "/")) == want {
			return true
		}
	}
	return false
}

func hashRemote(ctx context.Context, b Backend, key, etag string) (localFile, storage.Entry, error) {
	r, entry, err := b.Open(ctx, key, etag)
	if err != nil {
		return localFile{}, storage.Entry{}, err
	}
	defer r.Close()
	limit := int64(FileLimit)
	if b.Service != nil && b.Service.Preferences().MaxFileBytes > 0 {
		limit = b.Service.Preferences().MaxFileBytes
	}
	if entry.IsDir || entry.Size > limit {
		return localFile{}, storage.Entry{}, errors.New("远端文件大小或类型不受支持")
	}
	expectedSize := entry.Size
	if expectedSize < 0 {
		if !strongTag(etag) {
			return localFile{}, storage.Entry{}, errors.New("远端缺少可靠版本标识，不能核验内容")
		}
		current, statErr := b.Stat(ctx, key)
		if statErr != nil {
			return localFile{}, storage.Entry{}, statErr
		}
		if current.IsDir || current.ETag != etag || current.Size < 0 || current.Size > limit {
			return localFile{}, storage.Entry{}, storage.ErrConflict
		}
		expectedSize = current.Size
		entry.Size = expectedSize
	}
	h := sha256.New()
	reader := io.Reader(r)
	if b.Service != nil {
		reader = b.Service.limitReader(ctx, r)
	}
	n, err := io.Copy(h, io.LimitReader(reader, limit+1))
	if err != nil {
		return localFile{}, storage.Entry{}, err
	}
	if n != expectedSize || n > limit {
		return localFile{}, storage.Entry{}, errors.New("远端文件内容长度与元数据不一致")
	}
	after, err := b.Stat(ctx, key)
	if err != nil {
		return localFile{}, storage.Entry{}, err
	}
	if after.IsDir || (etag != "" && after.ETag != etag) || after.Size != n {
		return localFile{}, storage.Entry{}, storage.ErrConflict
	}
	return localFile{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}, entry, nil
}

func (s *Service) AddJob(j Job) (Job, error) {
	if err := validateJobIcon(j.Icon); err != nil {
		return j, err
	}
	if _, err := s.connection(j.ConnectionID); err != nil {
		return j, err
	}
	if strings.TrimSpace(j.Name) == "" {
		return j, errors.New("请填写任务名称")
	}
	if j.Direction != "both" && j.Direction != "upload" && j.Direction != "download" && j.Direction != "mirror-upload" && j.Direction != "mirror-download" {
		return j, errors.New("无效同步方向")
	}
	if err := validKey(j.RemotePath); err != nil {
		return j, err
	}
	if j.RemotePath != "" {
		for _, part := range strings.Split(j.RemotePath, "/") {
			if err := validSyncName(part); err != nil {
				return j, fmt.Errorf("远端目录名不适用于所有平台：%w", err)
			}
		}
	}
	if err := validateExcludePatterns(j.Exclude); err != nil {
		return j, err
	}
	if err := s.checkManagedResource(context.Background(), j.ConnectionID, j.RemotePath); err != nil {
		return j, err
	}
	local, err := filepath.Abs(j.LocalPath)
	if err != nil {
		return j, err
	}
	local, err = filepath.EvalSymlinks(local)
	if err != nil {
		return j, err
	}
	if isManagedPath(filepath.ToSlash(local)) {
		return j, errors.New("本地目录位于备份任务管理的保留目录中")
	}
	fi, err := os.Stat(local)
	if err != nil || !fi.IsDir() {
		return j, errors.New("请选择有效本地文件夹")
	}
	data, _ := filepath.Abs(s.dir)
	if containsPath(local, data) || containsPath(data, local) {
		return j, errors.New("同步目录不能包含应用数据目录，或位于其内部")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.cfg.Jobs {
		if containsPath(other.LocalPath, local) || containsPath(local, other.LocalPath) {
			return j, errors.New("本地目录与已有任务重叠")
		}
	}
	j.ID = ID()
	j.LocalPath = local
	j.Status = "idle"
	j.ActiveAction = ""
	j.Detail = "先预览，再同步；不自动传播删除"
	j.Enabled = true
	if j.DeleteThreshold <= 0 {
		j.DeleteThreshold = defaultDeleteThreshold
	}
	s.cfg.Jobs = append(s.cfg.Jobs, j)
	if err = s.saveLocked(); err != nil {
		s.cfg.Jobs = s.cfg.Jobs[:len(s.cfg.Jobs)-1]
	}
	return j, err
}

func (s *Service) UpdateJob(j Job) (Job, error) {
	runLock := s.syncJobLock(j.ID)
	runLock.Lock()
	defer runLock.Unlock()

	if err := validateJobIcon(j.Icon); err != nil {
		return j, err
	}
	if strings.TrimSpace(j.Name) == "" {
		return j, errors.New("请填写任务名称")
	}
	if _, err := s.connection(j.ConnectionID); err != nil {
		return j, err
	}
	switch j.Direction {
	case "both", "upload", "download", "mirror-upload", "mirror-download":
	default:
		return j, errors.New("无效同步方向")
	}
	if err := validKey(j.RemotePath); err != nil {
		return j, err
	}
	if j.RemotePath != "" {
		for _, part := range strings.Split(j.RemotePath, "/") {
			if err := validSyncName(part); err != nil {
				return j, fmt.Errorf("远端目录名不适用于所有平台：%w", err)
			}
		}
	}
	if err := validateExcludePatterns(j.Exclude); err != nil {
		return j, err
	}
	if err := s.checkManagedResource(context.Background(), j.ConnectionID, j.RemotePath); err != nil {
		return j, err
	}
	local, err := filepath.Abs(j.LocalPath)
	if err != nil {
		return j, err
	}
	local, err = filepath.EvalSymlinks(local)
	if err != nil {
		return j, err
	}
	if isManagedPath(filepath.ToSlash(local)) {
		return j, errors.New("本地目录位于备份任务管理的保留目录中")
	}
	fi, err := os.Stat(local)
	if err != nil || !fi.IsDir() {
		return j, errors.New("请选择有效本地文件夹")
	}
	data, _ := filepath.Abs(s.dir)
	if containsPath(local, data) || containsPath(data, local) {
		return j, errors.New("同步目录不能包含应用数据目录，或位于其内部")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, old := range s.cfg.Jobs {
		if old.ID == j.ID {
			index = i
			break
		}
	}
	if index < 0 {
		return j, storage.ErrNotFound
	}
	old := s.cfg.Jobs[index]
	if old.Status == "running" {
		return old, errors.New("任务正在执行，无法修改")
	}
	for _, other := range s.cfg.Jobs {
		if other.ID == j.ID {
			continue
		}
		if containsPath(other.LocalPath, local) || containsPath(local, other.LocalPath) {
			return j, errors.New("本地目录与已有任务重叠")
		}
	}
	if j.DeleteThreshold <= 0 {
		j.DeleteThreshold = defaultDeleteThreshold
	}
	j.LocalPath = local
	j.Status = old.Status
	j.ActiveAction = ""
	j.LastRun = old.LastRun
	j.LastScanAt = old.LastScanAt
	j.LastScanSummary = old.LastScanSummary
	j.Progress = old.Progress
	j.QueueTotal = old.QueueTotal
	j.QueueDone = old.QueueDone
	resetBaseline := old.ConnectionID != j.ConnectionID || old.RemotePath != j.RemotePath || old.LocalPath != j.LocalPath || old.Direction != j.Direction || strings.Join(old.Exclude, "\x00") != strings.Join(j.Exclude, "\x00")
	if resetBaseline {
		j.Status = "idle"
		j.Detail = "同步范围已更改，请先预览"
	}

	s.cfg.Jobs[index] = j
	raw, err := s.durableJSONLocked()
	if err != nil {
		s.cfg.Jobs[index] = old
		return old, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.cfg.Jobs[index] = old
		return old, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(raw)); err == nil {
		_, err = tx.Exec("DELETE FROM sync_plans WHERE job=?", j.ID)
	}
	if err == nil {
		_, err = tx.Exec("DELETE FROM sync_queue WHERE job=?", j.ID)
	}
	if err == nil && resetBaseline {
		_, err = tx.Exec("DELETE FROM baseline WHERE job=?", j.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.cfg.Jobs[index] = old
		return old, err
	}
	for token, p := range s.plans {
		if p.JobID == j.ID {
			delete(s.plans, token)
		}
	}

	return j, nil
}

func (s *Service) ResumeJob(id string) error {
	s.mu.Lock()
	for i := range s.cfg.Jobs {
		j := &s.cfg.Jobs[i]
		if j.ID != id {
			continue
		}
		if j.LocalPath == "" {
			s.mu.Unlock()
			return errors.New("请先重新选择本地目录")
		}
		if j.Status == "running" {
			s.mu.Unlock()
			return errors.New("任务正在执行")
		}
		j.Enabled = true
		j.Status = "idle"
		j.ActiveAction = ""
		j.Detail = "任务已恢复，正在等待重新核对"
		err := s.saveLocked()
		s.mu.Unlock()
		if err == nil {
			s.WakeScheduler()
		}
		return err
	}
	s.mu.Unlock()
	return storage.ErrNotFound
}

func (s *Service) CancelJob(id string) error {
	s.mu.Lock()
	var cancel context.CancelFunc
	found := false
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == id {
			found = true
			s.cfg.Jobs[i].Enabled = false
			s.cfg.Jobs[i].ActiveAction = ""
			if s.cfg.Jobs[i].Status != "running" {
				s.cfg.Jobs[i].Status = "paused"
			}
			s.cfg.Jobs[i].Detail = "已取消当前执行；未提交操作已停止"
			break
		}
	}
	if !found {
		s.mu.Unlock()
		return storage.ErrNotFound
	}
	cancel = s.jobCancels[id]
	err := s.saveLocked()
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return err
}

func (s *Service) RetryJob(ctx context.Context, id string) (Job, error) {
	s.mu.Lock()
	found := false
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == id {
			if s.cfg.Jobs[i].Status == "running" {
				s.mu.Unlock()
				return Job{}, errors.New("任务正在执行")
			}
			s.cfg.Jobs[i].Enabled = true
			s.cfg.Jobs[i].Status = "retrying"
			s.cfg.Jobs[i].ActiveAction = ""
			s.cfg.Jobs[i].Detail = "正在重新核对失败项"
			found = true
			break
		}
	}
	if !found {
		s.mu.Unlock()
		return Job{}, storage.ErrNotFound
	}
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	s.mu.Unlock()
	p, err := s.Preview(ctx, id)
	if err != nil {
		status := "error"
		if errors.Is(err, ErrPendingOperation) {
			status = "needs_attention"
		}
		s.setJobAttention(id, "重试前核对失败："+err.Error(), status)
		return Job{}, err
	}
	j, runErr := s.RunPlan(ctx, id, p.Token)
	if runErr != nil {
		status := "error"
		if errors.Is(runErr, ErrPendingOperation) || strings.Contains(runErr.Error(), "冲突") || strings.Contains(runErr.Error(), "确认") {
			status = "needs_attention"
		}
		s.setJobAttention(id, "重试未执行："+runErr.Error(), status)
	}
	return j, runErr
}

func (s *Service) ResolveConflict(ctx context.Context, id, key, choice string) (Job, error) {
	if err := validSyncPath(key); err != nil {
		return Job{}, err
	}
	if err := checkManagedPath(ctx, key); err != nil {
		return Job{}, err
	}
	if choice != "local" && choice != "remote" && choice != "keep-both" {
		return Job{}, errors.New("冲突选择必须是 local、remote 或 keep-both")
	}
	runLock := s.syncJobLock(id)
	runLock.Lock()
	defer runLock.Unlock()
	j, err := s.job(id)
	if err != nil {
		return Job{}, err
	}
	if err = s.checkManagedResource(ctx, j.ConnectionID, path.Join(j.RemotePath, key)); err != nil {
		return j, err
	}
	if j.Status == "running" {
		return j, errors.New("任务正在执行")
	}
	if choice == "local" || choice == "keep-both" {
		if err = s.validateSyncWritePolicy(j.ConnectionID, []Action{{Kind: "upload"}}); err != nil {
			return j, err
		}
	}
	root, err := os.OpenRoot(j.LocalPath)
	if err != nil {
		return j, err
	}
	defer root.Close()
	b := Backend{Service: s, ConnectionID: j.ConnectionID}
	remoteKey := path.Join(j.RemotePath, key)
	local, localErr := s.hashLocal(root, key)
	localExists := localErr == nil
	if localErr != nil && !errors.Is(localErr, os.ErrNotExist) {
		return j, localErr
	}
	remote, remoteErr := b.Stat(ctx, remoteKey)
	remoteExists := remoteErr == nil
	if remoteErr != nil && !errors.Is(remoteErr, storage.ErrNotFound) {
		return j, remoteErr
	}
	if !localExists && !remoteExists {
		return j, errors.New("两侧文件均不存在")
	}
	switch choice {
	case "local":
		if !localExists {
			return j, errors.New("本地版本已不存在")
		}
		if remoteExists && !strongTag(remote.ETag) {
			return j, errors.New("远端缺少可靠版本标识，无法安全覆盖")
		}
		if err = putLocalSync(ctx, s, root, b, j, key, remoteKey, local, remote, remoteExists); err != nil {
			return j, err
		}
	case "remote":
		if !remoteExists {
			return j, errors.New("远端版本已不存在")
		}
		if !strongTag(remote.ETag) {
			return j, errors.New("远端缺少可靠版本标识，无法安全下载")
		}
		want := ""
		if localExists {
			want = local.Hash
		}
		a := Action{Path: key, Kind: "download", LocalHash: want, RemoteETag: remote.ETag}
		if err = s.downloadSafely(ctx, root, b, remoteKey, a, j.ID); err != nil {
			return j, err
		}
	case "keep-both":
		if !localExists || !remoteExists {
			return j, errors.New("双方版本都存在时才能选择保留两份")
		}
		if !strongTag(remote.ETag) {
			return j, errors.New("远端缺少可靠版本标识，无法安全保留两份")
		}
		copyPath := conflictCopyPath(key, "remote")
		for tries := 0; tries < 10; tries++ {
			if _, e := root.Lstat(copyPath); errors.Is(e, os.ErrNotExist) {
				break
			}
			copyPath = conflictCopyPath(key, "remote")
		}
		if _, e := root.Lstat(copyPath); !errors.Is(e, os.ErrNotExist) {
			return j, errors.New("无法生成不冲突的副本名称")
		}
		copyAction := Action{Path: copyPath, Kind: "download", RemoteETag: remote.ETag}
		if err = s.downloadSafely(ctx, root, b, remoteKey, copyAction, j.ID); err != nil {
			return j, err
		}
		_, _ = s.db.Exec("DELETE FROM baseline WHERE job=? AND path=?", j.ID, copyPath)
		local, err = s.hashLocal(root, key)
		if err != nil {
			return j, err
		}
		latest, statErr := b.Stat(ctx, remoteKey)
		if statErr != nil {
			return j, statErr
		}
		if latest.ETag != remote.ETag {
			return j, storage.ErrConflict
		}
		if err = putLocalSync(ctx, s, root, b, j, key, remoteKey, local, latest, true); err != nil {
			return j, err
		}
	}
	_, _ = s.db.Exec("DELETE FROM sync_plans WHERE job=?", id)
	s.mu.Lock()
	for token, p := range s.plans {
		if p.JobID == id {
			delete(s.plans, token)
		}
	}
	s.mu.Unlock()
	updated, _ := s.job(id)
	return updated, nil
}

func putLocalSync(ctx context.Context, s *Service, root *os.Root, b Backend, j Job, key, remoteKey string, local localFile, remote storage.Entry, remoteExists bool) error {
	if current, err := s.hashLocal(root, key); err != nil || current.Hash != local.Hash {
		return storage.ErrConflict
	}
	if err := ensureRemoteParents(ctx, b, remoteKey); err != nil {
		return err
	}
	f, err := root.Open(key)
	if err != nil {
		return err
	}
	cond := storage.Condition{IfNoneMatch: !remoteExists}
	if remoteExists {
		cond.IfMatch = remote.ETag
	}
	result, putErr := b.put(ctx, remoteKey, f, local.Size, cond, local.Hash)
	closeErr := f.Close()
	if putErr != nil {
		return putErr
	}
	if closeErr != nil {
		return closeErr
	}
	after, err := s.hashLocal(root, key)
	if err != nil || after.Hash != local.Hash {
		return storage.ErrConflict
	}
	actual, entry, err := hashRemote(ctx, b, remoteKey, result.ETag)
	if err != nil {
		return err
	}
	if actual.Hash != local.Hash {
		return errors.New("远端内容核验失败")
	}
	return s.saveBaseline(j.ID, key, after.Hash, entry.ETag)
}
func containsPath(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Service) syncJobLock(id string) *sync.Mutex {
	s.mu.Lock()
	if s.jobLocks == nil {
		s.jobLocks = map[string]*sync.Mutex{}
	}
	if s.jobLocks[id] == nil {
		s.jobLocks[id] = &sync.Mutex{}
	}
	lock := s.jobLocks[id]
	s.mu.Unlock()
	return lock
}

func (s *Service) job(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.cfg.Jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return Job{}, storage.ErrNotFound
}

func (s *Service) setJobActiveAction(id, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID != id {
			continue
		}
		if action != "" && (!s.cfg.Jobs[i].Enabled || s.cfg.Jobs[i].Status != "running") {
			return
		}
		s.cfg.Jobs[i].ActiveAction = action
		return
	}
}

func hashLocal(root *os.Root, key string) (localFile, error) {
	return hashLocalLimit(root, key, FileLimit)
}

func hashLocalLimit(root *os.Root, key string, limit int64) (localFile, error) {
	info, err := root.Lstat(key)
	if err != nil {
		return localFile{}, err
	}
	if !info.Mode().IsRegular() {
		return localFile{}, errors.New("同步仅接受普通文件，不跟随符号链接")
	}
	if limit <= 0 {
		limit = FileLimit
	}
	if info.Size() > limit {
		return localFile{}, errors.New("文件超过当前单文件上限，请缩小同步范围")
	}
	f, err := root.Open(key)
	if err != nil {
		return localFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil {
		return localFile{}, err
	}
	after, err := root.Lstat(key)
	if err != nil {
		return localFile{}, err
	}
	if n != info.Size() || !os.SameFile(info, after) || info.ModTime() != after.ModTime() {
		return localFile{}, storage.ErrConflict
	}
	return localFile{Hash: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func (s *Service) hashLocal(root *os.Root, key string) (localFile, error) {
	return hashLocalLimit(root, key, s.Preferences().MaxFileBytes)
}
func scanLocal(ctx context.Context, root *os.Root) (map[string]localFile, error) {
	files, _, err := scanLocalTree(ctx, root)
	return files, err
}

func scanLocalTree(ctx context.Context, root *os.Root, limits ...int64) (map[string]localFile, map[string]bool, error) {
	limit := int64(FileLimit)
	if len(limits) > 0 && limits[0] > 0 {
		limit = limits[0]
	}
	return scanLocalFiltered(ctx, root, limit, nil)
}

func scanLocalFiltered(ctx context.Context, root *os.Root, limit int64, excludes []string) (map[string]localFile, map[string]bool, error) {
	files, dirs, _, err := scanLocalFilteredProtected(ctx, root, limit, excludes)
	return files, dirs, err
}

func scanLocalFilteredProtected(ctx context.Context, root *os.Root, limit int64, excludes []string) (map[string]localFile, map[string]bool, map[string]bool, error) {
	files := map[string]localFile{}
	dirs := map[string]bool{}
	protected := map[string]bool{}
	count := 0
	err := fs.WalkDir(root.FS(), ".", func(k string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 10001 {
			return errors.New("原型每任务最多 10000 个本地条目")
		}
		if isManagedPath(filepath.ToSlash(k)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		key := filepath.ToSlash(k)
		if d.IsDir() {
			if excludedPath(excludes, key) {
				return filepath.SkipDir
			}
		} else if syncPathExcluded(excludes, key) {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tami-download-") || strings.HasPrefix(d.Name(), ".tami-sync-old-") || strings.HasPrefix(d.Name(), ".tami-sync-delete-") {
			return fmt.Errorf("发现未完成同步临时文件 %s；请检查并恢复或移出该文件后再同步", k)
		}
		if d.Type()&os.ModeSymlink != 0 {
			protected[filepath.ToSlash(k)] = true
			return nil
		}
		if d.IsDir() {
			if k != "." {
				if err := validSyncPath(k); err != nil {
					return fmt.Errorf("本地路径名不适用于所有平台 %q：%w", k, err)
				}
				dirs[k] = true
			}
			return nil
		}
		if err := validSyncPath(k); err != nil {
			return fmt.Errorf("本地路径名不适用于所有平台 %q：%w", k, err)
		}
		if len(files) >= 10000 {
			return errors.New("原型每任务最多 10000 个文件")
		}
		v, err := hashLocalLimit(root, k, limit)
		if err != nil {
			return fmt.Errorf("%s：%w", k, err)
		}
		files[k] = v
		return nil
	})
	return files, dirs, protected, err
}
func scanRemote(ctx context.Context, st storage.Store, prefix string) (map[string]storage.Entry, error) {
	files, _, err := scanRemoteTree(ctx, st, prefix)
	return files, err
}

func scanRemoteTree(ctx context.Context, st storage.Store, prefix string) (map[string]storage.Entry, map[string]bool, error) {
	return scanRemoteFiltered(ctx, st, prefix, nil)
}

func scanRemoteFiltered(ctx context.Context, st storage.Store, prefix string, excludes []string) (map[string]storage.Entry, map[string]bool, error) {
	files := map[string]storage.Entry{}
	directories := map[string]bool{}
	queue := []string{strings.TrimSuffix(prefix, "/")}
	seen := map[string]bool{}
	count := 0
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			return nil, nil, errors.New("远端目录循环")
		}
		seen[dir] = true
		entries, err := st.List(ctx, dir)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			count++
			if count > 10000 {
				return nil, nil, errors.New("原型每任务最多 10000 个远端条目")
			}
			if prefix != "" && !strings.HasPrefix(e.Path, strings.TrimSuffix(prefix, "/")+"/") {
				return nil, nil, errors.New("远端返回越界路径")
			}
			rel := strings.TrimPrefix(e.Path, strings.TrimSuffix(prefix, "/")+"/")
			if prefix == "" {
				rel = e.Path
			}
			if isManagedPath(rel) || isManagedPath(e.Path) {
				continue
			}
			if e.IsDir {
				if excludedPath(excludes, rel) {
					continue
				}
			} else if syncPathExcluded(excludes, rel) {
				continue
			}
			if err := validSyncPath(e.Path); err != nil {
				return nil, nil, fmt.Errorf("远端路径名不适用于所有平台 %q：%w", e.Path, err)
			}
			if e.IsDir {
				directories[rel] = true
				queue = append(queue, e.Path)
			} else {
				files[rel] = e
			}
		}
	}
	return files, directories, nil
}
func (s *Service) Preview(ctx context.Context, id string) (preview Plan, err error) {
	runLock := s.syncJobLock(id)
	runLock.Lock()
	defer runLock.Unlock()

	j, err := s.job(id)
	if err != nil {
		return Plan{}, err
	}
	if j.Status == "running" {
		return Plan{}, errors.New("任务正在执行")
	}
	if err = s.reconcileJobOperations(ctx, j); err != nil {
		return Plan{}, err
	}
	s.setJobActiveAction(id, "")
	scanID, scanErr := s.beginSyncScan(j)
	if scanErr != nil {
		return Plan{}, scanErr
	}
	var localCount, remoteCount int
	var localDirCount, remoteDirCount int
	defer func() {
		s.finishSyncScan(scanID, j, preview, err, localCount, remoteCount, localDirCount, remoteDirCount)
	}()
	if err = validateExcludePatterns(j.Exclude); err != nil {
		return Plan{}, err
	}
	if isManagedPath(filepath.ToSlash(j.LocalPath)) {
		return Plan{}, errors.New("本地同步目录位于备份任务管理的保留目录中")
	}
	if err = s.checkManagedResource(ctx, j.ConnectionID, j.RemotePath); err != nil {
		return Plan{}, err
	}
	st, err := s.store(j.ConnectionID)
	if err != nil {
		return Plan{}, err
	}
	connection, err := s.connection(j.ConnectionID)
	if err != nil {
		return Plan{}, err
	}
	root, err := os.OpenRoot(j.LocalPath)
	if err != nil {
		return Plan{}, err
	}
	defer root.Close()
	local, localDirs, protectedLocal, err := scanLocalFilteredProtected(ctx, root, s.Preferences().MaxFileBytes, j.Exclude)
	if err != nil {
		return Plan{}, err
	}
	localCount, localDirCount = len(local), len(localDirs)
	remote, remoteDirs, err := scanRemoteFiltered(ctx, st, j.RemotePath, j.Exclude)
	if err != nil {
		return Plan{}, err
	}
	remoteCount, remoteDirCount = len(remote), len(remoteDirs)
	for key := range protectedLocal {
		for k := range remote {
			if pathWithinProtected(key, k) {
				delete(remote, k)
			}
		}
		for k := range remoteDirs {
			if pathWithinProtected(key, k) {
				delete(remoteDirs, k)
			}
		}
	}
	if err = s.ensureSyncSchema(); err != nil {
		return Plan{}, err
	}
	b := Backend{Service: s, ConnectionID: j.ConnectionID}
	bases := map[string]baseline{}
	rows, err := s.db.Query("SELECT path,local_hash,remote_etag FROM baseline WHERE job=?", id)
	if err != nil {
		return Plan{}, err
	}
	for rows.Next() {
		var k string
		var v baseline
		if err = rows.Scan(&k, &v.local, &v.remote); err != nil {
			rows.Close()
			return Plan{}, err
		}
		bases[k] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Plan{}, err
	}
	retired := map[string]bool{}
	pathProtected := func(k string) bool {
		if isManagedPath(k) || isSyncMetadataPath(k) {
			return true
		}
		for protected := range protectedLocal {
			if pathWithinProtected(protected, k) {
				return true
			}
		}
		return false
	}
	for k := range bases {
		if pathProtected(k) {
			delete(bases, k)
			retired[k] = true
		}
	}
	queueRows, err := s.db.Query("SELECT path FROM sync_queue WHERE job=?", id)
	if err != nil {
		return Plan{}, err
	}
	for queueRows.Next() {
		var k string
		if err = queueRows.Scan(&k); err != nil {
			queueRows.Close()
			return Plan{}, err
		}
		if pathProtected(k) {
			retired[k] = true
		}
	}
	err = queueRows.Err()
	queueRows.Close()
	if err != nil {
		return Plan{}, err
	}
	retiredPaths := make([]string, 0, len(retired))
	for k := range retired {
		retiredPaths = append(retiredPaths, k)
	}
	plan := Plan{Token: ID(), JobID: id, ConfigFingerprint: syncPlanConfigFingerprint(j), Created: time.Now(), Actions: []Action{}, DeletePaths: []string{}}
	keys := map[string]bool{}
	for k := range local {
		keys[k] = true
	}
	for k := range remote {
		keys[k] = true
	}
	for k := range bases {
		if !syncPathExcluded(j.Exclude, k) {
			keys[k] = true
		}
	}
	for k := range localDirs {
		keys[k] = true
	}
	for k := range remoteDirs {
		keys[k] = true
	}

	// A case-only or file/directory collision must be resolved before a portable
	// path can be written on another operating system.
	localKinds := map[string]string{}
	remoteKinds := map[string]string{}
	for k := range local {
		localKinds[k] = "file"
	}
	for k := range localDirs {
		localKinds[k] = "dir"
	}
	for k := range remote {
		remoteKinds[k] = "file"
	}
	for k := range remoteDirs {
		remoteKinds[k] = "dir"
	}
	localByFold, remoteByFold := map[string]string{}, map[string]string{}
	localGroups, remoteGroups := map[string][]string{}, map[string][]string{}
	for k := range localKinds {
		folded := portablePathIdentity(k)
		localByFold[folded] = k
		localGroups[folded] = append(localGroups[folded], k)
	}
	for k := range remoteKinds {
		folded := portablePathIdentity(k)
		remoteByFold[folded] = k
		remoteGroups[folded] = append(remoteGroups[folded], k)
	}
	blocked := map[string]bool{}
	blockedRoots := []string{}
	for _, groups := range []map[string][]string{localGroups, remoteGroups} {
		for _, group := range groups {
			if len(group) < 2 {
				continue
			}
			sort.Strings(group)
			plan.Actions = append(plan.Actions, Action{Path: group[0], Kind: "conflict", Reason: "同一侧存在大小写或 Unicode 规范化后重名的路径"})
			for _, k := range group {
				blocked[k] = true
				blockedRoots = append(blockedRoots, k)
				keys[k] = false
			}
		}
	}
	for folded, lk := range localByFold {
		rk, exists := remoteByFold[folded]
		if !exists || lk == rk && localKinds[lk] == remoteKinds[rk] && !blocked[lk] {
			continue
		}
		shown := lk
		if rk < shown {
			shown = rk
		}
		plan.Actions = append(plan.Actions, Action{Path: shown, Kind: "conflict", Reason: "两侧存在大小写、Unicode 或文件与目录名称冲突"})
		blocked[lk], blocked[rk] = true, true
		blockedRoots = append(blockedRoots, lk, rk)
		keys[lk], keys[rk] = false, false
	}
	ordered := make([]string, 0, len(keys))
	for k, include := range keys {
		if include {
			ordered = append(ordered, k)
		}
	}
	sort.Strings(ordered)
	for _, k := range ordered {
		isBlocked := blocked[k]
		for _, rootPath := range blockedRoots {
			if pathWithinPortablePrefix(rootPath, k) {
				isBlocked = true
				break
			}
		}
		if isBlocked {
			continue
		}
		_, ld := localDirs[k]
		_, rd := remoteDirs[k]
		l, le := local[k]
		r, re := remote[k]
		b0, hasBase := bases[k]
		a := Action{Path: k, Kind: "skip", Reason: "内容未变化", LocalHash: l.Hash, RemoteETag: r.ETag}
		if (le && rd) || (re && ld) {
			a.Kind, a.Reason = "conflict", "一侧把此路径作为文件，另一侧作为目录"
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if ld || rd {
			switch {
			case ld && rd:
				a.Reason = "目录已存在两侧"
			case ld:
				switch j.Direction {
				case "both", "upload", "mirror-upload":
					a.Kind, a.Reason = "mkdir-remote", "本地目录需要在远端创建"
				case "mirror-download":
					empty, emptyErr := localDirectoryEmpty(root, k)
					if emptyErr != nil {
						return Plan{}, fmt.Errorf("检查本地目录 %s：%w", k, emptyErr)
					}
					if empty {
						a.Kind, a.Reason = "delete-local-dir", "镜像下载：安全移除已确认空的本地目录"
					} else {
						a.Reason = "镜像下载保留含有文件或排除项的本地目录"
					}
				default:
					a.Reason = "仅下载，保留本地独有目录"
				}
			case rd:
				switch j.Direction {
				case "both", "download", "mirror-download":
					a.Kind, a.Reason = "mkdir-local", "远端目录需要在本地创建"
				case "mirror-upload":
					a.Reason = "镜像上传保留远端目录，避免递归删除未受保护的子项"
				default:
					a.Reason = "仅上传，保留远端独有目录"
				}
			}
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if !le && !re {
			if hasBase {
				a.ForgetBaseline = true
				a.Reason = "两侧均已删除，清理基线"
			} else {
				a.Reason = "两侧均不存在"
			}
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if le && !re {
			switch j.Direction {
			case "download":
				a.Reason = "仅下载，保留本地独有文件"
			case "mirror-download":
				a.Kind, a.Reason = "delete-local", "镜像下载：本地存在远端没有的文件"
			default:
				a.Kind, a.Reason = "upload", "本地存在远端没有的文件"
			}
			if hasBase && j.Direction == "both" {
				if l.Hash == b0.local {
					a.Kind, a.Reason = "delete-local", "远端删除且本地内容未变"
				} else {
					a.Kind, a.Reason = "conflict", "远端删除期间本地内容也发生修改"
				}
			}
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if !le && re {
			switch j.Direction {
			case "upload":
				a.Reason = "仅上传，保留远端独有文件"
			case "mirror-upload":
				a.Kind, a.Reason = "delete-remote", "镜像上传：远端存在本地没有的文件"
			default:
				a.Kind, a.Reason = "download", "远端存在本地没有的文件"
			}
			if hasBase && j.Direction == "both" && r.ETag == b0.remote {
				a.Kind, a.Reason = "delete-remote", "本地删除且远端未变"
			} else if hasBase && j.Direction == "both" && r.ETag != b0.remote {
				a.Kind, a.Reason = "conflict", "本地删除期间远端也发生修改"
			}
			plan.Actions = append(plan.Actions, a)
			continue
		}

		if !hasBase {
			remoteHash, _, hashErr := hashRemote(ctx, b, path.Join(j.RemotePath, k), r.ETag)
			if hashErr != nil {
				return Plan{}, fmt.Errorf("核验远端 %s：%w", k, hashErr)
			}
			if l.Hash == remoteHash.Hash {
				a.BaselineMerge, a.Reason = true, "首次同步同名文件内容一致，合并基线"
			} else {
				a.Kind, a.Reason = "conflict", "首次同步同名文件内容不同，请选择保留方式"
			}
			plan.Actions = append(plan.Actions, a)
			continue
		}
		localChanged, remoteChanged := l.Hash != b0.local, r.ETag != b0.remote
		if !remoteChanged {
			if !localChanged {
				plan.Actions = append(plan.Actions, a)
				continue
			}
			switch j.Direction {
			case "download", "mirror-download":
				a.Kind, a.Reason = "conflict", "仅下载方向中的本地内容已变化，保留双方内容"
			default:
				a.Kind, a.Reason = "upload", "本地内容已变化"
			}
			plan.Actions = append(plan.Actions, a)
			continue
		}
		remoteHash, _, hashErr := hashRemote(ctx, b, path.Join(j.RemotePath, k), r.ETag)
		if hashErr != nil {
			return Plan{}, fmt.Errorf("核验远端 %s：%w", k, hashErr)
		}
		if l.Hash == remoteHash.Hash {
			a.BaselineMerge, a.Reason = true, "两侧内容一致，更新版本基线"
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if !localChanged {
			switch j.Direction {
			case "upload", "mirror-upload":
				a.Kind, a.Reason = "conflict", "远端内容已变化，保留本地与远端版本"
			default:
				a.Kind, a.Reason = "download", "远端内容已变化，本地内容与基线一致"
			}
		} else {
			a.Kind, a.Reason = "conflict", "本地与远端均已变化，保留双方内容"
		}
		plan.Actions = append(plan.Actions, a)
	}
	sort.SliceStable(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Path != plan.Actions[j].Path {
			return plan.Actions[i].Path < plan.Actions[j].Path
		}
		return plan.Actions[i].Kind < plan.Actions[j].Kind
	})
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Kind == "delete-local" || a.Kind == "delete-remote" || a.Kind == "delete-local-dir" {
			plan.DeleteCount++
			plan.DeletePaths = append(plan.DeletePaths, a.Path)
			if a.Kind == "delete-remote" {
				r := remote[a.Path]
				if !strongTag(r.ETag) || r.Size > s.Preferences().MaxFileBytes {
					a.Kind, a.Reason = "conflict", "远端版本或大小无法安全保留，已停止删除"
					plan.DeleteCount--
					plan.DeletePaths = plan.DeletePaths[:len(plan.DeletePaths)-1]
				}
			}
		}
		if a.Kind == "upload" && remote[a.Path].Path != "" && !strongTag(remote[a.Path].ETag) {
			a.Kind, a.Reason = "conflict", "远端缺少可靠版本标识，无法安全覆盖"
		}
		if a.Kind == "download" && !strongTag(remote[a.Path].ETag) {
			a.Kind, a.Reason = "conflict", "远端缺少可靠版本标识，无法安全下载和建立基线"
		}
		needsRemoteSizeBound := a.Kind == "download" || a.Kind == "upload" && remote[a.Path].Path != "" && connection.KeepRecovery || a.BaselineMerge
		if needsRemoteSizeBound && remote[a.Path].Size > s.Preferences().MaxFileBytes {
			a.Kind, a.BaselineMerge, a.Reason = "conflict", false, "远端文件超过当前单文件上限"
		}
		if a.BaselineMerge && !strongTag(remote[a.Path].ETag) {
			a.Kind, a.BaselineMerge, a.Reason = "conflict", false, "远端缺少可靠版本标识，不能建立可验证的基线"
		}
		switch a.Kind {
		case "upload":
			a.Size = local[a.Path].Size
			plan.UploadBytes += a.Size
		case "download":
			a.Size = remote[a.Path].Size
			plan.DownloadBytes += a.Size
		}
	}
	threshold := j.DeleteThreshold
	if threshold <= 0 {
		threshold = defaultDeleteThreshold
	}
	plan.RequiresDeleteConfirmation = plan.DeleteCount >= threshold
	if err = s.validateSyncWritePolicy(j.ConnectionID, plan.Actions); err != nil {
		return Plan{}, err
	}
	// A denied write or delete must leave the prior plan, queue, and baselines
	// intact so the user can review or change the connection policy.
	if len(retiredPaths) > 0 {
		if err = s.retireSyncPaths(id, retiredPaths); err != nil {
			return Plan{}, err
		}
	}
	if err = s.ensureSyncSchema(); err != nil {
		return Plan{}, err
	}
	var oldToken, oldRaw string
	oldErr := s.db.QueryRow("SELECT token,data FROM sync_plans WHERE job=?", id).Scan(&oldToken, &oldRaw)
	if oldErr == nil {
		oldPlan, decodeErr := unmarshalPlan(oldRaw)
		age := time.Since(oldPlan.Created)
		if decodeErr == nil && oldToken != "" && oldPlan.JobID == id && age >= 0 && age < 10*time.Minute && equivalentSyncPlan(oldPlan, plan) {
			plan.Token, plan.Created = oldToken, oldPlan.Created
		}
	} else if !errors.Is(oldErr, sql.ErrNoRows) {
		return Plan{}, oldErr
	}
	data, err := marshalPlan(plan)
	if err != nil {
		return Plan{}, err
	}
	if plan.Token != oldToken {
		if _, err = s.db.Exec("INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?) ON CONFLICT(job) DO UPDATE SET token=excluded.token,data=excluded.data,created=excluded.created", id, plan.Token, string(data), plan.Created.Format(time.RFC3339Nano)); err != nil {
			return Plan{}, err
		}
	}
	s.mu.Lock()
	for token, old := range s.plans {
		if old.JobID == id || time.Since(old.Created) > 15*time.Minute {
			delete(s.plans, token)
		}
	}
	s.plans[plan.Token] = plan
	s.mu.Unlock()
	return plan, nil
}

func (s *Service) validateSyncWritePolicy(connectionID string, actions []Action) error {
	connection, err := s.connection(connectionID)
	if err != nil {
		return err
	}
	for _, action := range actions {
		switch action.Kind {
		case "upload", "mkdir-remote":
			if !canUpload(connection) {
				return errors.New("此同步计划需要远端写入；请验证条件写入能力，或切换到兼容上传模式")
			}
		case "delete-remote":
			if !canDelete(connection) {
				return errors.New("此同步计划需要条件删除能力，已保留远端文件")
			}
		}
	}
	return nil
}

func equivalentSyncPlan(a, b Plan) bool {
	if a.JobID != b.JobID || a.ConfigFingerprint != b.ConfigFingerprint || a.DeleteCount != b.DeleteCount || a.UploadBytes != b.UploadBytes || a.DownloadBytes != b.DownloadBytes || a.RequiresDeleteConfirmation != b.RequiresDeleteConfirmation || len(a.DeletePaths) != len(b.DeletePaths) || len(a.Actions) != len(b.Actions) {
		return false
	}
	for i := range a.DeletePaths {
		if a.DeletePaths[i] != b.DeletePaths[i] {
			return false
		}
	}
	for i := range a.Actions {
		x, y := a.Actions[i], b.Actions[i]
		if x.Path != y.Path || x.Size != y.Size || x.Kind != y.Kind || x.Reason != y.Reason || x.LocalHash != y.LocalHash || x.RemoteETag != y.RemoteETag || x.BaselineMerge != y.BaselineMerge || x.ForgetBaseline != y.ForgetBaseline {
			return false
		}
	}
	return true
}

type storedAction struct {
	Size                                      int64
	Path, Kind, Reason, LocalHash, RemoteETag string
	BaselineMerge, ForgetBaseline             bool
}
type storedPlan struct {
	Token, JobID               string
	ConfigFingerprint          string
	Created                    time.Time
	UploadBytes, DownloadBytes int64
	Actions                    []storedAction
	DeleteCount                int
	DeletePaths                []string
	RequiresDeleteConfirmation bool
}

func marshalPlan(p Plan) ([]byte, error) {
	r := storedPlan{Token: p.Token, JobID: p.JobID, ConfigFingerprint: p.ConfigFingerprint, Created: p.Created, UploadBytes: p.UploadBytes, DownloadBytes: p.DownloadBytes, DeleteCount: p.DeleteCount, DeletePaths: p.DeletePaths, RequiresDeleteConfirmation: p.RequiresDeleteConfirmation}
	for _, a := range p.Actions {
		r.Actions = append(r.Actions, storedAction{a.Size, a.Path, a.Kind, a.Reason, a.LocalHash, a.RemoteETag, a.BaselineMerge, a.ForgetBaseline})
	}
	return json.Marshal(r)
}
func unmarshalPlan(raw string) (Plan, error) {
	var r storedPlan
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Plan{}, err
	}
	p := Plan{Token: r.Token, JobID: r.JobID, ConfigFingerprint: r.ConfigFingerprint, Created: r.Created, UploadBytes: r.UploadBytes, DownloadBytes: r.DownloadBytes, DeleteCount: r.DeleteCount, DeletePaths: r.DeletePaths, RequiresDeleteConfirmation: r.RequiresDeleteConfirmation}
	for _, a := range r.Actions {
		p.Actions = append(p.Actions, Action{Size: a.Size, Path: a.Path, Kind: a.Kind, Reason: a.Reason, LocalHash: a.LocalHash, RemoteETag: a.RemoteETag, BaselineMerge: a.BaselineMerge, ForgetBaseline: a.ForgetBaseline})
	}
	return p, nil
}
func (s *Service) storedPlan(id, token string) (Plan, error) {
	current, err := s.job(id)
	if err != nil {
		return Plan{}, errors.New("预览已过期，请重新预览")
	}
	wantFingerprint := syncPlanConfigFingerprint(current)
	validate := func(p Plan) (Plan, error) {
		if p.Created.IsZero() || time.Since(p.Created) > 15*time.Minute {
			return Plan{}, errors.New("预览已过期，请重新预览")
		}
		if p.ConfigFingerprint == "" || p.ConfigFingerprint != wantFingerprint {
			return Plan{}, errors.New("同步任务配置已更改，请重新预览")
		}
		return p, nil
	}
	s.mu.Lock()
	for _, p := range s.plans {
		if p.JobID == id && p.Token == token {
			s.mu.Unlock()
			return validate(p)
		}
	}
	s.mu.Unlock()
	var raw, savedToken string
	if err := s.db.QueryRow("SELECT token,data FROM sync_plans WHERE job=?", id).Scan(&savedToken, &raw); err != nil {
		return Plan{}, errors.New("预览已过期，请重新预览")
	}
	if savedToken != token {
		return Plan{}, errors.New("预览已过期，请重新预览")
	}
	p, err := unmarshalPlan(raw)
	if err != nil || p.JobID != id || time.Since(p.Created) > 15*time.Minute {
		return Plan{}, errors.New("预览已过期，请重新预览")
	}
	return validate(p)
}

func syncPlanConfigFingerprint(j Job) string {
	config := struct {
		ConnectionID    string
		RemotePath      string
		LocalPath       string
		Direction       string
		Exclude         []string
		DeleteThreshold int
		BehaviorVersion string
	}{j.ConnectionID, j.RemotePath, filepath.Clean(j.LocalPath), j.Direction, append([]string(nil), j.Exclude...), j.DeleteThreshold, "sync-exclusion-ds-store-v1"}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *Service) saveBaseline(jobID, key, localHash, remoteETag string) error {
	_, err := s.db.Exec("INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?) ON CONFLICT(job,path) DO UPDATE SET local_hash=excluded.local_hash,remote_etag=excluded.remote_etag", jobID, key, localHash, remoteETag)
	return err
}

// retireSyncPaths forgets every prior deletion/write signal for paths that are
// protected during this scan. Removing the old plan in the same transaction
// ensures a crash between retirement and saving the replacement plan cannot
// leave a stale token that still authorizes those paths.
func (s *Service) retireSyncPaths(jobID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM sync_plans WHERE job=?", jobID); err != nil {
		return err
	}
	for i, key := range paths {
		if i > 0 && key == paths[i-1] {
			continue
		}
		if _, err = tx.Exec("DELETE FROM baseline WHERE job=? AND path=?", jobID, key); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM sync_queue WHERE job=? AND path=?", jobID, key); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for token, p := range s.plans {
		if p.JobID == jobID {
			delete(s.plans, token)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) setQueue(jobID string, a Action, state string, runErr error) error {
	if err := s.ensureSyncSchema(); err != nil {
		return err
	}
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	_, err := s.db.Exec(`INSERT INTO sync_queue(job,path,kind,local_hash,remote_etag,state,bytes_done,error,updated)
		VALUES(?,?,?,?,?,?,0,?,?) ON CONFLICT(job,path,kind) DO UPDATE SET local_hash=excluded.local_hash,remote_etag=excluded.remote_etag,state=excluded.state,bytes_done=CASE WHEN excluded.state IN ('pending','running') THEN 0 ELSE sync_queue.bytes_done END,error=excluded.error,updated=excluded.updated`,
		jobID, a.Path, a.Kind, a.LocalHash, a.RemoteETag, state, message, time.Now().Format(time.RFC3339Nano))
	return err
}
func (s *Service) RunPlan(ctx context.Context, id, token string, confirmDeletes ...bool) (Job, error) {
	ctx = withTransferScope(ctx, "job", id)
	runLock := s.syncJobLock(id)
	runLock.Lock()
	defer runLock.Unlock()
	p, err := s.storedPlan(id, token)
	if err != nil {
		return Job{}, err
	}
	job, err := s.job(id)
	if err != nil {
		return Job{}, err
	}
	if isManagedPath(filepath.ToSlash(job.LocalPath)) {
		return Job{}, errors.New("本地同步目录位于备份任务管理的保留目录中")
	}
	if err = s.checkManagedResource(ctx, job.ConnectionID, job.RemotePath); err != nil {
		return Job{}, err
	}
	for _, a := range p.Actions {
		if err = checkManagedPath(ctx, a.Path); err != nil {
			return Job{}, err
		}
		if err = s.checkManagedResource(ctx, job.ConnectionID, path.Join(job.RemotePath, a.Path)); err != nil {
			return Job{}, err
		}
	}
	if err = s.validateSyncWritePolicy(job.ConnectionID, p.Actions); err != nil {
		return Job{}, err
	}
	if p.RequiresDeleteConfirmation && (len(confirmDeletes) == 0 || !confirmDeletes[0]) {
		return Job{}, errors.New("此预览包含大量删除，请明确确认后再执行")
	}
	for _, a := range p.Actions {
		if a.Kind == "conflict" {
			return Job{}, errors.New("预览包含冲突，请先处理后重新预览")
		}
	}
	s.mu.Lock()
	index := -1
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return Job{}, storage.ErrNotFound
	}
	if !s.cfg.Jobs[index].Enabled {
		s.mu.Unlock()
		return Job{}, errors.New("任务已暂停，请先恢复任务后重新预览")
	}
	if s.cfg.Jobs[index].Status == "running" {
		s.mu.Unlock()
		return Job{}, errors.New("任务已经在执行")
	}
	j := s.cfg.Jobs[index]
	jobCtx, cancel := context.WithCancel(ctx)
	s.jobCancels[id] = cancel
	s.cfg.Jobs[index].Status = "running"
	s.cfg.Jobs[index].ActiveAction = ""
	s.cfg.Jobs[index].Enabled = true
	s.cfg.Jobs[index].Progress = 0
	s.cfg.Jobs[index].QueueTotal = len(p.Actions)
	s.cfg.Jobs[index].QueueDone = 0
	s.cfg.Jobs[index].Detail = "正在按预览执行"
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		cancel()
		return j, err
	}
	defer func() { cancel(); s.mu.Lock(); delete(s.jobCancels, id); s.mu.Unlock() }()
	var counts syncRunCounts
	if err = s.ensureSyncSchema(); err != nil {
		return s.finishJob(id, err, counts)
	}
	if _, err = s.db.Exec("DELETE FROM sync_plans WHERE job=? AND token=?", id, token); err != nil {
		return s.finishJob(id, err, counts)
	}
	// A freshly revalidated plan supersedes pending queue rows left by a
	// cancelled run. Rebuild the queue from this plan so stale paths cannot keep
	// waking the scheduler indefinitely.
	_, _ = s.db.Exec("UPDATE sync_queue SET state='done',error='',updated=? WHERE job=? AND state='pending'", time.Now().Format(time.RFC3339Nano), id)
	s.mu.Lock()
	delete(s.plans, token)
	s.mu.Unlock()
	for _, a := range p.Actions {
		if err = s.setQueue(id, a, "pending", nil); err != nil {
			return s.finishJob(id, err, counts)
		}
	}
	root, err := os.OpenRoot(j.LocalPath)
	if err != nil {
		return s.finishJob(id, err, counts)
	}
	defer root.Close()
	b := Backend{Service: s, ConnectionID: j.ConnectionID}
	for _, a := range p.Actions {
		if jobCtx.Err() != nil {
			err = jobCtx.Err()
			break
		}
		current, getErr := s.job(id)
		if getErr != nil {
			err = getErr
			break
		}
		if !current.Enabled {
			err = errors.New("任务已暂停，请重新预览后继续")
			break
		}
		if a.Kind == "skip" && !a.BaselineMerge && !a.ForgetBaseline {
			_ = s.setQueue(id, a, "done", nil)
			counts.record(a)
			s.updateProgress(id, counts.Completed, len(p.Actions))
			continue
		}
		if err = s.setQueue(id, a, "running", nil); err != nil {
			break
		}
		s.setJobActiveAction(id, a.Kind)
		err = s.executeSyncAction(jobCtx, j, root, b, a)
		if err != nil {
			queueState := "error"
			if errors.Is(err, context.Canceled) {
				queueState = "pending"
			}
			_ = s.setQueue(id, a, queueState, err)
			break
		}
		if err = s.setQueue(id, a, "done", nil); err != nil {
			break
		}
		counts.record(a)
		s.updateProgress(id, counts.Completed, len(p.Actions))
		s.setJobActiveAction(id, "")
	}
	return s.finishJob(id, err, counts)
}

func (s *Service) updateProgress(id string, done, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == id {
			s.cfg.Jobs[i].QueueDone = done
			s.cfg.Jobs[i].QueueTotal = total
			if total > 0 {
				s.cfg.Jobs[i].Progress = done * 100 / total
			}
			_ = s.saveLocked()
			return
		}
	}
}

func (s *Service) updateTransferProgress(id, key, kind string, doneBytes, totalBytes int64) {
	if doneBytes < 0 {
		doneBytes = 0
	}
	if totalBytes > 0 && doneBytes > totalBytes {
		doneBytes = totalBytes
	}
	_, _ = s.db.Exec("UPDATE sync_queue SET bytes_done=?,updated=? WHERE job=? AND path=? AND kind=?", doneBytes, time.Now().Format(time.RFC3339Nano), id, key, kind)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		j := &s.cfg.Jobs[i]
		if j.ID != id {
			continue
		}
		if j.QueueTotal > 0 {
			fraction := 0
			if totalBytes > 0 {
				fraction = int(doneBytes * 100 / totalBytes)
			}
			j.Progress = (j.QueueDone*100 + fraction) / j.QueueTotal
		}
		_ = s.saveLocked()
		return
	}
}

func (s *Service) executeSyncAction(ctx context.Context, j Job, root *os.Root, b Backend, a Action) error {
	if err := validSyncPath(a.Path); err != nil {
		return err
	}
	if err := checkManagedPath(ctx, a.Path); err != nil {
		return err
	}
	if err := s.checkManagedResource(ctx, j.ConnectionID, path.Join(j.RemotePath, a.Path)); err != nil {
		return err
	}
	if err := s.validateSyncWritePolicy(j.ConnectionID, []Action{a}); err != nil {
		return err
	}
	if err := rejectLocalSymlinkPath(root, a.Path); err != nil {
		return err
	}
	remoteKey := path.Join(j.RemotePath, a.Path)
	switch a.Kind {
	case "mkdir-local":
		remote, err := b.Stat(ctx, remoteKey)
		if err != nil {
			return err
		}
		if !remote.IsDir {
			return storage.ErrConflict
		}
		if info, err := root.Lstat(a.Path); err == nil {
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				return nil
			}
			return storage.ErrConflict
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := root.MkdirAll(a.Path, 0700); err != nil {
			return err
		}
		info, err := root.Lstat(a.Path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return storage.ErrConflict
		}
		return nil
	case "mkdir-remote":
		local, err := root.Lstat(a.Path)
		if err != nil {
			return err
		}
		if !local.IsDir() || local.Mode()&os.ModeSymlink != 0 {
			return storage.ErrConflict
		}
		if current, err := b.Stat(ctx, remoteKey); err == nil {
			if current.IsDir {
				return nil
			}
			return storage.ErrConflict
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err := ensureRemoteParents(ctx, b, remoteKey); err != nil {
			return err
		}
		if err := b.Mkdir(ctx, remoteKey); err != nil {
			return err
		}
		created, err := b.Stat(ctx, remoteKey)
		if err != nil {
			return err
		}
		if !created.IsDir {
			return storage.ErrConflict
		}
		return nil
	case "delete-local-dir":
		empty, err := localDirectoryEmpty(root, a.Path)
		if err != nil {
			return err
		}
		if !empty {
			return storage.ErrConflict
		}
		if err := root.Remove(a.Path); err != nil {
			return err
		}
		if _, err := root.Lstat(a.Path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return storage.ErrConflict
			}
			return err
		}
		return nil
	case "baseline":
		v, err := s.hashLocal(root, a.Path)
		if err != nil || v.Hash != a.LocalHash {
			return storage.ErrConflict
		}
		rv, entry, err := hashRemote(ctx, b, remoteKey, a.RemoteETag)
		if err != nil {
			return err
		}
		if rv.Hash != v.Hash {
			return storage.ErrConflict
		}
		return s.saveBaseline(j.ID, a.Path, v.Hash, entry.ETag)
	case "skip":
		if a.BaselineMerge {
			v, err := s.hashLocal(root, a.Path)
			if err != nil || v.Hash != a.LocalHash {
				return storage.ErrConflict
			}
			rv, entry, err := hashRemote(ctx, b, remoteKey, a.RemoteETag)
			if err != nil {
				return err
			}
			if rv.Hash != v.Hash {
				return storage.ErrConflict
			}
			return s.saveBaseline(j.ID, a.Path, v.Hash, entry.ETag)
		}
		if a.ForgetBaseline {
			if _, err := root.Lstat(a.Path); !errors.Is(err, os.ErrNotExist) {
				return storage.ErrConflict
			}
			if _, err := b.Stat(ctx, remoteKey); !errors.Is(err, storage.ErrNotFound) {
				if err == nil {
					return storage.ErrConflict
				}
				return err
			}
			_, err := s.db.Exec("DELETE FROM baseline WHERE job=? AND path=?", j.ID, a.Path)
			return err
		}
		return nil
	case "upload":
		v, err := s.hashLocal(root, a.Path)
		if err != nil || v.Hash != a.LocalHash {
			return storage.ErrConflict
		}
		if err = ensureRemoteParents(ctx, b, remoteKey); err != nil {
			return err
		}
		current, statErr := b.Stat(ctx, remoteKey)
		if a.RemoteETag == "" {
			if !errors.Is(statErr, storage.ErrNotFound) {
				if statErr == nil {
					return storage.ErrConflict
				}
				return statErr
			}
		} else if statErr != nil || current.ETag != a.RemoteETag {
			if statErr != nil {
				return statErr
			}
			return storage.ErrConflict
		}
		f, err := root.Open(a.Path)
		if err != nil {
			return err
		}
		cond := storage.Condition{IfNoneMatch: a.RemoteETag == "", IfMatch: a.RemoteETag}
		progress := &syncProgressReader{r: f, progress: func(done int64) { s.updateTransferProgress(j.ID, a.Path, a.Kind, done, v.Size) }}
		e, putErr := b.put(ctx, remoteKey, progress, v.Size, cond, a.LocalHash)
		closeErr := f.Close()
		if putErr != nil {
			return putErr
		}
		if closeErr != nil {
			return closeErr
		}
		after, err := s.hashLocal(root, a.Path)
		if err != nil || after.Hash != a.LocalHash {
			return storage.ErrConflict
		}
		rv, verified, err := hashRemote(ctx, b, remoteKey, e.ETag)
		if err != nil {
			return err
		}
		if rv.Hash != a.LocalHash {
			return errors.New("上传后远端内容核验失败")
		}
		return s.saveBaseline(j.ID, a.Path, after.Hash, verified.ETag)
	case "download":
		return s.downloadSafely(ctx, root, b, remoteKey, a, j.ID)
	case "delete-remote":
		if _, err := root.Lstat(a.Path); !errors.Is(err, os.ErrNotExist) {
			return storage.ErrConflict
		}
		current, err := b.Stat(ctx, remoteKey)
		if err != nil {
			return err
		}
		if current.ETag != a.RemoteETag || !strongTag(current.ETag) {
			return storage.ErrConflict
		}
		if err = b.Delete(ctx, remoteKey, storage.Condition{IfMatch: a.RemoteETag}); err != nil {
			return err
		}
		if _, err = b.Stat(ctx, remoteKey); !errors.Is(err, storage.ErrNotFound) {
			if err == nil {
				return storage.ErrConflict
			}
			return err
		}
		_, err = s.db.Exec("DELETE FROM baseline WHERE job=? AND path=?", j.ID, a.Path)
		return err
	case "delete-local":
		if _, err := b.Stat(ctx, remoteKey); !errors.Is(err, storage.ErrNotFound) {
			if err == nil {
				return storage.ErrConflict
			}
			return err
		}
		if err := s.deleteLocalSafely(root, a.Path, a.LocalHash, j); err != nil {
			return err
		}
		_, err := s.db.Exec("DELETE FROM baseline WHERE job=? AND path=?", j.ID, a.Path)
		return err
	default:
		return fmt.Errorf("不支持的同步操作：%s", a.Kind)
	}
}
func ensureRemoteParents(ctx context.Context, b Backend, key string) error {
	parents := []string{}
	for p := path.Dir(key); p != "." && p != ""; p = path.Dir(p) {
		parents = append(parents, p)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		e, err := b.Stat(ctx, parents[i])
		if errors.Is(err, storage.ErrNotFound) {
			if err = b.Mkdir(ctx, parents[i]); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !e.IsDir {
			return storage.ErrConflict
		}
	}
	return nil
}

func localDirectoryEmpty(root *os.Root, key string) (bool, error) {
	info, err := root.Lstat(key)
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, storage.ErrConflict
	}
	entries, err := fs.ReadDir(root.FS(), key)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func rejectLocalSymlinkPath(root *os.Root, key string) error {
	parts := strings.Split(filepath.ToSlash(key), "/")
	prefix := ""
	for i, part := range parts {
		if prefix == "" {
			prefix = part
		} else {
			prefix = path.Join(prefix, part)
		}
		info, err := root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("同步路径包含符号链接，已保留并跳过")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return storage.ErrConflict
		}
	}
	return nil
}

func (s *Service) recordLocalRecovery(root *os.Root, key, want string, j Job) error {
	connection, err := s.connection(j.ConnectionID)
	if err != nil {
		return err
	}
	if !connection.KeepRecovery {
		return nil
	}
	v, err := s.hashLocal(root, key)
	if err != nil || v.Hash != want {
		return storage.ErrConflict
	}
	prefs := s.Preferences()
	if v.Size > prefs.MaxFileBytes || directorySize(filepath.Join(s.dir, "recovery"))+v.Size > prefs.RecoveryBytes {
		return errors.New("恢复副本额度不足，已停止本地替换或删除")
	}
	if free := s.unreservedFreeBytes(); free < 0 || free-v.Size < 256<<20 {
		return errors.New("磁盘空间不足以安全保留本地旧版本")
	}
	src, err := root.Open(key)
	if err != nil {
		return err
	}
	defer src.Close()
	id := ID()
	dataPath := filepath.Join(s.dir, "recovery", id+".data")
	dst, err := os.OpenFile(dataPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(dst, h), io.LimitReader(src, prefs.MaxFileBytes+1))
	if copyErr == nil && (n != v.Size || hex.EncodeToString(h.Sum(nil)) != want) {
		copyErr = storage.ErrConflict
	}
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	closeErr := dst.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(dataPath)
		return copyErr
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	meta, err := json.Marshal(map[string]any{"schema": 1, "sha256": want, "path": key, "entry": storage.Entry{Path: key, Name: path.Base(key), Size: v.Size, ETag: "\"sha256-" + want + "\""}, "created": created})
	if err != nil {
		_ = os.Remove(dataPath)
		return err
	}
	metaPath := filepath.Join(s.dir, "recovery", id+".json")
	metaFile, openErr := os.OpenFile(metaPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if openErr != nil {
		_ = os.Remove(dataPath)
		return openErr
	}
	_, err = metaFile.Write(meta)
	if err == nil {
		err = metaFile.Sync()
	}
	closeErr = metaFile.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncDirectory(filepath.Join(s.dir, "recovery"))
	}
	if err != nil {
		_ = os.Remove(dataPath)
		_ = os.Remove(metaPath)
		return err
	}
	_, err = s.db.Exec("INSERT INTO operations(id,connection,path,kind,state,staging,receipt,created) VALUES(?,?,?,?, 'committed','', '',?)", id, "sync:"+j.ConnectionID, key, "sync-local-backup", created)
	if err != nil {
		_ = os.Remove(dataPath)
		_ = os.Remove(metaPath)
		return err
	}
	return nil
}

func conflictCopyPath(key, side string) string {
	dir, base := path.Dir(key), path.Base(key)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	device, _ := os.Hostname()
	var safe strings.Builder
	for _, r := range device {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			safe.WriteRune(r)
		} else if safe.Len() == 0 || !strings.HasSuffix(safe.String(), "-") {
			safe.WriteByte('-')
		}
	}
	device = strings.Trim(safe.String(), "-")
	if device == "" {
		device = "device"
	}
	if len(device) > 48 {
		device = device[:48]
	}
	name := stem + " (conflict " + side + " " + device + " " + time.Now().UTC().Format("20060102T150405.000000000Z") + ")" + ext
	if dir == "." {
		return name
	}
	return path.Join(dir, name)
}
func preserveAsConflict(root *os.Root, source, original, side string) error {
	for i := 0; i < 5; i++ {
		name := conflictCopyPath(original, side)
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		if err := root.Link(source, name); err == nil {
			return nil
		}
	}
	return errors.New("无法安全创建冲突副本")
}

func preserveQuarantineAsConflict(root *os.Root, quarantine, original, side string) error {
	if err := preserveAsConflict(root, quarantine, original, side); err != nil {
		return errors.Join(storage.ErrConflict, fmt.Errorf("无法保留同步期间发现的本地修改，临时文件 %q 已保留：%w", quarantine, err))
	}
	if err := root.Remove(quarantine); err != nil {
		return errors.Join(storage.ErrConflict, fmt.Errorf("冲突副本已保存，但临时文件 %q 清理失败：%w", quarantine, err))
	}
	return storage.ErrConflict
}

func (s *Service) downloadSafely(ctx context.Context, root *os.Root, b Backend, key string, a Action, jobID string) error {
	r, remote, err := b.Open(ctx, key, a.RemoteETag)
	if err != nil {
		return err
	}
	defer r.Close()
	if remote.IsDir || remote.ETag != a.RemoteETag {
		return storage.ErrConflict
	}
	if remote.Size < 0 {
		// Chunked GET responses can omit Content-Length. Pin the expected size
		// to the same strong version, then retain the byte-count and final Stat
		// checks below before installing any local content.
		current, statErr := b.Stat(ctx, key)
		if statErr != nil {
			return statErr
		}
		if current.IsDir || current.Size < 0 || !strongTag(current.ETag) || current.ETag != a.RemoteETag {
			return storage.ErrConflict
		}
		remote.Size = current.Size
	}
	progress := &syncProgressReader{r: r, progress: func(done int64) { s.updateTransferProgress(jobID, a.Path, a.Kind, done, remote.Size) }}
	spool, err := s.spool(ctx, progress, remote.Size)
	if err != nil {
		return err
	}
	defer func() { spool.Close(); _ = os.Remove(spool.Name()) }()
	parent := path.Dir(a.Path)
	if parent != "." {
		if err = root.MkdirAll(parent, 0700); err != nil {
			return err
		}
	}
	tmp := path.Join(parent, ".tami-download-"+ID())
	out, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), spool)
	if err == nil && n != remote.Size {
		err = errors.New("下载内容长度与远端不一致")
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	downloadHash := hex.EncodeToString(h.Sum(nil))
	latest, err := b.Stat(ctx, key)
	if err != nil {
		return err
	}
	if latest.ETag != a.RemoteETag || latest.Size != remote.Size {
		return storage.ErrConflict
	}

	s.writes.Lock()
	defer s.writes.Unlock()
	oldQuarantine := ""
	current, statErr := root.Lstat(a.Path)
	if a.LocalHash == "" {
		if !errors.Is(statErr, os.ErrNotExist) {
			if statErr == nil {
				return storage.ErrConflict
			}
			return statErr
		}
		if err = root.Link(tmp, a.Path); err != nil {
			return storage.ErrConflict
		}
	} else {
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return storage.ErrConflict
			}
			return statErr
		}
		if !current.Mode().IsRegular() {
			return storage.ErrConflict
		}
		old, hashErr := s.hashLocal(root, a.Path)
		if hashErr != nil || old.Hash != a.LocalHash {
			return storage.ErrConflict
		}
		if err = s.recordLocalRecovery(root, a.Path, a.LocalHash, Job{ID: jobID, ConnectionID: b.ConnectionID}); err != nil {
			return err
		}
		oldQuarantine = path.Join(parent, ".tami-sync-old-"+ID())
		if err = root.Rename(a.Path, oldQuarantine); err != nil {
			return err
		}
		moved, moveErr := s.hashLocal(root, oldQuarantine)
		if moveErr != nil || moved.Hash != a.LocalHash {
			return preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local")
		}
		if err = root.Link(tmp, a.Path); err != nil {
			// Keep the old inode until it has either been restored or preserved as
			// a visible conflict. A concurrently created destination is untouched.
			if _, inspectErr := root.Lstat(a.Path); errors.Is(inspectErr, os.ErrNotExist) {
				if restoreErr := root.Link(oldQuarantine, a.Path); restoreErr != nil {
					return errors.Join(storage.ErrConflict, fmt.Errorf("无法回滚本地文件；旧版本仍保留在 %q：%w", oldQuarantine, restoreErr))
				}
				if removeErr := root.Remove(oldQuarantine); removeErr != nil {
					return errors.Join(storage.ErrConflict, fmt.Errorf("本地文件已回滚，但临时文件 %q 清理失败：%w", oldQuarantine, removeErr))
				}
			} else if inspectErr == nil {
				return preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local")
			} else {
				return errors.Join(storage.ErrConflict, fmt.Errorf("无法检查并回滚本地文件；旧版本仍保留在 %q：%w", oldQuarantine, inspectErr))
			}
			return storage.ErrConflict
		}
	}
	installed, err := s.hashLocal(root, a.Path)
	if err != nil {
		if oldQuarantine != "" {
			return errors.Join(err, preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local"))
		}
		return err
	}
	if installed.Hash != downloadHash {
		if oldQuarantine != "" {
			return preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local")
		}
		return storage.ErrConflict
	}
	latest, err = b.Stat(ctx, key)
	if err != nil {
		if oldQuarantine != "" {
			return errors.Join(err, preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local"))
		}
		return err
	}
	if latest.ETag != a.RemoteETag {
		if oldQuarantine != "" {
			return preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local")
		}
		return storage.ErrConflict
	}
	if oldQuarantine != "" {
		moved, moveErr := s.hashLocal(root, oldQuarantine)
		if moveErr != nil || moved.Hash != a.LocalHash {
			return preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local")
		}
	}
	if err = s.saveBaseline(jobID, a.Path, downloadHash, latest.ETag); err != nil {
		if oldQuarantine != "" {
			return errors.Join(err, preserveQuarantineAsConflict(root, oldQuarantine, a.Path, "local"))
		}
		return err
	}
	if oldQuarantine != "" {
		if err = root.Remove(oldQuarantine); err != nil {
			return fmt.Errorf("已完成下载，但临时旧文件 %q 清理失败：%w", oldQuarantine, err)
		}
	}
	return nil
}

func (s *Service) deleteLocalSafely(root *os.Root, key, want string, j Job) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	v, err := s.hashLocal(root, key)
	if err != nil || v.Hash != want {
		return storage.ErrConflict
	}
	if err = s.recordLocalRecovery(root, key, want, j); err != nil {
		return err
	}
	quarantine := path.Join(path.Dir(key), ".tami-sync-delete-"+ID())
	if err = root.Rename(key, quarantine); err != nil {
		return err
	}
	moved, err := s.hashLocal(root, quarantine)
	if err != nil || moved.Hash != want {
		return preserveQuarantineAsConflict(root, quarantine, key, "local")
	}
	latest, err := s.hashLocal(root, quarantine)
	if err != nil || latest.Hash != want {
		return preserveQuarantineAsConflict(root, quarantine, key, "local")
	}
	if err = root.Remove(quarantine); err != nil {
		return err
	}
	if _, err = root.Lstat(key); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return storage.ErrConflict
		}
		return err
	}
	return nil
}
func (s *Service) finishJob(id string, runErr error, counts syncRunCounts) (Job, error) {
	s.mu.Lock()
	var j Job
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == id {
			x := &s.cfg.Jobs[i]
			x.ActiveAction = ""
			x.LastRun = time.Now().Format(time.RFC3339)
			x.Status = "synced"
			x.Detail = syncCompletionDetail(x.QueueTotal, counts)
			if runErr != nil {
				if errors.Is(runErr, context.Canceled) || !x.Enabled {
					x.Status = "paused"
					x.Detail = fmt.Sprintf("已暂停，本轮完成 %d 个操作；重新预览后继续", counts.Completed)
				} else {
					x.Status = "error"
					x.Detail = runErr.Error()
				}
			}
			if !x.Enabled && x.Status != "paused" {
				x.Status = "paused"
				x.Detail = fmt.Sprintf("已暂停，本轮完成 %d 个操作；继续前请重新预览", counts.Completed)
			}
			if x.Status == "synced" {
				x.Progress = 100
				x.QueueDone = x.QueueTotal
			}
			j = *x
			break
		}
	}
	saveErr := s.saveLocked()
	s.mu.Unlock()
	status := "success"
	if runErr != nil {
		status = "error"
	}
	s.activity("sync", status, j.Detail, j.Name)
	if runErr != nil {
		return j, runErr
	}
	return j, saveErr
}
