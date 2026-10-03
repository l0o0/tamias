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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"tamiops/internal/storage"
	"time"
)

type RecoveryEntry struct {
	ID                 string `json:"id"`
	Path               string `json:"path"`
	ConnectionID       string `json:"connectionId,omitempty"`
	ETag               string `json:"etag,omitempty"`
	Size               int64  `json:"size"`
	Created            string `json:"created"`
	State              string `json:"state"`
	CanRestoreToRemote bool   `json:"canRestoreToRemote"`
	Integrity          string `json:"integrity"`
}

type RecoveryPreview struct {
	ID                  string         `json:"id"`
	ConnectionID        string         `json:"connectionId"`
	Path                string         `json:"path"`
	Saved               storage.Entry  `json:"saved"`
	SavedHash           string         `json:"savedHash"`
	Current             *storage.Entry `json:"current,omitempty"`
	CurrentExists       bool           `json:"currentExists"`
	ExpectedCurrentETag string         `json:"expectedCurrentEtag"`
	Token               string         `json:"token"`
}

type OperationStatus struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	SourcePath      string `json:"sourcePath,omitempty"`
	DestinationPath string `json:"destinationPath,omitempty"`
	Created         string `json:"created"`
	Error           string `json:"error,omitempty"`
}

type recoveryMetadata struct {
	MutableCache bool          `json:"mutableCache,omitempty"`
	Schema       int           `json:"schema"`
	Path         string        `json:"path"`
	Entry        storage.Entry `json:"entry"`
	ConnectionID string        `json:"connectionId,omitempty"`
	SHA256       string        `json:"sha256,omitempty"`
	Created      string        `json:"created"`
}

func validRecoveryID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}

func (s *Service) removeBackup(id string) {
	_ = os.Remove(filepath.Join(s.dir, "recovery", id+".data"))
	_ = os.Remove(filepath.Join(s.dir, "recovery", id+".json"))
	_ = syncDirectory(filepath.Join(s.dir, "recovery"))
}

func (s *Service) readRecovery(id string) (recoveryMetadata, *os.File, error) {
	if !validRecoveryID(id) {
		return recoveryMetadata{}, nil, errors.New("无效副本编号")
	}
	dir := filepath.Join(s.dir, "recovery")
	for _, suffix := range []string{".json", ".data"} {
		info, err := os.Lstat(filepath.Join(dir, id+suffix))
		if err != nil {
			return recoveryMetadata{}, nil, err
		}
		if !info.Mode().IsRegular() {
			return recoveryMetadata{}, nil, errors.New("恢复副本文件类型无效")
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return recoveryMetadata{}, nil, err
	}
	var meta recoveryMetadata
	if err = json.Unmarshal(raw, &meta); err != nil {
		return recoveryMetadata{}, nil, err
	}
	if err = validKey(meta.Path); err != nil || meta.Path == "" || meta.Entry.IsDir || meta.Entry.Size < 0 {
		return recoveryMetadata{}, nil, errors.New("恢复副本记录无效")
	}
	if meta.SHA256 == "" && strings.HasPrefix(meta.Entry.ETag, "\"sha256-") {
		candidate := strings.TrimSuffix(strings.TrimPrefix(meta.Entry.ETag, "\"sha256-"), "\"")
		if decoded, e := hex.DecodeString(candidate); e == nil && len(decoded) == 32 {
			meta.SHA256 = candidate
		}
	}
	if meta.Schema >= 1 && len(meta.SHA256) != 64 {
		return recoveryMetadata{}, nil, errors.New("恢复副本缺少校验值")
	}
	f, err := os.Open(filepath.Join(dir, id+".data"))
	if err != nil {
		return recoveryMetadata{}, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return recoveryMetadata{}, nil, err
	}
	if meta.MutableCache {
		meta.Entry.Size = info.Size()
		meta.SHA256 = ""
		meta.Schema = 0
	}
	if info.Size() != meta.Entry.Size {
		f.Close()
		return recoveryMetadata{}, nil, errors.New("恢复副本大小与记录不符")
	}
	return meta, f, nil
}

func hashFile(f *os.File) (string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (s *Service) recoveriesLocked() ([]RecoveryEntry, error) {
	files, err := os.ReadDir(filepath.Join(s.dir, "recovery"))
	if err != nil {
		return nil, err
	}
	entries := []RecoveryEntry{}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		if !validRecoveryID(id) {
			continue
		}
		meta, data, e := s.readRecovery(id)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		info, e := data.Stat()
		data.Close()
		if e != nil {
			return nil, e
		}
		var state string
		if e = s.db.QueryRow("SELECT state FROM operations WHERE id=?", id).Scan(&state); e != nil {
			state = "untracked"
		}
		if meta.MutableCache {
			state = "cache-retired"
		}
		integrity := "sha256"
		if meta.SHA256 == "" {
			integrity = "unverified"
		}
		entries = append(entries, RecoveryEntry{Integrity: integrity, ID: id, Path: meta.Path, ConnectionID: meta.ConnectionID, ETag: meta.Entry.ETag, Size: info.Size(), Created: meta.Created, State: state, CanRestoreToRemote: meta.ConnectionID != ""})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Created > entries[j].Created })
	return entries, nil
}

