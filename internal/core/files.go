package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"time"
)

type Backend struct {
	Service      *Service
	ConnectionID string
}

// operationReceipt keeps enough durable evidence to reconcile a remote write
// whose response was lost. It intentionally lives in the existing receipt
// column so older state databases do not need a schema migration.
type operationReceipt struct {
	Version               int            `json:"version"`
	Kind                  string         `json:"kind"`
	SourceConnection      string         `json:"sourceConnection,omitempty"`
	SourcePath            string         `json:"sourcePath,omitempty"`
	Source                storage.Entry  `json:"source,omitempty"`
	SourceHash            string         `json:"sourceHash,omitempty"`
	DestinationConnection string         `json:"destinationConnection,omitempty"`
	DestinationPath       string         `json:"destinationPath,omitempty"`
	DestinationBefore     *storage.Entry `json:"destinationBefore,omitempty"`
	DestinationAbsent     bool           `json:"destinationAbsent,omitempty"`
	DestinationHash       string         `json:"destinationHash,omitempty"`
	DestinationSize       int64          `json:"destinationSize,omitempty"`
	Destination           storage.Entry  `json:"destination,omitempty"`
	Step                  string         `json:"step,omitempty"`
	Error                 string         `json:"error,omitempty"`
}

func (s *Service) recordOperation(id string, evidence operationReceipt) error {
	evidence.Version = 1
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE operations SET receipt=? WHERE id=?", string(raw), id)
	return err
}

func (s *Service) operationEvidence(id string) (operationReceipt, error) {
	var raw string
	if err := s.db.QueryRow("SELECT receipt FROM operations WHERE id=?", id).Scan(&raw); err != nil {
		return operationReceipt{}, err
	}
	var evidence operationReceipt
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		return operationReceipt{}, err
	}
	return evidence, nil
}

func (b Backend) List(ctx context.Context, key string) ([]storage.Entry, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return nil, err
	}
	return st.List(ctx, key)
}
func (b Backend) Stat(ctx context.Context, key string) (storage.Entry, error) {
	if err := validKey(key); err != nil {
		return storage.Entry{}, err
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return storage.Entry{}, err
	}
	return st.Stat(ctx, key)
}
func (b Backend) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	if err := validKey(key); err != nil {
		return nil, storage.Entry{}, err
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return nil, storage.Entry{}, err
	}
	release, err := b.Service.acquireReader(ctx)
	if err != nil {
		return nil, storage.Entry{}, err
	}
	r, e, err := st.Open(ctx, key, etag)
	if err != nil {
		release()
		b.Service.recordStats("connection", b.ConnectionID, 0, 0, true)
		return nil, e, err
	}
	return &observedReader{ctx: ctx, reader: b.Service.limitReader(ctx, r), source: r, release: release, service: b.Service, connection: b.ConnectionID}, e, nil
}

