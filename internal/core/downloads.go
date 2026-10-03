package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"tamiops/internal/storage"
	"time"
)

type downloadControlKey struct {
	service *Service
	id      string
}

type downloadControl struct {
	refs       int
	active     bool
	cancelling bool
	cancel     context.CancelFunc
	done       chan struct{}
}

var downloadControls = struct {
	sync.Mutex
	byKey map[downloadControlKey]*downloadControl
}{byKey: make(map[downloadControlKey]*downloadControl)}

func retainDownloadControl(s *Service, id string) (*downloadControl, downloadControlKey) {
	key := downloadControlKey{service: s, id: id}
	downloadControls.Lock()
	c := downloadControls.byKey[key]
	if c == nil {
		c = &downloadControl{}
		downloadControls.byKey[key] = c
	}
	c.refs++
	downloadControls.Unlock()
	return c, key
}

func releaseDownloadControl(key downloadControlKey, c *downloadControl) {
	downloadControls.Lock()
	defer downloadControls.Unlock()
	c.refs--
	if c.refs == 0 && !c.active && !c.cancelling && downloadControls.byKey[key] == c {
		delete(downloadControls.byKey, key)
	}
}

// acquireDownloadRun serializes work for one transfer ID without taking the
// service-wide writer lock. The done channel closes only after the transfer's
// files and staging reservation have been released.
func (s *Service) acquireDownloadRun(parent context.Context, id string) (context.Context, func(), error) {
	c, key := retainDownloadControl(s, id)
	downloadControls.Lock()
	if c.cancelling || c.active {
		cancelling := c.cancelling
		downloadControls.Unlock()
		releaseDownloadControl(key, c)
		if cancelling {
			return nil, nil, errors.New("下载正在取消")
		}
		return nil, nil, errors.New("下载正在进行中")
	}
	c.active = true
	c.done = make(chan struct{})
	downloadControls.Unlock()

	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		cancel()
		downloadControls.Lock()
		c.active = false
		close(c.done)
		downloadControls.Unlock()
		releaseDownloadControl(key, c)
		return nil, nil, errors.New("应用正在退出")
	}
	s.tasks.Add(1)
	s.downloadCancels[id] = cancel
	s.mu.Unlock()
	downloadControls.Lock()
	c.cancel = cancel
	cancelNow := c.cancelling
	downloadControls.Unlock()
	if cancelNow {
		cancel()
	}

	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel()
			downloadControls.Lock()
			c.active = false
			c.cancel = nil
			close(c.done)
			downloadControls.Unlock()
			s.mu.Lock()
			delete(s.downloadCancels, id)
			s.mu.Unlock()
			s.tasks.Done()
			releaseDownloadControl(key, c)
		})
	}
	return ctx, finish, nil
}

type DownloadTransfer struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	Path         string `json:"path"`
	ETag         string `json:"etag"`
	Destination  string `json:"destination"`
	Staging      string `json:"-"`
	Size         int64  `json:"size"`
	Received     int64  `json:"received"`
	Hash         string `json:"-"`
	State        string `json:"state"`
	Updated      string `json:"updated"`
}