func (s *Service) recoveries() ([]RecoveryEntry, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	return s.recoveriesLocked()
}

func (s *Service) saveRecovery(id, dest string) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	meta, f, err := s.readRecovery(id)
	if err != nil {
		return err
	}
	defer f.Close()
	hash, size, err := hashFile(f)
	if err != nil {
		return err
	}
	if size != meta.Entry.Size || meta.SHA256 != "" && hash != meta.SHA256 {
		return errors.New("恢复副本校验失败")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staged, err := s.spool(context.Background(), f, size)
	if err != nil {
		return err
	}
	defer func() { staged.Close(); os.Remove(staged.Name()) }()
	stagedHash, _, err := hashFile(staged)
	if err != nil {
		return err
	}
	if stagedHash != hash {
		return storage.ErrConflict
	}
	return publishNewFile(dest, staged)
}

// Publish only a completely written file, without ever removing or replacing a user's destination.
func publishNewFile(dest string, r io.Reader) error {
	root, err := os.OpenRoot(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer root.Close()
	tmp := ".tami-download-" + ID()
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = root.Link(tmp, filepath.Base(dest)); err != nil {
		return errors.New("目标文件已存在，或文件系统不支持安全保存；请选择新的位置")
	}
	return nil
}

func (s *Service) PreviewRecovery(ctx context.Context, id string) (RecoveryPreview, error) {
	meta, f, err := s.readRecovery(id)
	if err != nil {
		return RecoveryPreview{}, err
	}
	hash, size, err := hashFile(f)
	f.Close()
	if err != nil {
		return RecoveryPreview{}, err
	}
	if size != meta.Entry.Size || meta.SHA256 != "" && hash != meta.SHA256 {
		return RecoveryPreview{}, errors.New("恢复副本校验失败")
	}
	if meta.ConnectionID == "" {
		return RecoveryPreview{}, errors.New("旧版恢复记录没有连接信息，只能保存到本地")
	}
	st, err := s.store(meta.ConnectionID)
	if err != nil {
		return RecoveryPreview{}, err
	}
	current, statErr := st.Stat(ctx, meta.Path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return RecoveryPreview{}, statErr
	}
	preview := RecoveryPreview{ID: id, ConnectionID: meta.ConnectionID, Path: meta.Path, Saved: meta.Entry, SavedHash: hash, CurrentExists: exists}
	if exists {
		if current.IsDir {
			return RecoveryPreview{}, errors.New("原路径当前是目录，不能直接恢复文件")
		}
		preview.Current = &current
		preview.ExpectedCurrentETag = current.ETag
	}
	seed := id + "\x00" + meta.Path + "\x00" + hash + "\x00" + preview.ExpectedCurrentETag
	token := sha256.Sum256([]byte(seed))
	preview.Token = hex.EncodeToString(token[:])
	return preview, nil
}

// RestoreRecovery writes an old local copy back to its original remote path.
// expectedETag must be the exact current version shown by PreviewRecovery, or
// empty when that preview showed the path was absent.
func (s *Service) RestoreRecovery(ctx context.Context, id, expectedETag string) (storage.Entry, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	meta, backup, err := s.readRecovery(id)
	if err != nil {
		return storage.Entry{}, err
	}
	defer backup.Close()
	if meta.ConnectionID == "" {
		return storage.Entry{}, errors.New("旧版恢复记录没有连接信息，只能保存到本地")
	}
	st, c, err := s.writable(meta.ConnectionID, meta.Path, false)
	if err != nil {
		return storage.Entry{}, err
	}
	if err = s.checkDAVLocks(ctx, c.ID, meta.Path); err != nil {
		return storage.Entry{}, err
	}
	current, statErr := st.Stat(ctx, meta.Path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return storage.Entry{}, statErr
	}
	if exists {
		if current.IsDir || expectedETag == "" || current.ETag != expectedETag || !strongTag(current.ETag) {
			return storage.Entry{}, storage.ErrConflict
		}
	} else if expectedETag != "" {
		return storage.Entry{}, storage.ErrConflict
	}
	hash, size, err := hashFile(backup)
	if err != nil {
		return storage.Entry{}, err
	}
	if size != meta.Entry.Size || meta.SHA256 != "" && hash != meta.SHA256 {
		return storage.Entry{}, errors.New("恢复副本校验失败")
	}
	if _, err = backup.Seek(0, io.SeekStart); err != nil {
		return storage.Entry{}, err
	}
	stage, err := s.spool(ctx, backup, size)
	if err != nil {
		return storage.Entry{}, err
	}
	defer stage.Close()
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.Remove(stage.Name())
		}
	}()
	op := ID()
	var condition storage.Condition
	if exists {
		if err = s.backup(ctx, st, c.ID, meta.Path, current, op); err != nil {
			return storage.Entry{}, err
		}
		condition = storage.Condition{IfMatch: current.ETag}
	} else {
		condition = storage.Condition{IfNoneMatch: true}
	}
	evidence := operationReceipt{Kind: "restore", DestinationConnection: c.ID, DestinationPath: meta.Path, DestinationHash: hash, DestinationSize: size, DestinationAbsent: !exists, Step: "prepared"}
	if exists {
		before := current
		evidence.DestinationBefore = &before
	}
	if _, err = s.beginWithEvidence(c, meta.Path, "restore", stage.Name(), op, evidence); err != nil {
		s.removeBackup(op)
		return storage.Entry{}, err
	}
	commit, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, putErr := st.Put(commit, meta.Path, stage, size, condition)
	if putErr == nil && !strongTag(result.ETag) {
		putErr = errors.New("远端已响应，但未返回可靠的提交版本；结果需要核对")
	}
	if putErr == nil {
		actualHash, actual, verifyErr := hashRemoteContent(commit, s, st, meta.Path, result.ETag)
		if verifyErr != nil || actualHash != hash || actual.Size != size {
			if verifyErr == nil {
				verifyErr = errors.New("远端恢复内容与副本不一致")
			}
			putErr = verifyErr
		} else {
			evidence.Destination = actual
			evidence.Step = "destination-verified"
			if err = s.recordOperation(op, evidence); err != nil {
				return result, errors.New("恢复已核验，但回执未能持久化；操作需要核对")
			}
		}
	}
	err = s.finish(op, "restore", meta.Path, result, putErr)
	if err != nil && !isDefiniteNoCommit(putErr) {
		removeStage = false
	}
	return result, err
}

