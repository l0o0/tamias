package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"tamiops/internal/storage"
	"time"
)

// TreePreview is a stable view of the exact files a recursive delete will
// affect. The token becomes stale when any listed path or file version changes.
type TreePreview struct {
	Path        string          `json:"path"`
	Files       []storage.Entry `json:"files"`
	Directories []string        `json:"directories"`
	Token       string          `json:"token"`
}

func (b Backend) Copy(ctx context.Context, src, dst string, overwrite bool) (storage.Entry, error) {
	return b.CopyFrom(ctx, b, src, dst, overwrite)
}

// CopyFrom streams a source file through the bounded local staging area into
// the destination connection. Source and destination must belong to one Core
// so the shared writer can serialize aliases and parent/child operations.
func (b Backend) CopyFrom(ctx context.Context, source Backend, src, dst string, overwrite bool) (storage.Entry, error) {
	if b.Service == nil || source.Service != b.Service {
		return storage.Entry{}, errors.New("源和目标必须由同一个 Core 管理")
	}
	s := b.Service
	s.writes.Lock()
	defer s.writes.Unlock()
	return b.copyOrMoveLocked(ctx, source, src, dst, overwrite, false)
}

func (b Backend) Move(ctx context.Context, src, dst string, overwrite bool) (storage.Entry, error) {
	return b.MoveFrom(ctx, b, src, dst, overwrite)
}

// MoveFrom copies and verifies each file before conditionally deleting its
// source. A directory move is a sequence of independently recoverable entries.
func (b Backend) MoveFrom(ctx context.Context, source Backend, src, dst string, overwrite bool) (storage.Entry, error) {
	if b.Service == nil || source.Service != b.Service {
		return storage.Entry{}, errors.New("源和目标必须由同一个 Core 管理")
	}
	s := b.Service
	s.writes.Lock()
	defer s.writes.Unlock()
	return b.copyOrMoveLocked(ctx, source, src, dst, overwrite, true)
}

// Rename is the file-browser spelling of a same-connection Move.
func (b Backend) Rename(ctx context.Context, src, dst string, overwrite bool) (storage.Entry, error) {
	return b.Move(ctx, src, dst, overwrite)
}

