package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"tamiops/internal/storage"
	"time"
)

type CacheEntry struct {
	ID            string `json:"id"`
	ConnectionID  string `json:"connectionId"`
	Path          string `json:"path"`
	ETag          string `json:"etag"`
	Hash          string `json:"hash"`
	Size          int64  `json:"size"`
	Pinned        bool   `json:"pinned"`
	Updated       string `json:"updated"`
	LocalPath     string `json:"localPath"`
	Dirty         bool   `json:"dirty"`
	Offline       bool   `json:"offline"`
	RemoteChanged bool   `json:"remoteChanged"`
	Checked       string `json:"checked"`
}

func (s *Service) cachePath(e CacheEntry) string {
	return filepath.Join(s.dir, "cache", e.ID, path.Base(e.Path))
}
func hashPlain(filename string) (string, int64, error) {
	i, err := os.Lstat(filename)
	if err != nil {
		return "", 0, err
	}
	if !i.Mode().IsRegular() {
		return "", 0, errors.New("仅支持普通文件")
	}
	f, err := os.Open(filename)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !os.SameFile(i, fi) {
		return "", 0, storage.ErrConflict
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(filename)
	if err != nil || !os.SameFile(i, after) || i.ModTime() != after.ModTime() || n != i.Size() {
		return "", 0, storage.ErrConflict
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func (s *Service) cacheEntries() ([]CacheEntry, error) {
	rows, err := s.db.Query("SELECT id,connection,path,etag,hash,size,pinned,updated,remote_changed,offline,checked FROM cache_entries ORDER BY updated DESC")
	if err != nil {
		return nil, err
	}
	out := []CacheEntry{}
	for rows.Next() {
		var e CacheEntry
		if err = rows.Scan(&e.ID, &e.ConnectionID, &e.Path, &e.ETag, &e.Hash, &e.Size, &e.Pinned, &e.Updated, &e.RemoteChanged, &e.Offline, &e.Checked); err != nil {
			rows.Close()
			return nil, err
		}
		e.LocalPath = s.cachePath(e)
		out = append(out, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		h, _, err := hashPlain(out[i].LocalPath)
		out[i].Dirty = err != nil || h != out[i].Hash
	}
	return out, nil
}
func (s *Service) CacheEntries() ([]CacheEntry, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	return s.cacheEntries()
}
func (s *Service) pruneCache(required int64, protectedID string) error {
	entries, err := s.cacheEntries()
	if err != nil {
		return err
	}
	limit := s.Preferences().CacheBytes
	used := directorySize(filepath.Join(s.dir, "cache"))
	for i := len(entries) - 1; i >= 0 && used+required > limit; i-- {
		e := entries[i]
		if e.Pinned || e.Dirty || e.ID == protectedID {
			continue
		}
		if _, err = s.retireCache(e); err != nil {
			if errors.Is(err, storage.ErrConflict) {
				continue
			}
			return err
		}
		_ = os.Remove(filepath.Dir(e.LocalPath))
		if _, err = s.db.Exec("DELETE FROM cache_entries WHERE id=?", e.ID); err != nil {
			return err
		}
		used -= e.Size
	}
	if used+required > limit {
		return errors.New("缓存额度不足；固定或已编辑的文件不会自动清理")
	}
	return nil
}
func (s *Service) FetchCache(ctx context.Context, connection, key string, pin, allowOffline bool) (CacheEntry, error) {
	return s.fetchCache(ctx, connection, key, pin, allowOffline, false)
}
func (s *Service) fetchCache(ctx context.Context, connection, key string, pin, allowOffline, refresh bool) (CacheEntry, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if err := validKey(key); err != nil {
		return CacheEntry{}, err
	}
	entries, err := s.cacheEntries()
	if err != nil {
		return CacheEntry{}, err
	}
	var old *CacheEntry
	for i := range entries {
		if entries[i].ConnectionID == connection && entries[i].Path == key {
			old = &entries[i]
			break
		}
	}
	b := Backend{s, connection}
	// Serialize metadata publication with mutations. Downloads themselves do
	// not hold the writer lock; their revision is checked again at publication.
	s.writes.Lock()
	writerHeld := true
	defer func() {
		if writerHeld {
			s.writes.Unlock()
		}
	}()
	remote, err := b.Stat(ctx, key)
	if err != nil {
		if old != nil {
			old.Offline = true
			old.Checked = time.Now().UTC().Format(time.RFC3339)
			if errors.Is(err, storage.ErrNotFound) {
				old.RemoteChanged = true
			}
			_, saveErr := s.db.Exec("UPDATE cache_entries SET remote_changed=CASE WHEN ? THEN 1 ELSE remote_changed END,offline=1,checked=? WHERE id=?", old.RemoteChanged, old.Checked, old.ID)
			if saveErr != nil {
				return CacheEntry{}, saveErr
			}
			if allowOffline {
				if readErr := s.db.QueryRow("SELECT remote_changed FROM cache_entries WHERE id=?", old.ID).Scan(&old.RemoteChanged); readErr != nil {
					return CacheEntry{}, readErr
				}
				return *old, nil
			}
		}
		return CacheEntry{}, err
	}
	if remote.IsDir || remote.Size < 0 || !strongTag(remote.ETag) {
		return CacheEntry{}, errors.New("缓存需要文件及可靠版本标识")
	}
	if old != nil {
		old.Checked = time.Now().UTC().Format(time.RFC3339)
		old.RemoteChanged = old.ETag != remote.ETag
		old.Offline = false
		if _, err = s.db.Exec("UPDATE cache_entries SET remote_changed=?,offline=0,checked=? WHERE id=?", old.RemoteChanged, old.Checked, old.ID); err != nil {
			return CacheEntry{}, err
		}
		if refresh && old.Dirty {
			return CacheEntry{}, errors.New("缓存有未提交修改，请先上传或另存再刷新")
		}
		if old.Dirty || (old.Pinned && !refresh) || old.ETag == remote.ETag {
			old.RemoteChanged = old.ETag != remote.ETag
			if pin {
				old.Pinned = true
				_, err = s.db.Exec("UPDATE cache_entries SET pinned=1 WHERE id=?", old.ID)
			}
			return *old, err
		}
	}
	s.writes.Unlock()
	writerHeld = false
	protectedID := ""
	if old != nil {
		protectedID = old.ID
	}
	if err = s.pruneCache(remote.Size, protectedID); err != nil {
		return CacheEntry{}, err
	}
	r, e, err := b.Open(ctx, key, remote.ETag)
	if err != nil {
		return CacheEntry{}, err
	}
	defer r.Close()
	f, err := s.spool(ctx, r, e.Size)
	if err != nil {
		return CacheEntry{}, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	hash, n, err := hashPlain(f.Name())
	if err != nil {
		return CacheEntry{}, err
	}
	if e.ETag != remote.ETag || n != remote.Size {
		return CacheEntry{}, storage.ErrConflict
	}
	s.writes.Lock()
	writerHeld = true
	current, err := b.Stat(ctx, key)
	if err != nil {
		return CacheEntry{}, err
	}
	if current.ETag != e.ETag || current.Size != n {
		return CacheEntry{}, storage.ErrConflict
	}
	if old != nil {
		pin = pin || old.Pinned
	}
	item := CacheEntry{ID: ID(), ConnectionID: connection, Path: key, ETag: e.ETag, Hash: hash, Size: n, Pinned: pin, Updated: time.Now().UTC().Format(time.RFC3339)}
	item.LocalPath = s.cachePath(item)
	item.Checked = item.Updated
	if err = os.MkdirAll(filepath.Dir(item.LocalPath), 0700); err != nil {
		return CacheEntry{}, err
	}
	if err = publishNewFile(item.LocalPath, f); err != nil {
		return CacheEntry{}, err
	}
	retired := ""
	if old != nil {
		retired, err = s.retireCacheLocked(*old)
		if err != nil {
			_ = os.Remove(item.LocalPath)
			return CacheEntry{}, err
		}
	}
	_, err = s.db.Exec("INSERT INTO cache_entries(id,connection,path,etag,hash,size,pinned,updated,checked) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(connection,path) DO UPDATE SET id=excluded.id,etag=excluded.etag,hash=excluded.hash,size=excluded.size,pinned=excluded.pinned,updated=excluded.updated,remote_changed=0,offline=0,checked=excluded.updated", item.ID, connection, key, item.ETag, hash, n, pin, item.Updated, item.Updated)
	if err != nil {
		_ = os.Remove(item.LocalPath)
		if old != nil && retired != "" {
			_ = os.Link(retired, old.LocalPath)
		}
		return CacheEntry{}, err
	}
	if old != nil {
		_ = os.Remove(filepath.Dir(old.LocalPath))
	}
	return item, nil
}
func (s *Service) PinCache(id string, pinned bool) error {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	r, err := s.db.Exec("UPDATE cache_entries SET pinned=? WHERE id=?", pinned, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return storage.ErrNotFound
	}
	return nil
}
func (s *Service) RemoveCache(id string) error {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entries, err := s.cacheEntries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID != id {
			continue
		}
		if e.Dirty {
			return errors.New("本地修改尚未提交，不能清除；请先上传或另存")
		}
		if _, err = s.retireCache(e); err != nil {
			return err
		}
		_ = os.Remove(filepath.Dir(e.LocalPath))
		_, err = s.db.Exec("DELETE FROM cache_entries WHERE id=?", id)
		return err
	}
	return storage.ErrNotFound
}
func (s *Service) UploadCache(ctx context.Context, id string) (storage.Entry, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entries, err := s.cacheEntries()
	if err != nil {
		return storage.Entry{}, err
	}
	for _, e := range entries {
		if e.ID != id {
			continue
		}
		before, err := os.Lstat(e.LocalPath)
		if err != nil || !before.Mode().IsRegular() {
			return storage.Entry{}, errors.New("缓存文件类型无效")
		}
		source, err := os.Open(e.LocalPath)
		if err != nil {
			return storage.Entry{}, err
		}
		defer source.Close()
		info, err := source.Stat()
		if err != nil {
			return storage.Entry{}, err
		}
		if !info.Mode().IsRegular() || !os.SameFile(before, info) {
			return storage.Entry{}, storage.ErrInvalidPath
		}
		staged, err := s.spool(ctx, source, info.Size())
		if err != nil {
			return storage.Entry{}, err
		}
		defer func() { staged.Close(); os.Remove(staged.Name()) }()
		after, statErr := os.Lstat(e.LocalPath)
		if statErr != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() || before.Size() != after.Size() {
			return storage.Entry{}, storage.ErrConflict
		}
		hash, n, err := hashPlain(staged.Name())
		if err != nil {
			return storage.Entry{}, err
		}
		if _, err = staged.Seek(0, io.SeekStart); err != nil {
			return storage.Entry{}, err
		}
		remote, err := (Backend{s, e.ConnectionID}).Put(ctx, e.Path, staged, n, storage.Condition{IfMatch: e.ETag})
		if err != nil {
			return storage.Entry{}, err
		}
		s.writes.Lock()
		defer s.writes.Unlock()
		current, statErr := (Backend{s, e.ConnectionID}).Stat(ctx, e.Path)
		changed := statErr != nil || current.ETag != remote.ETag
		now := time.Now().UTC().Format(time.RFC3339)
		_, err = s.db.Exec("UPDATE cache_entries SET etag=?,hash=?,size=?,updated=?,remote_changed=?,offline=?,checked=? WHERE id=?", remote.ETag, hash, n, now, changed, statErr != nil, now, id)
		return remote, err
	}
	return storage.Entry{}, storage.ErrNotFound
}
