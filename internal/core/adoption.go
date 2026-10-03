package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"tamiops/internal/storage"
)

type OperationAdoption struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	Exists    bool   `json:"exists"`
	Directory bool   `json:"directory"`
}

// Adoption acknowledges current state; it never claims to prove who created a collection.
func (s *Service) previewOperationAdoption(ctx context.Context, id string) (OperationAdoption, operationReceipt, error) {
	out := OperationAdoption{ID: id}
	var state, kind, raw string
	var evidence operationReceipt
	if !validRecoveryID(id) {
		return out, evidence, errors.New("无效操作编号")
	}
	if err := s.db.QueryRow("SELECT state,kind,receipt FROM operations WHERE id=?", id).Scan(&state, &kind, &raw); err != nil {
		return out, evidence, err
	}
	if state != "uncertain" || kind != "mkdir" {
		return out, evidence, errors.New("仅可接管结果待核对的目录创建操作")
	}
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil || evidence.Version != 1 || evidence.DestinationConnection == "" || evidence.DestinationPath == "" {
		return out, evidence, errors.New("目录操作缺少有效回执")
	}
	st, err := s.store(evidence.DestinationConnection)
	if err != nil {
		return out, evidence, err
	}
	entry, err := st.Stat(ctx, evidence.DestinationPath)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return out, evidence, err
	}
	out.Exists = err == nil
	out.Directory = out.Exists && entry.IsDir
	if out.Exists && !out.Directory {
		return out, evidence, errors.New("目标已成为文件，请先处理路径冲突")
	}
	data, _ := json.Marshal([]any{id, raw, out.Exists, entry})
	h := sha256.Sum256(data)
	out.Token = hex.EncodeToString(h[:])
	return out, evidence, nil
}
func (s *Service) PreviewOperationAdoption(ctx context.Context, id string) (OperationAdoption, error) {
	s.writes.Lock()
	defer s.writes.Unlock()
	out, _, err := s.previewOperationAdoption(ctx, id)
	return out, err
}
func (s *Service) AdoptOperation(ctx context.Context, id, token string) error {
	s.writes.Lock()
	defer s.writes.Unlock()
	preview, evidence, err := s.previewOperationAdoption(ctx, id)
	if err != nil {
		return err
	}
	if token == "" || preview.Token != token {
		return storage.ErrConflict
	}
	evidence.Step = "manually-adopted-current-state"
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	state := "adopted"
	if !preview.Exists {
		state = "failed"
	}
	_, err = s.db.Exec("UPDATE operations SET state=?,receipt=? WHERE id=? AND state='uncertain'", state, string(raw), id)
	if err == nil {
		s.activity("operation", "manual", "已接管目录当前状态；原操作结果未被推定", "")
	}
	return err
}