func (b Backend) copyOrMoveLocked(ctx context.Context, source Backend, src, dst string, overwrite, move bool) (storage.Entry, error) {
	if err := b.Service.checkManagedResource(ctx, b.ConnectionID, dst); err != nil {
		return storage.Entry{}, err
	}
	if err := b.Service.checkManagedResource(ctx, source.ConnectionID, src); err != nil {
		return storage.Entry{}, err
	}
	if err := validKey(src); err != nil {
		return storage.Entry{}, err
	}
	if err := validKey(dst); err != nil {
		return storage.Entry{}, err
	}
	if src == "" || dst == "" {
		return storage.Entry{}, errors.New("不能复制或移动存储根目录")
	}
	sourceConfig, e := b.Service.connection(source.ConnectionID)
	if e != nil {
		return storage.Entry{}, e
	}
	targetConfig, e := b.Service.connection(b.ConnectionID)
	if e != nil {
		return storage.Entry{}, e
	}
	sourceNamespace, sourceKey := resource(sourceConfig, src)
	targetNamespace, targetKey := resource(targetConfig, dst)
	if sourceNamespace == targetNamespace && sourceKey == targetKey {
		return storage.Entry{}, storage.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	from, err := b.Service.store(source.ConnectionID)
	if err != nil {
		return storage.Entry{}, err
	}
	to, targetConn, err := b.Service.writable(b.ConnectionID, dst, false)
	if err != nil {
		return storage.Entry{}, err
	}
	var sourceConn Connection
	if move {
		from, sourceConn, err = b.Service.writable(source.ConnectionID, src, true)
	} else {
		sourceConn, err = b.Service.connection(source.ConnectionID)
	}
	if err != nil {
		return storage.Entry{}, err
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	if err = validateParents(commit, to, dst); err != nil {
		return storage.Entry{}, err
	}
	sourceEntry, err := from.Stat(commit, src)
	if err != nil {
		return storage.Entry{}, err
	}
	if sourceEntry.IsDir {
		if sourceNamespace == targetNamespace && pathOverlaps(sourceKey, targetKey) {
			return storage.Entry{}, errors.New("不能将目录复制或移动到自身的子目录或父目录")
		}
		preview, err := buildTreePreview(commit, from, src)
		if err != nil {
			return storage.Entry{}, err
		}
		if err = b.Service.checkManagedTree(ctx, source.ConnectionID, preview); err != nil {
			return storage.Entry{}, err
		}
		if err = makeTreeDirectories(commit, b.Service, targetConn, to, src, dst, preview.Directories, overwrite); err != nil {
			return storage.Entry{}, err
		}
		var last storage.Entry
		for _, entry := range preview.Files {
			if err = ctx.Err(); err != nil {
				return last, err
			}
			destKey := treeDestination(src, dst, entry.Path)
			last, err = b.copyFileLocked(commit, from, sourceConn, to, targetConn, entry.Path, destKey, overwrite, move, entry)
			if err != nil {
				return last, fmt.Errorf("%w: 目录操作在 %s 处部分完成", storage.ErrPartialOperation, path.Base(entry.Path))
			}
		}
		if move {
			for i := len(preview.Directories) - 1; i >= 0; i-- {
				if err = deleteEmptyDirectory(commit, b.Service, sourceConn.ID, from, preview.Directories[i]); err != nil {
					return last, fmt.Errorf("文件已核验，但源目录未能清理：%w", err)
				}
			}
			if err = deleteEmptyDirectory(commit, b.Service, sourceConn.ID, from, src); err != nil {
				return last, fmt.Errorf("文件已核验，但源目录未能清理：%w", err)
			}
		}
		return storage.Entry{Path: dst, Name: path.Base(dst), IsDir: true}, nil
	}
	return b.copyFileLocked(commit, from, sourceConn, to, targetConn, src, dst, overwrite, move, sourceEntry)
}

func (b Backend) copyFileLocked(ctx context.Context, from storage.Store, sourceConn Connection, to storage.Store, targetConn Connection, src, dst string, overwrite, move bool, plannedSource storage.Entry) (storage.Entry, error) {
	if err := validKey(src); err != nil {
		return storage.Entry{}, err
	}
	if err := validKey(dst); err != nil {
		return storage.Entry{}, err
	}
	if plannedSource.IsDir || !strongTag(plannedSource.ETag) {
		return storage.Entry{}, errors.New("源文件没有可验证的版本标识")
	}
	if err := b.Service.checkDAVLocks(ctx, sourceConn.ID, src); err != nil {
		return storage.Entry{}, err
	}
	if err := b.Service.checkDAVLocks(ctx, targetConn.ID, dst); err != nil {
		return storage.Entry{}, err
	}
	currentSource, err := from.Stat(ctx, src)
	if err != nil {
		return storage.Entry{}, err
	}
	if currentSource.IsDir || currentSource.ETag != plannedSource.ETag || currentSource.Size != plannedSource.Size {
		return storage.Entry{}, storage.ErrConflict
	}
	r, opened, err := from.Open(ctx, src, plannedSource.ETag)
	if err != nil {
		return storage.Entry{}, err
	}
	if opened.ETag != plannedSource.ETag || opened.Size != plannedSource.Size {
		r.Close()
		return storage.Entry{}, storage.ErrConflict
	}
	f, err := b.Service.spool(ctx, r, plannedSource.Size)
	closeErr := r.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return storage.Entry{}, err
	}
	defer f.Close()
	keepStage := false
	defer func() {
		if !keepStage {
			osRemoveStage(f.Name())
		}
	}()
	contentHash, size, err := fileSHA256(f)
	if err != nil {
		return storage.Entry{}, err
	}
	if size != plannedSource.Size {
		return storage.Entry{}, errors.New("暂存内容与源文件大小不符")
	}
	currentSource, err = from.Stat(ctx, src)
	if err != nil || currentSource.ETag != plannedSource.ETag || currentSource.Size != plannedSource.Size {
		if err != nil {
			return storage.Entry{}, err
		}
		return storage.Entry{}, storage.ErrConflict
	}
	if move && (!sourceConn.Capabilities.ConditionalDelete || !strongTag(plannedSource.ETag)) {
		return storage.Entry{}, errors.New("源存储不支持安全条件删除，已保留源文件")
	}
	targetBefore, statErr := to.Stat(ctx, dst)
	targetExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return storage.Entry{}, statErr
	}
	if targetExists {
		if targetBefore.IsDir {
			return storage.Entry{}, errors.New("目标路径是目录")
		}
		if !overwrite {
			return storage.Entry{}, storage.ErrConflict
		}
		if !strongTag(targetBefore.ETag) {
			return storage.Entry{}, errors.New("目标没有可靠版本标识，已停止覆盖")
		}
	}
	op := ID()
	if targetExists {
		if err = b.Service.backup(ctx, to, targetConn.ID, dst, targetBefore, op); err != nil {
			return storage.Entry{}, err
		}
	}
	evidence := operationReceipt{
		Kind:                  map[bool]string{true: "move", false: "copy"}[move],
		SourceConnection:      sourceConn.ID,
		SourcePath:            src,
		Source:                plannedSource,
		SourceHash:            contentHash,
		DestinationConnection: targetConn.ID,
		DestinationPath:       dst,
		DestinationAbsent:     !targetExists,
		DestinationHash:       contentHash,
		DestinationSize:       size,
		Step:                  "prepared",
	}
	if targetExists {
		before := targetBefore
		evidence.DestinationBefore = &before
	}
	if _, err = b.Service.beginWithEvidence(targetConn, dst, evidence.Kind, f.Name(), op, evidence, map[string]string{sourceConn.ID: src}); err != nil {
		b.Service.removeBackup(op)
		return storage.Entry{}, err
	}
	keepStage = true
	condition := storage.Condition{IfNoneMatch: true}
	if targetExists {
		condition = storage.Condition{IfMatch: targetBefore.ETag}
	}
	result, putErr := to.Put(ctx, dst, f, size, condition)
	if putErr != nil {
		finishErr := b.Service.finish(op, evidence.Kind, dst, result, putErr)
		if isDefiniteNoCommit(putErr) && finishErr == putErr {
			keepStage = false
		}
		return result, finishErr
	}
	if !strongTag(result.ETag) {
		putErr = errors.New("目标已响应，但没有可靠版本标识")
		_ = b.Service.finish(op, evidence.Kind, dst, result, putErr)
		return result, putErr
	}
	actualHash, actual, verifyErr := hashRemoteContent(ctx, b.Service, to, dst, result.ETag)
	if verifyErr != nil || actualHash != contentHash || actual.Size != size {
		if verifyErr == nil {
			verifyErr = errors.New("目标内容与源文件不一致")
		}
		_ = b.Service.finish(op, evidence.Kind, dst, result, verifyErr)
		return result, errors.New("目标内容尚未通过核验，源文件已保留")
	}
	evidence.Destination = actual
	evidence.Step = "destination-verified"
	if err = b.Service.recordOperation(op, evidence); err != nil {
		return result, errors.New("目标已核验，但回执未能持久化；操作需要核对")
	}
	if move {
		latest, statErr := from.Stat(ctx, src)
		if statErr != nil || latest.ETag != plannedSource.ETag || latest.Size != plannedSource.Size {
			if statErr == nil {
				statErr = storage.ErrConflict
			}
			if b.Service.finishPartial(op, evidence.Kind, src, result, statErr) == nil {
				keepStage = false
			}
			return result, fmt.Errorf("%w: 目标已复制并核验，源文件已变化并予以保留", storage.ErrPartialOperation)
		}
		if err = from.Delete(ctx, src, storage.Condition{IfMatch: plannedSource.ETag}); err != nil {
			if isDefiniteNoCommit(err) {
				if b.Service.finishPartial(op, evidence.Kind, src, result, err) == nil {
					keepStage = false
				}
				return result, fmt.Errorf("%w: 目标已复制并核验，源文件保留；请检查后重试", storage.ErrPartialOperation)
			}
			evidence.Step = "source-delete-unknown"
			_ = b.Service.recordOperation(op, evidence)
			_ = b.Service.finish(op, evidence.Kind, src, result, err)
			return result, fmt.Errorf("%w: 目标已复制并核验，源删除结果需要核对", storage.ErrPartialOperation)
		}
		evidence.Step = "source-deleted"
		if err = b.Service.recordOperation(op, evidence); err != nil {
			return result, fmt.Errorf("%w: 移动已完成，但回执未能持久化；操作需要核对", storage.ErrPartialOperation)
		}
	}
	if err = b.Service.finish(op, evidence.Kind, dst, result, nil); err != nil {
		return result, err
	}
	keepStage = false
	return result, nil
}

