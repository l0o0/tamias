package core

import (
	"errors"
	"net/http"
	"path/filepath"
)

func (s *Service) backupGet(r *http.Request) (any, error, bool) {
	q := r.URL.Query()
	switch r.URL.Path {
	case "/api/backups":
		v, e := s.ListBackupJobs()
		return v, e, true
	case "/api/backups/snapshots":
		v, e := s.ListBackupSnapshots(q.Get("jobId"))
		return v, e, true
	case "/api/backups/remote-snapshots":
		v, e := s.ListRemoteBackupSnapshots(r.Context(), q.Get("connectionId"), q.Get("prefix"))
		return v, e, true
	}
	return nil, nil, false
}
func (s *Service) backupCommand(r *http.Request) (any, error, bool) {
	switch r.URL.Path {
	case "/api/backups", "/api/backups/update":
		var b BackupJob
		if err := decode(r, &b); err != nil {
			return nil, err, true
		}
		if r.URL.Path == "/api/backups" {
			v, e := s.CreateBackupJob(b)
			return v, e, true
		}
		v, e := s.UpdateBackupJob(b)
		return v, e, true
	}
	switch r.URL.Path {
	case "/api/backups/delete", "/api/backups/preview", "/api/backups/run", "/api/backups/restore", "/api/backups/restore-remote":
	default:
		return nil, nil, false
	}
	var c struct {
		ID           string `json:"id"`
		Token        string `json:"token"`
		ConnectionID string `json:"connectionId"`
		Prefix       string `json:"prefix"`
		Destination  string `json:"destination"`
	}
	if err := decode(r, &c); err != nil {
		return nil, err, true
	}
	switch r.URL.Path {
	case "/api/backups/delete":
		return nil, s.DeleteBackupJob(c.ID), true
	case "/api/backups/preview":
		v, e := s.PreviewBackup(r.Context(), c.ID)
		return v, e, true
	case "/api/backups/run":
		v, e := s.RunBackup(r.Context(), c.ID, c.Token)
		return v, e, true
	case "/api/backups/restore", "/api/backups/restore-remote":
		if c.Destination == "" {
			if s.PickFolder == nil {
				return nil, errors.New("请选择恢复父目录"), true
			}
			base, err := s.PickFolder()
			if err != nil {
				return nil, err, true
			}
			if base == "" {
				return nil, errors.New("已取消恢复"), true
			}
			c.Destination = filepath.Join(base, "tamiops-恢复-"+ID()[:8])
		}
		var err error
		if r.URL.Path == "/api/backups/restore-remote" {
			err = s.RestoreRemoteBackupSnapshot(r.Context(), c.ConnectionID, c.Prefix, c.ID, c.Destination)
		} else {
			err = s.RestoreBackupSnapshot(r.Context(), c.ID, c.Destination)
		}
		return map[string]string{"path": c.Destination}, err, true
	}
	return nil, nil, false
}