func strongTag(e string) bool {
	return len(e) >= 2 && strings.HasPrefix(e, "\"") && strings.HasSuffix(e, "\"") && !strings.ContainsAny(e[1:len(e)-1], "\"\r\n")
}
func validateParents(ctx context.Context, st storage.Store, key string) error {
	for parent := path.Dir(key); parent != "." && parent != ""; parent = path.Dir(parent) {
		e, err := st.Stat(ctx, parent)
		if errors.Is(err, storage.ErrNotFound) {
			return errors.New("目标父目录不存在，请先创建目录")
		}
		if err != nil {
			return err
		}
		if !e.IsDir {
			return errors.New("目标父路径是文件，不能创建同名目录")
		}
	}
	return nil
}
func resource(c Connection, key string) (string, string) {
	u, _ := url.Parse(c.Endpoint)
	ns := c.Kind + ":" + strings.ToLower(u.Scheme+"://"+u.Host) + ":" + c.Bucket
	if c.Kind == "demo" {
		ns = c.ID
	}
	return ns, path.Join(u.Path, c.Prefix, key)
}
func (s *Service) writable(id, key string, deleting bool) (storage.Store, Connection, error) {
	if err := validKey(key); err != nil {
		return nil, Connection{}, err
	}
	if key == "" {
		return nil, Connection{}, errors.New("不能修改存储根目录")
	}
	c, err := s.connection(id)
	if err != nil {
		return nil, c, err
	}
	if !c.Capabilities.ConditionalWrite || (deleting && !c.Capabilities.ConditionalDelete) {
		return nil, c, errors.New("请先在设置中验证条件写入能力；删除还需条件删除支持")
	}
	ns, k := resource(c, key)
	var count int
	err = s.db.QueryRow(`SELECT count(*) FROM operations o
	 WHERE o.state IN ('committing','uncertain')
	 AND (
	   (o.connection=? AND (o.path=? OR substr(o.path,1,length(?)+1)=?||'/' OR substr(?,1,length(o.path)+1)=o.path||'/'))
	   OR EXISTS (
	     SELECT 1 FROM operation_resources r
	     WHERE r.operation_id=o.id AND r.connection=?
	       AND (r.path=? OR substr(r.path,1,length(?)+1)=?||'/' OR substr(?,1,length(r.path)+1)=r.path||'/')
	   )
	 )`, ns, k, k, k, k, ns, k, k, k, k).Scan(&count)
	if err != nil {
		return nil, c, err
	}
	if count > 0 {
		return nil, c, errors.New("该资源有结果待核对的操作，已暂停写入；请查看活动与操作日志")
	}
	st, err := s.store(id)
	return st, c, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func (s *Service) spool(ctx context.Context, r io.Reader, size int64) (*os.File, error) {
	prefs := s.Preferences()
	if size > prefs.MaxFileBytes {
		return nil, fmt.Errorf("单文件超过设置上限 %d MiB", prefs.MaxFileBytes>>20)
	}
	limit := prefs.MaxFileBytes
	if size >= 0 {
		limit = size
	}

	f, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "upload-*")
	if err != nil {
		return nil, err
	}
	writer, release, err := s.reserveStaging(f, limit)
	if err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	defer release()
	n, err := io.Copy(writer, io.LimitReader(s.limitReader(ctx, r), limit+1))
	if err == nil && n > limit {
		err = errors.New("传输超过暂存额度")
	}
	if err == nil && size >= 0 && n != size {
		err = errors.New("传输内容不完整")
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		_, err = f.Seek(0, 0)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}
func (s *Service) backup(ctx context.Context, st storage.Store, connectionID, key string, old storage.Entry, op string) error {
	prefs := s.Preferences()
	if old.Size > prefs.MaxFileBytes {
		return errors.New("旧文件超过单文件上限，已停止覆盖")
	}
	if directorySize(filepath.Join(s.dir, "recovery"))+old.Size > prefs.RecoveryBytes {
		return errors.New("恢复副本额度已满，请先检查恢复目录")
	}
	if remaining := s.unreservedFreeBytes(); remaining < 0 || remaining-old.Size < 256<<20 {
		return errors.New("磁盘空间不足以安全保留旧版本")
	}
	r, opened, err := st.Open(ctx, key, old.ETag)
	if err != nil {
		return err
	}
	defer r.Close()
	if opened.ETag != old.ETag || opened.Size != old.Size {
		return storage.ErrConflict
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "recovery", op+".data"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(s.limitReader(ctx, r), prefs.MaxFileBytes+1))
	if err == nil && n != old.Size {
		err = errors.New("恢复副本内容不完整")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	meta, _ := json.Marshal(map[string]any{"schema": 1, "path": key, "entry": old, "connectionId": connectionID, "sha256": hex.EncodeToString(h.Sum(nil)), "created": time.Now().Format(time.RFC3339)})
	mf, err := os.OpenFile(filepath.Join(s.dir, "recovery", op+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = mf.Write(meta)
		if err == nil {
			err = mf.Sync()
		}
		closeErr := mf.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		s.removeBackup(op)
		return err
	}
	return syncDirectory(filepath.Join(s.dir, "recovery"))
}
func (s *Service) begin(c Connection, key, kind, temp, id string) (string, error) {
	return s.beginWithEvidence(c, key, kind, temp, id, operationReceipt{Kind: kind, DestinationPath: key, Step: "prepared"}, map[string]string{c.ID: key})
}
func (s *Service) beginWithEvidence(c Connection, key, kind, temp, id string, evidence operationReceipt, resourceKeys ...map[string]string) (string, error) {
	ns, k := resource(c, key)
	evidence.Version = 1
	raw, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	keys := []struct{ ns, key string }{{ns, k}}
	for _, set := range resourceKeys {
		for connectionID, pathKey := range set {
			connection, e := s.connection(connectionID)
			if e != nil {
				return "", e
			}
			resourceNS, resourcePath := resource(connection, pathKey)
			keys = append(keys, struct{ ns, key string }{resourceNS, resourcePath})
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO operations(id,connection,path,kind,state,staging,receipt,created) VALUES(?,?,?,?, 'committing',?,?,?)", id, ns, k, kind, temp, string(raw), time.Now().Format(time.RFC3339)); err != nil {
		return "", err
	}
	for _, item := range keys {
		if _, err = tx.Exec("INSERT OR IGNORE INTO operation_resources(operation_id,connection,path) VALUES(?,?,?)", id, item.ns, item.key); err != nil {
			return "", err
		}
	}

	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}
func (s *Service) finish(id, kind, key string, receipt storage.Entry, opErr error) error {
	state := "committed"
	if opErr != nil {
		state = "uncertain"
		if errors.Is(opErr, storage.ErrConflict) || errors.Is(opErr, storage.ErrNotFound) || errors.Is(opErr, storage.ErrUnsupported) || errors.Is(opErr, storage.ErrLocked) {
			state = "failed"
		}
	}
	evidence, evidenceErr := s.operationEvidence(id)
	if evidenceErr != nil {
		// Preserve old receipts if a legacy or partially initialized operation is
		// being completed. The state still records whether reconciliation is due.
		evidence = operationReceipt{Version: 1, Kind: kind}
	}
	evidence.Destination = receipt
	if opErr != nil {
		evidence.Error = safeOperationError(opErr)
	}
	if err := s.finalizeOperation(id, state, evidence); err != nil {
		s.activity(kind, "error", "远端结果未能记账，资源已暂停写入", key)
		return errors.New("无法记录提交结果，操作需要核对")
	}
	if opErr != nil {
		if state == "failed" && evidence.Step != "destination-verified" && evidence.Step != "source-delete-unknown" {
			s.removeBackup(id)
		}
		msg := "操作未完成（" + state + "），请核对远端结果"
		if state == "failed" {
			msg = "操作未完成，远端结果已确认未提交"
		}
		s.activity(kind, "error", msg, key)
		return opErr
	}
	s.activity(kind, "success", "文件操作完成", key)
	return nil
}

func safeOperationError(err error) string {
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "条件冲突"
	case errors.Is(err, storage.ErrNotFound):
		return "资源不存在"
	case errors.Is(err, storage.ErrUnsupported):
		return "存储不支持此操作"
	default:
		return "远端结果需要核对"
	}
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func fileSHA256(f *os.File) (string, int64, error) {
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

func hashRemoteContent(ctx context.Context, s *Service, st storage.Store, key, etag string) (string, storage.Entry, error) {
	r, entry, err := st.Open(ctx, key, etag)
	if err != nil {
		return "", storage.Entry{}, err
	}
	h := sha256.New()
	n, readErr := io.Copy(h, s.limitReader(ctx, r))
	closeErr := r.Close()
	if readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		return "", entry, readErr
	}
	if n != entry.Size {
		return "", entry, errors.New("远端读取内容不完整")
	}
	return hex.EncodeToString(h.Sum(nil)), entry, nil
}
func (b Backend) Put(ctx context.Context, key string, r io.Reader, size int64, cond storage.Condition) (storage.Entry, error) {
	return b.put(ctx, key, r, size, cond, "")
}
func (b Backend) put(ctx context.Context, key string, r io.Reader, size int64, cond storage.Condition, expectedHash string) (storage.Entry, error) {
	s := b.Service
	if err := s.checkManagedResource(ctx, b.ConnectionID, key); err != nil {
		return storage.Entry{}, err
	}
	// Receive immutable content before acquiring the shared commit coordinator.
	if _, _, err := s.writable(b.ConnectionID, key, false); err != nil {
		return storage.Entry{}, err
	}
	if err := s.checkDAVLocks(ctx, b.ConnectionID, key); err != nil {
		return storage.Entry{}, err
	}
	f, err := s.spool(ctx, r, size)
	if err != nil {
		return storage.Entry{}, err
	}
	defer f.Close()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(f.Name())
		}
	}()
	contentHash, stagedSize, err := fileSHA256(f)
	if err != nil {
		return storage.Entry{}, err
	}
	if expectedHash != "" {
		if contentHash != expectedHash {
			return storage.Entry{}, storage.ErrConflict
		}
	}
	if err = ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	st, c, err := s.writable(b.ConnectionID, key, false)
	if err != nil {
		return storage.Entry{}, err
	}
	if err = s.checkDAVLocks(ctx, c.ID, key); err != nil {
		return storage.Entry{}, err
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	if err = validateParents(commit, st, key); err != nil {
		return storage.Entry{}, err
	}
	old, statErr := st.Stat(commit, key)
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return storage.Entry{}, statErr
	}
	exists := statErr == nil
	if exists && old.IsDir {
		return storage.Entry{}, errors.New("不能覆盖目录")
	}
	if cond.IfNoneMatch && exists {
		return storage.Entry{}, storage.ErrConflict
	}
	if cond.IfMatch != "" && (!exists || (cond.IfMatch != "*" && cond.IfMatch != old.ETag)) {
		return storage.Entry{}, storage.ErrConflict
	}
	op := ID()
	if exists {
		if !strongTag(old.ETag) {
			return storage.Entry{}, errors.New("远端没有可靠版本标识，已停止覆盖")
		}
		cond = storage.Condition{IfMatch: old.ETag}
		if err = s.backup(commit, st, c.ID, key, old, op); err != nil {
			return storage.Entry{}, err
		}
	} else {
		cond = storage.Condition{IfNoneMatch: true}
	}
	evidence := operationReceipt{Kind: "upload", DestinationConnection: c.ID, DestinationPath: key, DestinationHash: contentHash, DestinationSize: stagedSize, DestinationAbsent: !exists, Step: "prepared"}
	if exists {
		before := old
		evidence.DestinationBefore = &before
	}
	op, err = s.beginWithEvidence(c, key, "upload", f.Name(), op, evidence)
	if err != nil {
		s.removeBackup(op)
		return storage.Entry{}, err
	}
	result, opErr := s.putRemote(commit, c.ID, st, key, f, stagedSize, cond, op)
	if opErr == nil && !strongTag(result.ETag) {
		opErr = errors.New("远端已响应，但未返回可靠的提交版本；结果需要核对")
	}
	err = s.finish(op, "upload", key, result, opErr)
	sent := stagedSize
	if opErr != nil {
		sent = 0
	}
	s.scopedStats(ctx, c.ID, sent, 0, opErr != nil)
	if err != nil && !errors.Is(opErr, storage.ErrConflict) && !errors.Is(opErr, storage.ErrNotFound) && !errors.Is(opErr, storage.ErrUnsupported) {
		remove = false
	}
	return result, err
}
func (b Backend) Delete(ctx context.Context, key string, cond storage.Condition) error {
	s := b.Service
	if err := s.checkManagedResource(ctx, b.ConnectionID, key); err != nil {
		return err
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	st, c, err := s.writable(b.ConnectionID, key, true)
	if err != nil {
		return err
	}
	if err = s.checkDAVLocks(ctx, c.ID, key); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return b.deleteLocked(ctx, st, c, key, cond)
}
func (b Backend) Mkdir(ctx context.Context, key string) error {
	s := b.Service
	if err := s.checkManagedResource(ctx, b.ConnectionID, key); err != nil {
		return err
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	st, c, err := s.writable(b.ConnectionID, key, false)
	if err != nil {
		return err
	}
	if err = s.checkDAVLocks(ctx, c.ID, key); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err = validateParents(commit, st, key); err != nil {
		return err
	}
	if _, statErr := st.Stat(commit, key); statErr == nil {
		return storage.ErrConflict
	} else if !errors.Is(statErr, storage.ErrNotFound) {
		return statErr
	}
	op, err := s.beginWithEvidence(c, key, "mkdir", "", ID(), operationReceipt{Kind: "mkdir", DestinationConnection: c.ID, DestinationPath: key, DestinationAbsent: true, Step: "prepared"})
	if err != nil {
		return err
	}
	err = st.Mkdir(commit, key)
	return s.finish(op, "mkdir", key, storage.Entry{Path: key, IsDir: true}, err)
}
func (b Backend) Download(ctx context.Context, key, dest string) error {
	s := b.Service
	r, entry, err := b.Open(ctx, key, "")
	if err != nil {
		return err
	}
	defer r.Close()
	if entry.IsDir {
		return errors.New("请选择文件")
	}
	f, err := s.spool(ctx, r, entry.Size)
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if err = publishNewFile(dest, f); err != nil {
		return err
	}
	s.activity("download", "success", "已下载到 "+dest, key)
	return nil
}