func isDefiniteNoCommit(err error) bool {
	return errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrUnsupported) || errors.Is(err, storage.ErrLocked)
}

func (s *Service) finishPartial(id, kind, key string, receipt storage.Entry, opErr error) error {
	evidence, err := s.operationEvidence(id)
	if err != nil {
		evidence = operationReceipt{Version: 1, Kind: kind}
	}
	evidence.Destination = receipt
	evidence.Error = safeOperationError(opErr)
	if err = s.finalizeOperation(id, "partial", evidence); err != nil {
		return err
	}
	s.activity(kind, "partial", "目标已核验，源文件已保留", key)
	return nil
}

func buildTreePreview(ctx context.Context, st storage.Store, root string) (TreePreview, error) {
	preview := TreePreview{Path: root, Files: []storage.Entry{}, Directories: []string{}}
	pending := []string{root}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return TreePreview{}, err
		}
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		children, err := st.List(ctx, dir)
		if err != nil {
			return TreePreview{}, err
		}
		for _, child := range children {
			if child.IsDir {
				preview.Directories = append(preview.Directories, child.Path)
				pending = append(pending, child.Path)
			} else {
				preview.Files = append(preview.Files, child)
			}
		}
	}
	sort.Slice(preview.Files, func(i, j int) bool { return preview.Files[i].Path < preview.Files[j].Path })
	sort.Strings(preview.Directories)
	h := sha256.New()
	for _, dir := range preview.Directories {
		io.WriteString(h, "d\x00"+dir+"\x00")
	}
	for _, file := range preview.Files {
		io.WriteString(h, "f\x00"+file.Path+"\x00"+file.ETag+"\x00")
	}
	preview.Token = hex.EncodeToString(h.Sum(nil))
	return preview, nil
}

