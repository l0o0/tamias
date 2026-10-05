package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"tamiops/internal/storage"
	"time"
)

const multipartPartSize int64 = 8 << 20

type MultipartUpload struct {
	OperationID  string                  `json:"operationId"`
	ConnectionID string                  `json:"connectionId"`
	Path         string                  `json:"path"`
	UploadID     string                  `json:"-"`
	Staging      string                  `json:"-"`
	Hash         string                  `json:"hash"`
	Size         int64                   `json:"size"`
	Condition    storage.Condition       `json:"-"`
	Parts        []storage.MultipartPart `json:"parts"`
	State        string                  `json:"state"`
}

func (s *Service) putRemote(ctx context.Context, connectionID string, st storage.Store, key string, f *os.File, size int64, cond storage.Condition, op string) (storage.Entry, error) {
	multipart, ok := st.(storage.MultipartStore)
	c, err := s.connection(connectionID)
	if err != nil {
		return storage.Entry{}, err
	}
	if !ok || c.WriteMode == storage.WriteModeCopy || !c.Capabilities.MultipartConditional || size < 2*multipartPartSize {
		return st.Put(ctx, key, f, size, cond)
	}
	hash, _, err := fileSHA256(f)
	if err != nil {
		return storage.Entry{}, err
	}
	upload := MultipartUpload{OperationID: op, ConnectionID: connectionID, Path: key, Staging: f.Name(), Hash: hash, Size: size, Condition: cond, Parts: []storage.MultipartPart{}, State: "creating"}
	if err = s.saveMultipart(upload); err != nil {
		return storage.Entry{}, err
	}
	upload.UploadID, err = multipart.CreateMultipart(ctx, key)
	if err != nil {
		return storage.Entry{}, err
	}
	upload.State = "uploading"
	if err = s.saveMultipart(upload); err != nil {
		_ = multipart.AbortMultipart(ctx, key, upload.UploadID)
		return storage.Entry{}, err
	}
	return s.continueMultipart(ctx, multipart, &upload, f)
}
func (s *Service) saveMultipart(u MultipartUpload) error {
	raw, err := json.Marshal(struct {
		UploadID  string
		Staging   string
		Condition storage.Condition
		Parts     []storage.MultipartPart
	}{u.UploadID, u.Staging, u.Condition, u.Parts})
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO multipart_uploads(operation_id,connection,path,hash,size,state,data) VALUES(?,?,?,?,?,?,?) ON CONFLICT(operation_id) DO UPDATE SET state=excluded.state,data=excluded.data", u.OperationID, u.ConnectionID, u.Path, u.Hash, u.Size, u.State, string(raw))
	return err
}
func (s *Service) MultipartUploads() ([]MultipartUpload, error) {
	rows, err := s.db.Query("SELECT operation_id,connection,path,hash,size,state,data FROM multipart_uploads WHERE state NOT IN ('done','aborted')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MultipartUpload{}
	for rows.Next() {
		var u MultipartUpload
		var raw string
		if err = rows.Scan(&u.OperationID, &u.ConnectionID, &u.Path, &u.Hash, &u.Size, &u.State, &raw); err != nil {
			return nil, err
		}
		var hidden struct {
			UploadID  string
			Staging   string
			Condition storage.Condition
			Parts     []storage.MultipartPart
		}
		if err = json.Unmarshal([]byte(raw), &hidden); err != nil {
			return nil, err
		}
		u.UploadID = hidden.UploadID
		u.Staging = hidden.Staging
		u.Condition = hidden.Condition
		u.Parts = hidden.Parts
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Service) continueMultipart(ctx context.Context, st storage.MultipartStore, u *MultipartUpload, f *os.File) (storage.Entry, error) {
	count := int((u.Size + multipartPartSize - 1) / multipartPartSize)
	if count > 10000 {
		return storage.Entry{}, errors.New("分片数量超过限制")
	}
	for i := len(u.Parts); i < count; i++ {
		offset := int64(i) * multipartPartSize
		size := multipartPartSize
		if remaining := u.Size - offset; remaining < size {
			size = remaining
		}
		part, err := st.UploadPart(ctx, u.Path, u.UploadID, int32(i+1), s.limitReader(ctx, io.NewSectionReader(f, offset, size)), size)
		if err != nil {
			u.State = "paused"
			_ = s.saveMultipart(*u)
			return storage.Entry{}, err
		}
		if part.Number != int32(i+1) || part.ETag == "" || part.Size != size {
			return storage.Entry{}, errors.New("分片回执不完整")
		}
		u.Parts = append(u.Parts, part)
		if err = s.saveMultipart(*u); err != nil {
			return storage.Entry{}, err
		}
	}
	u.State = "completing"
	if err := s.saveMultipart(*u); err != nil {
		return storage.Entry{}, err
	}
	entry, err := st.CompleteMultipart(ctx, u.Path, u.UploadID, u.Parts, u.Condition)
	if err != nil {
		if errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrUnsupported) {
			u.State = "rejected"
			_ = s.saveMultipart(*u)
		}
		return entry, err
	}
	if !strongTag(entry.ETag) {
		return entry, errors.New("完成分片未返回可靠版本，请核对结果")
	}
	u.State = "done"
	if err = s.saveMultipart(*u); err != nil {
		return entry, err
	}
	return entry, nil
}
func (s *Service) ResumeUpload(ctx context.Context, id string) (storage.Entry, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	uploads, err := s.MultipartUploads()
	if err != nil {
		return storage.Entry{}, err
	}
	var u *MultipartUpload
	for i := range uploads {
		if uploads[i].OperationID == id {
			u = &uploads[i]
			break
		}
	}
	if u == nil {
		return storage.Entry{}, storage.ErrNotFound
	}
	if u.State == "completing" {
		return storage.Entry{}, errors.New("完成请求结果未知，请先核对操作；不能直接重放完成请求")
	}
	if u.UploadID == "" {
		return storage.Entry{}, errors.New("上传会话创建结果未知，请核对并清理远端未完成分片")
	}
	c, err := s.connection(u.ConnectionID)
	if err != nil {
		return storage.Entry{}, err
	}
	if !canWriteStrict(c) || !c.Capabilities.MultipartConditional {
		return storage.Entry{}, errors.New("请先重新验证分片条件提交能力")
	}
	if err = s.checkDAVLocks(ctx, u.ConnectionID, u.Path); err != nil {
		return storage.Entry{}, err
	}
	st, err := s.store(u.ConnectionID)
	if err != nil {
		return storage.Entry{}, err
	}
	mp, ok := st.(storage.MultipartStore)
	if !ok {
		return storage.Entry{}, storage.ErrUnsupported
	}
	if filepath.Dir(u.Staging) != filepath.Join(s.dir, "staging") {
		return storage.Entry{}, errors.New("上传暂存路径无效")
	}
	hash, size, err := hashPlain(u.Staging)
	if err != nil || hash != u.Hash || size != u.Size {
		return storage.Entry{}, errors.New("上传暂存内容已变化或缺失")
	}
	current, statErr := st.Stat(ctx, u.Path)
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return storage.Entry{}, statErr
	}
	if (u.Condition.IfNoneMatch && statErr == nil) || (u.Condition.IfMatch != "" && (statErr != nil || current.ETag != u.Condition.IfMatch)) {
		return storage.Entry{}, storage.ErrConflict
	}
	remoteParts, err := mp.ListParts(ctx, u.Path, u.UploadID)
	if err != nil {
		return storage.Entry{}, err
	}
	if len(remoteParts) < len(u.Parts) {
		return storage.Entry{}, errors.New("远端分片回执缺失")
	}
	for i, p := range u.Parts {
		if remoteParts[i] != p {
			return storage.Entry{}, errors.New("远端分片与已保存回执不一致")
		}
	}
	// An unjournalled part may have succeeded before interruption. Re-uploading the
	// same immutable bytes to the same part number is safe; completion is still conditional.
	f, err := os.Open(u.Staging)
	if err != nil {
		return storage.Entry{}, err
	}
	defer f.Close()
	commit, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	entry, opErr := s.continueMultipart(commit, mp, u, f)
	err = s.finish(id, "upload", u.Path, entry, opErr)
	if err == nil {
		_ = f.Close()
		_ = os.Remove(u.Staging)
	}
	return entry, err
}
func (s *Service) AbortUpload(ctx context.Context, id string) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	uploads, err := s.MultipartUploads()
	if err != nil {
		return err
	}
	for _, u := range uploads {
		if u.OperationID != id {
			continue
		}
		if u.State == "completing" {
			return errors.New("提交结果未知，先核对远端操作再清理")
		}
		st, err := s.store(u.ConnectionID)
		if err != nil {
			return err
		}
		mp, ok := st.(storage.MultipartStore)
		if !ok {
			return storage.ErrUnsupported
		}
		if u.UploadID == "" {
			return errors.New("缺少远端上传编号，不能证明分片已清理")
		}
		if err = mp.AbortMultipart(ctx, u.Path, u.UploadID); err != nil {
			return err
		}
		u.State = "aborted"
		if err = s.saveMultipart(u); err != nil {
			return err
		}
		if _, err = s.db.Exec("UPDATE operations SET state='failed' WHERE id=?", id); err != nil {
			return err
		}
		s.removeBackup(id)
		if err = os.Remove(u.Staging); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return fmt.Errorf("上传会话不存在: %w", storage.ErrNotFound)
}
