package core

import (
	"context"
	"errors"
	"net/http"
	"path"
	"tamiops/internal/storage"
	"time"
)

func (s *Service) extendedGet(r *http.Request) (any, error, bool) {
	if v, e, handled := s.scanHistoryGet(r); handled {
		return v, e, true
	}
	if v, e, handled := s.migrationGet(r); handled {
		return v, e, true
	}
	if v, e, handled := s.backupGet(r); handled {
		return v, e, true
	}
	q := r.URL.Query()
	switch r.URL.Path {
	case "/api/jobs/queue":
		v, e := s.SyncQueue(q.Get("id"))
		return v, e, true
	case "/api/gateways/access":
		v, e := s.GatewayAccess(q.Get("id"))
		return v, e, true
	case "/api/downloads":
		v, e := s.Downloads()
		return v, e, true
	case "/api/transfers":
		v, e := s.MultipartUploads()
		return v, e, true
	case "/api/operations":
		v, e := s.PendingOperations()
		return v, e, true
	case "/api/automation/environment":
		return s.Environment(), nil, true
	case "/api/preferences":
		return s.Preferences(), nil, true
	case "/api/config/export":
		v, e := s.ExportConfiguration()
		return v, e, true
	case "/api/diagnostics":
		return s.Diagnostics(), nil, true
	case "/api/statistics":
		v, e := s.Statistics()
		return v, e, true
	case "/api/cache":
		v, e := s.CacheEntries()
		return v, e, true
	case "/api/files/versions":
		v, e := (Backend{s, q.Get("connectionId")}).Versions(r.Context(), q.Get("path"))
		return v, e, true
	}
	return nil, nil, false
}
func (s *Service) extendedCommand(r *http.Request) (any, error, bool) {
	if v, e, handled := s.migrationCommand(r); handled {
		return v, e, true
	}
	if v, e, handled := s.backupCommand(r); handled {
		return v, e, true
	}
	switch r.URL.Path {
	case "/api/config/save":
		value, err := s.ExportConfiguration()
		if err != nil {
			return nil, err, true
		}
		result, err := s.saveGeneratedJSON("tamias-config.json", value)
		return result, err, true
	case "/api/diagnostics/save":
		result, err := s.saveGeneratedJSON("tamias-diagnostics.json", s.Diagnostics())
		return result, err, true
	}
	switch r.URL.Path {
	case "/api/preferences":
		var p Preferences
		if err := decode(r, &p); err != nil {
			return nil, err, true
		}
		return nil, s.SetPreferences(p), true
	case "/api/config/import":
		var c ConfigurationExport
		if err := decode(r, &c); err != nil {
			return nil, err, true
		}
		v, e := s.ImportConfiguration(c)
		return v, e, true
	case "/api/connections/credentials":
		var c ConnectionInput
		if err := decode(r, &c); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		return nil, s.UpdateCredentials(ctx, c), true
	}
	switch r.URL.Path {
	case "/api/operations/adopt-preview", "/api/operations/adopt", "/api/downloads/resume", "/api/downloads/cancel", "/api/transfers/resume", "/api/transfers/abort", "/api/operations/reconcile", "/api/recovery/preview", "/api/recovery/restore", "/api/recovery/delete", "/api/recovery/prune", "/api/gateways/reset-password", "/api/files/copy", "/api/files/move", "/api/files/delete-tree-preview", "/api/files/delete-tree", "/api/cache/fetch", "/api/cache/open", "/api/cache/pin", "/api/cache/delete", "/api/cache/upload", "/api/files/version-save":
	default:
		return nil, nil, false
	}
	var c struct {
		ID           string `json:"id"`
		ETag         string `json:"etag"`
		ConnectionID string `json:"connectionId"`
		Path         string `json:"path"`
		Destination  string `json:"destination"`
		Overwrite    bool   `json:"overwrite"`
		Token        string `json:"token"`
		Pinned       bool   `json:"pinned"`
		Refresh      bool   `json:"refresh"`
		AllowOffline bool   `json:"allowOffline"`
		VersionID    string `json:"versionId"`
	}
	if err := decode(r, &c); err != nil {
		return nil, err, true
	}
	b := Backend{s, c.ConnectionID}
	switch r.URL.Path {
	case "/api/downloads/resume":
		v, e := s.ResumeDownload(r.Context(), c.ID)
		return v, e, true
	case "/api/downloads/cancel":
		return nil, s.CancelDownload(c.ID), true
	case "/api/transfers/resume":
		v, e := s.ResumeUpload(r.Context(), c.ID)
		return v, e, true
	case "/api/transfers/abort":
		return nil, s.AbortUpload(r.Context(), c.ID), true
	case "/api/operations/adopt-preview":
		v, e := s.PreviewOperationAdoption(r.Context(), c.ID)
		return v, e, true
	case "/api/operations/adopt":
		return nil, s.AdoptOperation(r.Context(), c.ID, c.Token), true
	case "/api/operations/reconcile":
		return nil, s.ReconcileOperation(r.Context(), c.ID), true
	case "/api/recovery/preview":
		v, e := s.PreviewRecovery(r.Context(), c.ID)
		return v, e, true
	case "/api/recovery/restore":
		v, e := s.RestoreRecovery(r.Context(), c.ID, c.ETag)
		return v, e, true
	case "/api/recovery/delete":
		return nil, s.DeleteRecovery(c.ID), true
	case "/api/recovery/prune":
		v, e := s.PruneRecoveries(time.Now())
		return v, e, true
	case "/api/gateways/reset-password":
		v, e := s.ResetGatewayPassword(c.ID)
		return map[string]string{"password": v}, e, true
	case "/api/files/copy":
		v, e := b.Copy(r.Context(), c.Path, c.Destination, c.Overwrite)
		return v, e, true
	case "/api/files/move":
		v, e := b.Move(r.Context(), c.Path, c.Destination, c.Overwrite)
		return v, e, true
	case "/api/files/delete-tree-preview":
		v, e := b.PreviewDeleteTree(r.Context(), c.Path)
		return v, e, true
	case "/api/files/delete-tree":
		return nil, b.DeleteTreeWithPreview(r.Context(), c.Path, c.Token), true
	case "/api/cache/fetch", "/api/cache/open":
		v, e := s.fetchCache(r.Context(), c.ConnectionID, c.Path, c.Pinned || r.URL.Path == "/api/cache/open", c.AllowOffline, c.Refresh)
		if e == nil && r.URL.Path == "/api/cache/open" {
			if s.OpenLocal == nil {
				e = errors.New("请在桌面应用中打开缓存文件")
			} else {
				e = s.OpenLocal(v.LocalPath)
			}
		}
		return v, e, true
	case "/api/cache/pin":
		return nil, s.PinCache(c.ID, c.Pinned), true
	case "/api/cache/delete":
		return nil, s.RemoveCache(c.ID), true
	case "/api/cache/upload":
		v, e := s.UploadCache(r.Context(), c.ID)
		return v, e, true
	case "/api/files/version-save":
		if s.PickSave == nil {
			return nil, errors.New("请在桌面应用中保存历史版本"), true
		}
		dest, e := s.PickSave(path.Base(c.Path))
		if e != nil {
			return nil, e, true
		}
		if dest == "" {
			return nil, errors.New("已取消保存"), true
		}
		return nil, b.SaveVersion(r.Context(), c.Path, c.VersionID, dest), true
	}
	return nil, storage.ErrNotFound, true
}
