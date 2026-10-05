package core

import (
	"context"
	"errors"
	"strings"
	"tamiops/internal/storage"
	"time"
)

// DAV leases coordinate every writer in this process, including aliases of a connection.
// They cannot coordinate clients talking directly to the underlying storage.
func pathOverlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") || strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}
func lockDuration(d time.Duration) time.Duration {
	if d <= 0 || d > time.Hour {
		return time.Hour
	}
	return d
}
func (s *Service) checkDAVLocks(ctx context.Context, connectionID, key string) error {
	c, err := s.connection(connectionID)
	if err != nil {
		return err
	}
	ns, k := resource(c, key)
	rows, err := s.db.Query("SELECT token,path,depth FROM dav_locks WHERE connection=? AND expires>?", ns, time.Now().Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	tokens := storage.LockTokens(ctx)
	for rows.Next() {
		var token, p string
		var depth bool
		if err = rows.Scan(&token, &p, &depth); err != nil {
			return err
		}
		protected := k == p || strings.HasPrefix(p, strings.TrimSuffix(k, "/")+"/") || (depth && strings.HasPrefix(k, strings.TrimSuffix(p, "/")+"/"))
		if !protected {
			continue
		}
		allowed := false
		for _, v := range tokens {
			if v == token {
				allowed = true
			}
		}
		if !allowed {
			return storage.ErrLocked
		}
	}
	return rows.Err()
}
func (b Backend) AcquireDAVLock(ctx context.Context, key, owner string, depth bool, timeout time.Duration) (string, time.Time, error) {
	s := b.Service
	s.writes.Lock()
	defer s.writes.Unlock()
	if err := validKey(key); err != nil {
		return "", time.Time{}, err
	}
	if len(owner) > 1024 {
		return "", time.Time{}, errors.New("锁所有者内容过长")
	}
	c, err := s.connection(b.ConnectionID)
	if err != nil {
		return "", time.Time{}, err
	}
	if !canWriteStrict(c) {
		return "", time.Time{}, errors.New("请先验证写入能力")
	}
	st, err := s.store(b.ConnectionID)
	if err != nil {
		return "", time.Time{}, err
	}
	if _, err = st.Stat(ctx, key); err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return "", time.Time{}, err
		}
		if err = validateParents(ctx, st, key); err != nil {
			return "", time.Time{}, err
		}
	}
	ns, k := resource(c, key)
	rows, err := s.db.Query("SELECT path,depth FROM dav_locks WHERE connection=? AND expires>?", ns, time.Now().Unix())
	if err != nil {
		return "", time.Time{}, err
	}
	for rows.Next() {
		var p string
		var recursive bool
		if err = rows.Scan(&p, &recursive); err != nil {
			rows.Close()
			return "", time.Time{}, err
		}
		if p == k || (recursive && strings.HasPrefix(k, strings.TrimSuffix(p, "/")+"/")) || (depth && strings.HasPrefix(p, strings.TrimSuffix(k, "/")+"/")) {
			rows.Close()
			return "", time.Time{}, storage.ErrLocked
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", time.Time{}, err
	}
	token := "opaquelocktoken:" + ID()
	expires := time.Now().Add(lockDuration(timeout))
	_, err = s.db.Exec("INSERT INTO dav_locks(token,connection,path,owner,depth,expires) VALUES(?,?,?,?,?,?)", token, ns, k, owner, depth, expires.Unix())
	return token, expires, err
}
func (b Backend) RefreshDAVLock(ctx context.Context, key, token string, timeout time.Duration) (time.Time, error) {
	s := b.Service
	s.writes.Lock()
	defer s.writes.Unlock()
	c, err := s.connection(b.ConnectionID)
	if err != nil {
		return time.Time{}, err
	}
	ns, k := resource(c, key)
	expires := time.Now().Add(lockDuration(timeout))
	r, err := s.db.Exec("UPDATE dav_locks SET expires=? WHERE token=? AND connection=? AND path=? AND expires>?", expires.Unix(), token, ns, k, time.Now().Unix())
	if err != nil {
		return time.Time{}, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return time.Time{}, storage.ErrLocked
	}
	return expires, nil
}
func (b Backend) UnlockDAVLock(ctx context.Context, key, token string) error {
	s := b.Service
	s.writes.Lock()
	defer s.writes.Unlock()
	c, err := s.connection(b.ConnectionID)
	if err != nil {
		return err
	}
	ns, k := resource(c, key)
	r, err := s.db.Exec("DELETE FROM dav_locks WHERE token=? AND connection=? AND path=?", token, ns, k)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return storage.ErrLocked
	}
	return nil
}
