package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"time"
)

// Move the original inode into recovery before releasing a cache path. Editors
// holding that inode can still save to it; never silently unlink those bytes.
// Such copies are deliberately unverified and excluded from automatic pruning.
func (s *Service) retireCache(e CacheEntry) (string, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	return s.retireCacheLocked(e)
}

// retireCacheLocked requires the shared writer lock.
func (s *Service) retireCacheLocked(e CacheEntry) (string, error) {
	info, err := os.Lstat(e.LocalPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", storage.ErrConflict
	}
	if directorySize(filepath.Join(s.dir, "recovery"))+info.Size() > s.Preferences().RecoveryBytes {
		return "", errors.New("恢复区额度不足；旧缓存已保留，请先检查恢复区")
	}
	id := ID()
	dest := filepath.Join(s.dir, "recovery", id+".data")
	if err = os.Rename(e.LocalPath, dest); err != nil {
		return "", err
	}
	meta := recoveryMetadata{MutableCache: true, Path: e.Path, Entry: storage.Entry{Path: e.Path, Size: info.Size(), ETag: e.ETag}, Created: time.Now().UTC().Format(time.RFC3339)}
	raw, err := json.Marshal(meta)
	if err == nil {
		err = publishNewFile(filepath.Join(s.dir, "recovery", id+".json"), strings.NewReader(string(raw)))
	}
	if err != nil {
		_ = os.Link(dest, e.LocalPath)
		return dest, err
	}
	if err = syncDirectory(filepath.Dir(e.LocalPath)); err != nil {
		return dest, err
	}
	hash, n, err := hashPlain(dest)
	if err != nil || hash != e.Hash || n != e.Size {
		_ = os.Link(dest, e.LocalPath)
		return dest, errors.Join(storage.ErrConflict, errors.New("缓存正被外部编辑，最新内容已保留在原路径或恢复区"))
	}
	if _, err = os.Lstat(e.LocalPath); !errors.Is(err, os.ErrNotExist) {
		return dest, errors.Join(storage.ErrConflict, errors.New("编辑器重新保存了缓存，已保留新旧两份内容"))
	}
	return dest, nil
}