func (s *Service) Downloads() ([]DownloadTransfer, error) {
	rows, err := s.db.Query("SELECT id,connection,path,etag,destination,staging,size,received,hash,state,updated FROM downloads WHERE state NOT IN ('done','cancelled') ORDER BY updated DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DownloadTransfer{}
	for rows.Next() {
		var d DownloadTransfer
		if err = rows.Scan(&d.ID, &d.ConnectionID, &d.Path, &d.ETag, &d.Destination, &d.Staging, &d.Size, &d.Received, &d.Hash, &d.State, &d.Updated); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Service) saveDownload(d DownloadTransfer) error {
	_, err := s.db.Exec("INSERT INTO downloads(id,connection,path,etag,destination,staging,size,received,hash,state,updated) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET received=excluded.received,hash=excluded.hash,state=excluded.state,updated=excluded.updated", d.ID, d.ConnectionID, d.Path, d.ETag, d.Destination, d.Staging, d.Size, d.Received, d.Hash, d.State, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Service) downloadRecord(id string) (DownloadTransfer, error) {
	var d DownloadTransfer
	err := s.db.QueryRow("SELECT id,connection,path,etag,destination,staging,size,received,hash,state,updated FROM downloads WHERE id=?", id).Scan(&d.ID, &d.ConnectionID, &d.Path, &d.ETag, &d.Destination, &d.Staging, &d.Size, &d.Received, &d.Hash, &d.State, &d.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DownloadTransfer{}, storage.ErrNotFound
	}
	return d, err
}

func (s *Service) StartDownload(ctx context.Context, connection, key, destination string) (DownloadTransfer, error) {
	if err := validKey(key); err != nil {
		return DownloadTransfer{}, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return DownloadTransfer{}, errors.New("保存位置已存在，请选择新文件名")
	}
	b := Backend{s, connection}
	e, err := b.Stat(ctx, key)
	if err != nil {
		return DownloadTransfer{}, err
	}
	if e.IsDir || e.Size < 0 || !strongTag(e.ETag) {
		return DownloadTransfer{}, errors.New("续传下载需要普通文件、大小及可靠版本")
	}
	p := s.Preferences()
	staged, reserved := s.stagingUsage()
	if e.Size > p.MaxFileBytes || staged > p.StagingBytes || e.Size > p.StagingBytes-staged || freeBytes(s.dir)-reserved-e.Size < 256<<20 {
		return DownloadTransfer{}, errors.New("文件超过暂存额度或磁盘可用空间")
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "download-*")
	if err != nil {
		return DownloadTransfer{}, err
	}
	name := f.Name()
	if err = f.Close(); err != nil {
		return DownloadTransfer{}, err
	}
	d := DownloadTransfer{ID: ID(), ConnectionID: connection, Path: key, ETag: e.ETag, Destination: destination, Staging: name, Size: e.Size, State: "pending"}
	d.Hash, _, err = hashPlain(name)
	if err != nil {
		_ = os.Remove(name)
		return d, err
	}
	if err = s.saveDownload(d); err != nil {
		_ = os.Remove(name)
		return d, err
	}
	return s.continueDownload(ctx, d)
}
func (s *Service) ResumeDownload(ctx context.Context, id string) (DownloadTransfer, error) {
	d, err := s.downloadRecord(id)
	if err != nil {
		return DownloadTransfer{}, err
	}
	if d.State == "done" || d.State == "cancelled" {
		return DownloadTransfer{}, storage.ErrNotFound
	}
	return s.continueDownload(ctx, d)
}
func (s *Service) continueDownload(ctx context.Context, d DownloadTransfer) (DownloadTransfer, error) {
	var finish func()
	ctx, finish, err := s.acquireDownloadRun(ctx, d.ID)
	if err != nil {
		return d, err
	}
	defer finish()
	d, err = s.downloadRecord(d.ID)
	if err != nil {
		return d, err
	}
	if d.State == "done" || d.State == "cancelled" {
		return d, storage.ErrNotFound
	}

	if filepath.Dir(d.Staging) != filepath.Join(s.dir, "staging") {
		return d, storage.ErrInvalidPath
	}
	hash, n, err := hashPlain(d.Staging)
	if err != nil || n < d.Received {
		return d, errors.New("续传暂存文件缺失或被截断")
	}
	if n > d.Received {
		if err = os.Truncate(d.Staging, d.Received); err != nil {
			return d, err
		}
		hash, n, err = hashPlain(d.Staging)
	}
	if err != nil || n != d.Received || hash != d.Hash {
		return d, errors.New("续传暂存校验失败")
	}
	b := Backend{s, d.ConnectionID}
	e, err := b.Stat(ctx, d.Path)
	if err != nil {
		return d, err
	}
	if e.ETag != d.ETag || e.Size != d.Size {
		return d, storage.ErrConflict
	}
	remaining := d.Size - d.Received
	p := s.Preferences()
	staged, reserved := s.stagingUsage()
	if d.Size > p.MaxFileBytes || staged > p.StagingBytes || remaining > p.StagingBytes-staged || freeBytes(s.dir)-reserved-remaining < 256<<20 {
		return d, errors.New("磁盘或暂存额度不足，下载保持暂停")
	}
	f, err := os.OpenFile(d.Staging, os.O_RDWR, 0600)
	if err != nil {
		return d, err
	}
	defer f.Close()
	writer, release, err := s.reserveStaging(f, remaining)
	if err != nil {
		return d, err
	}
	reservationReleased := false
	defer func() {
		if !reservationReleased {
			_ = f.Close()
			release()
		}
	}()
	hasher := sha256.New()
	if _, err = io.Copy(hasher, io.NewSectionReader(f, 0, d.Received)); err != nil {
		return d, err
	}
	if hex.EncodeToString(hasher.Sum(nil)) != d.Hash {
		return d, errors.New("续传前缀校验失败")
	}
	if d.Received < d.Size {
		var r io.ReadCloser
		if d.Received > 0 {
			if !b.SupportsRangeRead() {
				return d, errors.New("连接未验证 Range 能力，请取消后重新下载")
			}
			r, _, err = b.OpenRange(ctx, d.Path, d.ETag, d.Received, remaining)
		} else {
			r, _, err = b.Open(ctx, d.Path, d.ETag)
		}
		if err != nil {
			return d, err
		}
		defer r.Close()
		if _, err = f.Seek(d.Received, io.SeekStart); err != nil {
			return d, err
		}
		d.State = "running"
		if err = s.saveDownload(d); err != nil {
			return d, err
		}
		for d.Received < d.Size {
			chunk := int64(4 << 20)
			if d.Size-d.Received < chunk {
				chunk = d.Size - d.Received
			}
			copied, copyErr := io.CopyN(io.MultiWriter(writer, hasher), contextReader{ctx, r}, chunk)
			d.Received += copied
			if err = f.Sync(); err != nil {
				return d, err
			}
			d.Hash = hex.EncodeToString(hasher.Sum(nil))
			if copyErr != nil {
				d.State = "paused"
			}
			if err = s.saveDownload(d); err != nil {
				return d, err
			}
			if copyErr != nil {
				return d, copyErr
			}
		}
		// Reject extra bytes rather than publishing a body inconsistent with metadata.
		extra := make([]byte, 1)
		if n, readErr := r.Read(extra); n != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
			d.State = "paused"
			_ = s.saveDownload(d)
			return d, errors.New("远端响应长度不一致")
		}
	}
	current, err := b.Stat(ctx, d.Path)
	if err != nil {
		return d, err
	}
	if current.ETag != d.ETag {
		return d, storage.ErrConflict
	}
	existingHash, existingSize, existingErr := hashPlain(d.Destination)
	if existingErr == nil && existingHash == d.Hash && existingSize == d.Size {
		d.State = "done"
		if err = s.saveDownload(d); err != nil {
			return d, err
		}
		_ = f.Close()
		release()
		reservationReleased = true
		_ = os.Remove(d.Staging)
		return d, nil
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return d, err
	}
	if err = publishNewFile(d.Destination, f); err != nil {
		d.State = "paused"
		_ = s.saveDownload(d)
		return d, err
	}
	d.State = "done"
	if err = s.saveDownload(d); err != nil {
		return d, err
	}
	_ = f.Close()
	release()
	reservationReleased = true
	_ = os.Remove(d.Staging)
	return d, nil
}
func (s *Service) CancelDownload(id string) error {
	c, key := retainDownloadControl(s, id)
	downloadControls.Lock()
	if c.cancelling {
		downloadControls.Unlock()
		releaseDownloadControl(key, c)
		return errors.New("下载正在取消")
	}
	c.cancelling = true
	cancel, done := c.cancel, c.done
	active := c.active
	downloadControls.Unlock()
	if cancel != nil {
		cancel()
	}
	if active && done != nil {
		<-done
	}
	d, err := s.downloadRecord(id)
	if err == nil {
		if d.State == "done" || d.State == "cancelled" {
			err = storage.ErrNotFound
		} else {
			d.State = "cancelled"
			err = s.saveDownload(d)
			if err == nil {
				err = os.Remove(d.Staging)
				if errors.Is(err, os.ErrNotExist) {
					err = nil
				}
			}
		}
	}
	downloadControls.Lock()
	c.cancelling = false
	downloadControls.Unlock()
	releaseDownloadControl(key, c)
	return err
}
