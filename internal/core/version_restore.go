package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tamiops/internal/storage"
)

const versionRestorePreviewTTL = 5 * time.Minute

type VersionRestorePreview struct {
	Token               string         `json:"token"`
	ExpiresAt           time.Time      `json:"expiresAt"`
	ConnectionID        string         `json:"connectionId"`
	Path                string         `json:"path"`
	Version             storage.Entry  `json:"version"`
	VersionID           string         `json:"versionId"`
	Current             *storage.Entry `json:"current,omitempty"`
	CurrentExists       bool           `json:"currentExists"`
	ExpectedCurrentETag string         `json:"expectedCurrentEtag"`
}

type storedVersionRestorePreview struct {
	ConnectionID  string
	Path          string
	VersionID     string
	SourceETag    string
	SourceSize    int64
	CurrentETag   string
	CurrentExists bool
	Active        bool
	Expires       string
}

func ensureVersionRestoreSchema(s *Service) error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS version_restore_previews (
		token TEXT PRIMARY KEY, connection TEXT NOT NULL, path TEXT NOT NULL,
		version_id TEXT NOT NULL, source_etag TEXT NOT NULL, source_size INTEGER NOT NULL,
		current_etag TEXT NOT NULL, current_exists INTEGER NOT NULL,
		active INTEGER NOT NULL DEFAULT 0, expires TEXT NOT NULL)`)
	return err
}

func (s *Service) previewVersionRestore(ctx context.Context, connectionID, key, versionID string) (VersionRestorePreview, error) {
	if err := ensureVersionRestoreSchema(s); err != nil {
		return VersionRestorePreview{}, err
	}
	connectionID = strings.TrimSpace(connectionID)
	versionID = strings.TrimSpace(versionID)
	if connectionID == "" || key == "" || versionID == "" {
		return VersionRestorePreview{}, errors.New("缺少连接、路径或版本编号")
	}
	if err := validKey(key); err != nil {
		return VersionRestorePreview{}, err
	}
	// Serialize preview creation against connection scope edits. An edit either
	// sees this token and invalidates it, or completes before this reads storage.
	s.writes.Lock()
	defer s.writes.Unlock()
	if err := s.checkManagedResource(ctx, connectionID, key); err != nil {
		return VersionRestorePreview{}, err
	}
	st, c, err := s.writable(connectionID, key, false)
	if err != nil {
		return VersionRestorePreview{}, err
	}
	if err = s.checkDAVLocks(ctx, c.ID, key); err != nil {
		return VersionRestorePreview{}, err
	}
	versioned, ok := st.(storage.VersionedStore)
	if !ok {
		return VersionRestorePreview{}, storage.ErrUnsupported
	}
	versions, err := versioned.ListVersions(ctx, key)
	if err != nil {
		return VersionRestorePreview{}, err
	}
	var selected *storage.ObjectVersion
	for i := range versions {
		if versions[i].VersionID == versionID {
			v := versions[i]
			selected = &v
			break
		}
	}
	if selected == nil {
		return VersionRestorePreview{}, storage.ErrNotFound
	}
	if selected.DeleteMarker || selected.Entry.IsDir || selected.Entry.Path != key || selected.VersionID == "" || selected.Entry.Size < 0 {
		return VersionRestorePreview{}, errors.New("选择的版本没有可恢复的文件内容")
	}
	current, statErr := st.Stat(ctx, key)
	currentExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return VersionRestorePreview{}, statErr
	}
	preview := VersionRestorePreview{ConnectionID: c.ID, Path: key, Version: selected.Entry, VersionID: selected.VersionID, CurrentExists: currentExists}
	if currentExists {
		if current.IsDir || !strongTag(current.ETag) {
			return VersionRestorePreview{}, errors.New("目标当前版本缺少可靠 ETag，不能安全覆盖")
		}
		preview.Current = &current
		preview.ExpectedCurrentETag = current.ETag
	}
	expires := time.Now().Add(versionRestorePreviewTTL).UTC()
	preview.ExpiresAt = expires
	preview.Token = ID()
	_, err = s.db.Exec(`INSERT INTO version_restore_previews(token,connection,path,version_id,source_etag,source_size,current_etag,current_exists,expires) VALUES(?,?,?,?,?,?,?,?,?)`, preview.Token, c.ID, key, preview.VersionID, preview.Version.ETag, preview.Version.Size, preview.ExpectedCurrentETag, preview.CurrentExists, expires.Format(time.RFC3339Nano))
	if err != nil {
		return VersionRestorePreview{}, err
	}
	_, _ = s.db.Exec(`DELETE FROM version_restore_previews WHERE expires<?`, time.Now().UTC().Format(time.RFC3339Nano))
	return preview, nil
}

func (s *Service) restoreVersion(ctx context.Context, token string) (storage.Entry, error) {
	if err := ensureVersionRestoreSchema(s); err != nil {
		return storage.Entry{}, err
	}
	if token == "" {
		return storage.Entry{}, errors.New("请先预览要恢复的版本")
	}
	// Claim the durable token under the shared writer so a scope edit cannot
	// switch its connection to another bucket after this restore starts.
	s.writes.Lock()
	var p storedVersionRestorePreview
	err := s.db.QueryRow(`SELECT connection,path,version_id,source_etag,source_size,current_etag,current_exists,active,expires FROM version_restore_previews WHERE token=?`, token).Scan(&p.ConnectionID, &p.Path, &p.VersionID, &p.SourceETag, &p.SourceSize, &p.CurrentETag, &p.CurrentExists, &p.Active, &p.Expires)
	if errors.Is(err, sql.ErrNoRows) {
		s.writes.Unlock()
		return storage.Entry{}, storage.ErrConflict
	}
	if err != nil {
		s.writes.Unlock()
		return storage.Entry{}, err
	}
	expires, err := time.Parse(time.RFC3339Nano, p.Expires)
	if err != nil || time.Now().After(expires) || p.Active {
		s.writes.Unlock()
		return storage.Entry{}, storage.ErrConflict
	}
	if p.ConnectionID == "" || p.Path == "" || p.VersionID == "" || p.SourceSize < 0 || validKey(p.Path) != nil {
		s.writes.Unlock()
		return storage.Entry{}, errors.New("版本恢复预览记录无效")
	}
	claimedExpiry := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	claimResult, err := s.db.Exec(`UPDATE version_restore_previews SET active=1,expires=? WHERE token=? AND active=0 AND expires>?`, claimedExpiry, token, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		s.writes.Unlock()
		return storage.Entry{}, err
	}
	claimed, _ := claimResult.RowsAffected()
	if claimed != 1 {
		s.writes.Unlock()
		return storage.Entry{}, storage.ErrConflict
	}
	s.writes.Unlock()
	finished := false
	defer func() {
		if !finished {
			_, _ = s.db.Exec(`UPDATE version_restore_previews SET active=0 WHERE token=?`, token)
		}
	}()
	if err = s.checkManagedResource(ctx, p.ConnectionID, p.Path); err != nil {
		return storage.Entry{}, err
	}
	st, c, err := s.writable(p.ConnectionID, p.Path, false)
	if err != nil {
		return storage.Entry{}, err
	}
	if err = s.checkDAVLocks(ctx, c.ID, p.Path); err != nil {
		return storage.Entry{}, err
	}
	current, statErr := st.Stat(ctx, p.Path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, storage.ErrNotFound) {
		return storage.Entry{}, statErr
	}
	if exists != p.CurrentExists || exists && (current.IsDir || current.ETag != p.CurrentETag || !strongTag(current.ETag)) {
		return storage.Entry{}, storage.ErrConflict
	}
	if !exists && p.CurrentETag != "" {
		return storage.Entry{}, storage.ErrConflict
	}
	versioned, ok := st.(storage.VersionedStore)
	if !ok {
		return storage.Entry{}, storage.ErrUnsupported
	}
	body, opened, err := versioned.OpenVersion(ctx, p.Path, p.VersionID)
	if err != nil {
		return storage.Entry{}, err
	}
	defer body.Close()
	if opened.Path != p.Path || opened.IsDir || opened.Size != p.SourceSize || opened.ETag != p.SourceETag || opened.VersionID != "" && opened.VersionID != p.VersionID {
		return storage.Entry{}, storage.ErrConflict
	}
	condition := storage.Condition{IfNoneMatch: true}
	if exists {
		condition = storage.Condition{IfMatch: p.CurrentETag}
	}
	result, err := (Backend{Service: s, ConnectionID: c.ID}).putStrict(ctx, p.Path, body, p.SourceSize, condition, "")
	if err != nil {
		return result, err
	}
	if _, err = s.db.Exec(`DELETE FROM version_restore_previews WHERE token=?`, token); err != nil {
		return result, fmt.Errorf("版本已恢复，但预览回执未清理：%w", err)
	}
	finished = true
	return result, nil
}

func (s *Service) versionRestoreCommand(r *http.Request) (any, error, bool) {
	switch r.URL.Path {
	case "/api/files/version-restore-preview":
		var req struct {
			ConnectionID string `json:"connectionId"`
			Path         string `json:"path"`
			VersionID    string `json:"versionId"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		v, err := s.previewVersionRestore(ctx, req.ConnectionID, req.Path, req.VersionID)
		return v, err, true
	case "/api/files/version-restore":
		var req struct {
			Token string `json:"token"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		v, err := s.restoreVersion(ctx, req.Token)
		return v, err, true
	default:
		return nil, nil, false
	}
}