func treeDestination(src, dst, item string) string {
	rel := strings.TrimPrefix(item, src+"/")
	return path.Join(dst, rel)
}

func makeTreeDirectories(ctx context.Context, s *Service, c Connection, st storage.Store, src, dst string, dirs []string, overwrite bool) error {
	targets := []string{dst}
	for _, dir := range dirs {
		targets = append(targets, treeDestination(src, dst, dir))
	}
	sort.Slice(targets, func(i, j int) bool {
		if strings.Count(targets[i], "/") == strings.Count(targets[j], "/") {
			return targets[i] < targets[j]
		}
		return strings.Count(targets[i], "/") < strings.Count(targets[j], "/")
	})
	for _, target := range targets {
		if err := s.checkManagedResource(ctx, c.ID, target); err != nil {
			return err
		}
		if err := s.checkDAVLocks(ctx, c.ID, target); err != nil {
			return err
		}
		entry, err := st.Stat(ctx, target)
		if err == nil {
			if !entry.IsDir || !overwrite {
				return storage.ErrConflict
			}
			continue
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err = validateParents(ctx, st, target); err != nil {
			return err
		}
		op := ID()
		evidence := operationReceipt{Kind: "mkdir", DestinationConnection: c.ID, DestinationPath: target, DestinationAbsent: true, Step: "prepared"}
		if _, err = s.beginWithEvidence(c, target, "mkdir", "", op, evidence); err != nil {
			return err
		}
		err = st.Mkdir(ctx, target)
		if err = s.finish(op, "mkdir", target, storage.Entry{Path: target, IsDir: true}, err); err != nil {
			return err
		}
	}
	return nil
}

func deleteEmptyDirectory(ctx context.Context, s *Service, connectionID string, st storage.Store, key string) error {
	if err := s.checkManagedResource(ctx, connectionID, key); err != nil {
		return err
	}
	if err := s.checkDAVLocks(ctx, connectionID, key); err != nil {
		return err
	}
	children, err := st.List(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(children) != 0 {
		return storage.ErrConflict
	}
	emptyStore, ok := st.(storage.EmptyDirectoryStore)
	if !ok {
		return fmt.Errorf("%w: 存储不支持原子空目录删除，已保留目录", storage.ErrUnsupported)
	}
	c, err := s.connection(connectionID)
	if err != nil {
		return err
	}
	op := ID()
	evidence := operationReceipt{Kind: "rmdir", SourceConnection: connectionID, SourcePath: key, Source: storage.Entry{Path: key, IsDir: true}, Step: "prepared"}
	if _, err = s.beginWithEvidence(c, key, "rmdir", "", op, evidence); err != nil {
		return err
	}
	err = emptyStore.RemoveEmptyDirectory(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		err = nil
	}
	return s.finish(op, "rmdir", key, storage.Entry{Path: key, IsDir: true}, err)
}

func osRemoveStage(name string) {
	_ = os.Remove(name)
}

func (b Backend) PreviewDeleteTree(ctx context.Context, key string) (TreePreview, error) {
	if err := validKey(key); err != nil {
		return TreePreview{}, err
	}
	if key == "" {
		return TreePreview{}, errors.New("不能删除存储根目录")
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return TreePreview{}, err
	}
	entry, err := st.Stat(ctx, key)
	if err != nil {
		return TreePreview{}, err
	}
	if !entry.IsDir {
		return TreePreview{}, errors.New("请选择目录")
	}
	return buildTreePreview(ctx, st, key)
}

// DeleteTree is an explicit recursive delete request. Callers that show a
// preview should use DeleteTreeWithPreview so stale plans cannot delete a new
// or modified child.
func (b Backend) DeleteTree(ctx context.Context, key string) error {
	return b.deleteTree(ctx, key, "")
}

func (b Backend) DeleteTreeWithPreview(ctx context.Context, key, token string) error {
	if token == "" {
		return errors.New("缺少目录删除预览凭据")
	}
	return b.deleteTree(ctx, key, token)
}

func (b Backend) deleteTree(ctx context.Context, key, token string) error {
	if err := validKey(key); err != nil {
		return err
	}
	if key == "" {
		return errors.New("不能删除存储根目录")
	}
	s := b.Service
	s.writes.Lock()
	defer s.writes.Unlock()
	st, c, err := s.writable(b.ConnectionID, key, true)
	if err != nil {
		return err
	}
	if err = s.checkDAVLocks(ctx, c.ID, key); err != nil {
		return err
	}
	entry, err := st.Stat(ctx, key)
	if err != nil {
		return err
	}
	if !entry.IsDir {
		return errors.New("请选择目录")
	}
	preview, err := buildTreePreview(ctx, st, key)
	if err != nil {
		return err
	}
	if err = s.checkManagedTree(ctx, b.ConnectionID, preview); err != nil {
		return err
	}
	if token != "" && token != preview.Token {
		return storage.ErrConflict
	}
	if !c.Capabilities.ConditionalDelete {
		return storage.ErrUnsupported
	}
	for _, file := range preview.Files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = b.deleteLocked(ctx, st, c, file.Path, storage.Condition{IfMatch: file.ETag}); err != nil {
			return fmt.Errorf("%w: 目录删除在 %s 处停止，剩余项目已保留", storage.ErrPartialOperation, path.Base(file.Path))
		}
	}
	for i := len(preview.Directories) - 1; i >= 0; i-- {
		if err = deleteEmptyDirectory(ctx, s, c.ID, st, preview.Directories[i]); err != nil {
			return fmt.Errorf("%w: 文件已处理，新增或未清空的目录已保留", storage.ErrPartialOperation)
		}
	}
	return deleteEmptyDirectory(ctx, s, c.ID, st, key)
}

func (b Backend) deleteLocked(ctx context.Context, st storage.Store, c Connection, key string, cond storage.Condition) error {
	if err := b.Service.checkManagedResource(ctx, c.ID, key); err != nil {
		return err
	}
	if err := b.Service.checkDAVLocks(ctx, c.ID, key); err != nil {
		return err
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	old, err := st.Stat(commit, key)
	if err != nil {
		return err
	}
	if cond.IfNoneMatch || cond.IfMatch == "" || cond.IfMatch != old.ETag {
		return storage.ErrConflict
	}
	if old.IsDir || !strongTag(old.ETag) {
		return storage.ErrUnsupported
	}
	op := ID()
	if err = b.Service.backup(commit, st, c.ID, key, old, op); err != nil {
		return err
	}
	op, err = b.Service.beginWithEvidence(c, key, "delete", "", op, operationReceipt{Kind: "delete", SourceConnection: c.ID, SourcePath: key, Source: old, Step: "prepared"})
	if err != nil {
		b.Service.removeBackup(op)
		return err
	}
	err = st.Delete(commit, key, storage.Condition{IfMatch: old.ETag})
	return b.Service.finish(op, "delete", key, old, err)
}
