package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"time"
)

type TransferStatistic struct {
	Scope      string `json:"scope"`
	ID         string `json:"id"`
	Requests   int64  `json:"requests"`
	Uploaded   int64  `json:"uploaded"`
	Downloaded int64  `json:"downloaded"`
	Errors     int64  `json:"errors"`
	LastAccess string `json:"lastAccess"`
}

func (s *Service) recordStats(scope, id string, up, down int64, failed bool) {
	errorCount := 0
	if failed {
		errorCount = 1
	}
	_, _ = s.db.Exec("INSERT INTO statistics(scope,id,requests,uploaded,downloaded,errors,last_access) VALUES(?,?,1,?,?,?,?) ON CONFLICT(scope,id) DO UPDATE SET requests=requests+1,uploaded=uploaded+excluded.uploaded,downloaded=downloaded+excluded.downloaded,errors=errors+excluded.errors,last_access=excluded.last_access", scope, id, up, down, errorCount, time.Now().UTC().Format(time.RFC3339))
}
func (s *Service) Statistics() ([]TransferStatistic, error) {
	rows, err := s.db.Query("SELECT scope,id,requests,uploaded,downloaded,errors,last_access FROM statistics ORDER BY scope,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TransferStatistic{}
	for rows.Next() {
		var v TransferStatistic
		if err = rows.Scan(&v.Scope, &v.ID, &v.Requests, &v.Uploaded, &v.Downloaded, &v.Errors, &v.LastAccess); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Service) acquireReader(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errors.New("应用正在退出")
	}
	// Never wait while a caller may hold the write coordinator: a streaming caller
	// can itself be waiting to commit. Admission failure is safe to retry.
	if s.activeReaders >= defaults(s.cfg.Preferences).MaxReaders {
		return nil, errors.New("并发读取已达到上限，请稍后重试")
	}
	s.activeReaders++
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.activeReaders--; s.mu.Unlock() }) }, nil
}

type observedReader struct {
	ctx        context.Context
	reader     io.Reader
	source     io.Closer
	release    func()
	service    *Service
	connection string
	bytes      int64
	failed     bool
	once       sync.Once
}

func (r *observedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		r.failed = true
	}
	return n, err
}
func (r *observedReader) rateLimited() {}
func (r *observedReader) Close() error {
	err := r.source.Close()
	r.once.Do(func() {
		r.release()
		r.service.scopedStats(r.ctx, r.connection, 0, r.bytes, r.failed || err != nil)
	})
	return err
}
func (b Backend) OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, storage.Entry, error) {
	if err := validKey(key); err != nil {
		return nil, storage.Entry{}, err
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return nil, storage.Entry{}, err
	}
	ranged, ok := st.(storage.RangeStore)
	if !ok {
		return nil, storage.Entry{}, storage.ErrUnsupported
	}
	release, err := b.Service.acquireReader(ctx)
	if err != nil {
		return nil, storage.Entry{}, err
	}
	r, e, err := ranged.OpenRange(ctx, key, etag, start, length)
	if err != nil {
		release()
		return nil, e, err
	}
	return &observedReader{ctx: ctx, reader: b.Service.limitReader(ctx, r), source: r, release: release, service: b.Service, connection: b.ConnectionID}, e, nil
}
func (b Backend) Versions(ctx context.Context, key string) ([]storage.ObjectVersion, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return nil, err
	}
	v, ok := st.(storage.VersionedStore)
	if !ok {
		return nil, storage.ErrUnsupported
	}
	return v.ListVersions(ctx, key)
}
func (b Backend) SaveVersion(ctx context.Context, key, version, dest string) error {
	if err := validKey(key); err != nil {
		return err
	}
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return err
	}
	v, ok := st.(storage.VersionedStore)
	if !ok {
		return storage.ErrUnsupported
	}
	r, e, err := v.OpenVersion(ctx, key, version)
	if err != nil {
		return err
	}
	defer r.Close()
	s := b.Service
	f, err := s.spool(ctx, r, e.Size)
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	return publishNewFile(filepath.Clean(dest), f)
}
func (b Backend) DAVLocks(ctx context.Context, key string) ([]storage.DAVLock, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	c, err := b.Service.connection(b.ConnectionID)
	if err != nil {
		return nil, err
	}
	ns, k := resource(c, key)
	_, base := resource(c, "")
	rows, err := b.Service.db.Query("SELECT token,path,owner,depth,expires FROM dav_locks WHERE connection=? AND expires>?", ns, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []storage.DAVLock{}
	for rows.Next() {
		var d storage.DAVLock
		var p string
		var expiry int64
		if err = rows.Scan(&d.Token, &p, &d.Owner, &d.DepthInfinity, &expiry); err != nil {
			return nil, err
		}
		if p == k || (d.DepthInfinity && strings.HasPrefix(k, strings.TrimSuffix(p, "/")+"/")) {
			d.Expires = time.Unix(expiry, 0)
			d.RootKey = strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
			result = append(result, d)
		}
	}
	return result, rows.Err()
}

func (b Backend) SupportsRangeRead() bool {
	st, err := b.Service.store(b.ConnectionID)
	if err != nil {
		return false
	}
	_, ok := st.(storage.RangeStore)
	c, err := b.Service.connection(b.ConnectionID)
	return ok && err == nil && c.Capabilities.RangeRead
}

func probeReadRange(ctx context.Context, st storage.Store) bool {
	ranged, ok := st.(storage.RangeStore)
	if !ok {
		return false
	}
	entries, err := st.List(ctx, "")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir && e.Size > 0 && strongTag(e.ETag) {
			r, _, err := ranged.OpenRange(ctx, e.Path, e.ETag, 0, 1)
			if err != nil {
				return false
			}
			data, err := io.ReadAll(io.LimitReader(r, 2))
			closeErr := r.Close()
			return err == nil && closeErr == nil && len(data) == 1
		}
	}
	return false
}