func (s *Service) PendingOperations() ([]OperationStatus, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	rows, err := s.db.Query("SELECT id,kind,state,receipt,created FROM operations WHERE state IN ('committing','uncertain','partial') ORDER BY created DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []OperationStatus{}
	for rows.Next() {
		var item OperationStatus
		var raw string
		if err = rows.Scan(&item.ID, &item.Kind, &item.State, &raw, &item.Created); err != nil {
			return nil, err
		}
		var evidence operationReceipt
		if json.Unmarshal([]byte(raw), &evidence) == nil {
			item.SourcePath = evidence.SourcePath
			item.DestinationPath = evidence.DestinationPath
			item.Error = evidence.Error
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Service) ReconcileOperation(ctx context.Context, id string) error {
	if !validRecoveryID(id) {
		return errors.New("无效操作编号")
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	var kind, state, raw string
	if err := s.db.QueryRow("SELECT kind,state,receipt FROM operations WHERE id=?", id).Scan(&kind, &state, &raw); err != nil {
		return err
	}
	if state != "committing" && state != "uncertain" {
		return errors.New("此操作当前不需要核对")
	}
	var evidence operationReceipt
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil || evidence.Version != 1 || evidence.Kind == "" {
		return errors.New("操作缺少可验证回执，仍保持待核对")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	newState, found, err := s.reconcileEvidence(ctx, id, &evidence)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("现有远端内容不足以判定结果，操作仍保持待核对")
	}
	evidence.Step = "reconciled-" + newState
	updated, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var staging string
	if err = tx.QueryRow("SELECT staging FROM operations WHERE id=?", id).Scan(&staging); err != nil {
		return err
	}
	var hasMultipart int
	if kind == "upload" {
		if err = tx.QueryRow("SELECT count(*) FROM multipart_uploads WHERE operation_id=?", id).Scan(&hasMultipart); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE operations SET state=?,receipt=? WHERE id=? AND state IN ('committing','uncertain')", newState, string(updated), id); err != nil {
		return err
	}
	if kind == "upload" {
		multipartState := ""
		if newState == "committed" {
			multipartState = "done"
		} else if newState == "failed" {
			multipartState = "paused"
		}
		if multipartState != "" {
			if _, err = tx.Exec("UPDATE multipart_uploads SET state=? WHERE operation_id=?", multipartState, id); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	keepForMultipartRetry := kind == "upload" && newState == "failed" && hasMultipart > 0
	if !keepForMultipartRetry && (newState == "committed" || newState == "failed" || newState == "partial") && filepath.Dir(staging) == filepath.Join(s.dir, "staging") && strings.HasPrefix(filepath.Base(staging), "upload-") {
		if err = os.Remove(staging); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	message := "远端结果已核对并确认完成"
	status := "success"
	if newState == "failed" {
		message = "远端结果已核对并确认未提交"
		status = "error"
	} else if newState == "partial" {
		message = "目标副本已核验，源文件仍保留"
		status = "partial"
	}
	s.activity(kind, status, message, evidence.DestinationPath)
	return nil
}

func (s *Service) reconcileEvidence(ctx context.Context, operationID string, e *operationReceipt) (string, bool, error) {
	switch e.Kind {
	case "delete":
		st, err := s.store(e.SourceConnection)
		if err != nil {
			return "", false, err
		}
		current, err := st.Stat(ctx, e.SourcePath)
		if errors.Is(err, storage.ErrNotFound) {
			return "committed", true, nil
		}
		if err != nil {
			return "", false, err
		}
		if current.ETag == e.Source.ETag {
			return "failed", true, nil
		}
		return "", false, nil
	case "rmdir":
		st, err := s.store(e.SourceConnection)
		if err != nil {
			return "", false, err
		}
		_, err = st.Stat(ctx, e.SourcePath)
		if errors.Is(err, storage.ErrNotFound) {
			return "committed", true, nil
		}
		if err != nil {
			return "", false, err
		}
		children, err := st.List(ctx, e.SourcePath)
		if err != nil {
			return "", false, err
		}
		if len(children) == 0 {
			return "failed", true, nil
		}
		return "", false, nil
	case "copy", "move", "upload", "restore":
		st, err := s.store(e.DestinationConnection)
		if err != nil {
			return "", false, err
		}
		current, err := st.Stat(ctx, e.DestinationPath)
		if errors.Is(err, storage.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		if e.DestinationBefore != nil && current.ETag == e.DestinationBefore.ETag {
			if same, verifyErr := s.remoteMatchesBackup(ctx, st, e.DestinationPath, current.ETag, operationID); verifyErr != nil {
				return "", false, verifyErr
			} else if same {
				return "failed", true, nil
			}
		}
		if e.DestinationHash == "" || !strongTag(current.ETag) {
			return "", false, nil
		}
		actualHash, actual, err := hashRemoteContent(ctx, s, st, e.DestinationPath, current.ETag)
		if err != nil {
			return "", false, err
		}
		if actual.Size != e.DestinationSize || actualHash != e.DestinationHash {
			return "", false, nil
		}
		e.Destination = actual
		if e.Kind == "move" {
			source, err := s.store(e.SourceConnection)
			if err != nil {
				return "", false, err
			}
			_, err = source.Stat(ctx, e.SourcePath)
			if errors.Is(err, storage.ErrNotFound) {
				return "committed", true, nil
			}
			if err != nil {
				return "", false, err
			}
			return "partial", true, nil
		}
		return "committed", true, nil
	case "mkdir":
		st, err := s.store(e.DestinationConnection)
		if err != nil {
			return "", false, err
		}
		_, err = st.Stat(ctx, e.DestinationPath)
		if errors.Is(err, storage.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		// Existence alone cannot prove this operation created the directory.
		return "", false, nil
	default:
		return "", false, fmt.Errorf("未知操作类型，仍保持待核对")
	}
}

func (s *Service) remoteMatchesBackup(ctx context.Context, st storage.Store, key, etag, operationID string) (bool, error) {
	meta, f, err := s.readRecovery(operationID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	if meta.Entry.ETag != etag {
		return false, nil
	}
	remoteHash, remote, err := hashRemoteContent(ctx, s, st, key, etag)
	if err != nil {
		return false, err
	}
	backupHash, backupSize, err := hashFile(f)
	if err != nil {
		return false, err
	}
	if meta.SHA256 != "" && meta.SHA256 != backupHash {
		return false, errors.New("恢复副本校验失败")
	}
	return remote.Size == backupSize && remoteHash == backupHash, nil
}

func (s *Service) DeleteRecovery(id string) error {
	if !validRecoveryID(id) {
		return errors.New("无效副本编号")
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	var state string
	if err := s.db.QueryRow("SELECT state FROM operations WHERE id=?", id).Scan(&state); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if state == "committing" || state == "uncertain" {
		return errors.New("副本关联的操作仍待核对，暂不能清理")
	}
	if _, data, err := s.readRecovery(id); err != nil {
		return err
	} else if closeErr := data.Close(); closeErr != nil {
		return closeErr
	}
	s.removeBackup(id)
	_, err := os.Stat(filepath.Join(s.dir, "recovery", id+".data"))
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("恢复副本清理失败")
	}
	return nil
}

// PruneRecoveries applies the configured age and byte budgets. Operations
// whose remote outcome is uncertain are always excluded from automatic cleanup.
func (s *Service) PruneRecoveries(now time.Time) ([]string, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	entries, err := s.recoveriesLocked()
	if err != nil {
		return nil, err
	}
	prefs := s.Preferences()
	limit := prefs.RecoveryBytes
	cutoff := now.AddDate(0, 0, -prefs.RecoveryDays)
	total := int64(0)
	for _, entry := range entries {
		total += entry.Size
	}
	removed := []string{}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		created, _ := time.Parse(time.RFC3339, entry.Created)
		if entry.State == "uncertain" || entry.State == "committing" || entry.State == "cache-retired" {
			continue
		}
		if (created.IsZero() || created.After(cutoff)) && total <= limit {
			continue
		}
		if err = os.Remove(filepath.Join(s.dir, "recovery", entry.ID+".data")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		if err = os.Remove(filepath.Join(s.dir, "recovery", entry.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed = append(removed, entry.ID)
		total -= entry.Size
	}
	if len(removed) > 0 {
		if err = syncDirectory(filepath.Join(s.dir, "recovery")); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// Called with the exclusive data-directory lock before accepting work. No remote
// mutation occurs before a durable operation references its staging file.
func (s *Service) reconcileTemporaryFiles() error {
	rows, err := s.db.Query("SELECT id,state,staging FROM operations")
	if err != nil {
		return err
	}
	keepStage := map[string]bool{}
	for rows.Next() {
		var id, state, temp string
		if err = rows.Scan(&id, &state, &temp); err != nil {
			rows.Close()
			return err
		}
		if state == "committing" || state == "uncertain" {
			keepStage[filepath.Clean(temp)] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	downloadRows, err := s.db.Query("SELECT staging FROM downloads WHERE state NOT IN ('done','cancelled')")
	if err != nil {
		return err
	}
	for downloadRows.Next() {
		var temp string
		if err = downloadRows.Scan(&temp); err != nil {
			downloadRows.Close()
			return err
		}
		keepStage[filepath.Clean(temp)] = true
	}
	err = downloadRows.Err()
	downloadRows.Close()
	if err != nil {
		return err
	}
	files, err := os.ReadDir(filepath.Join(s.dir, "staging"))
	if err != nil {
		return err
	}
	for _, f := range files {
		p := filepath.Join(s.dir, "staging", f.Name())
		if !f.IsDir() && strings.HasPrefix(f.Name(), "upload-") && !keepStage[p] {
			if err = os.Remove(p); err != nil {
				return err
			}
		}
	}
	// A local recovery file can precede its database receipt after a crash.
	// Preserve it: absence of a database row is not evidence that its bytes are disposable.
	return nil
}
