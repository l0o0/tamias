package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	syncScanHistoryPerJob = 100
	syncScanHistoryGlobal = 500
)

type syncScanScope struct {
	LocalPath  string   `json:"localPath"`
	RemotePath string   `json:"remotePath"`
	Direction  string   `json:"direction"`
	Exclude    []string `json:"exclude"`
}

// SyncScanHistory describes one complete local/remote reconciliation attempt.
// Transfer byte counts are estimates captured by the saved preview plan.
type SyncScanHistory struct {
	ID                int64         `json:"id"`
	JobID             string        `json:"jobId"`
	Started           string        `json:"started"`
	Completed         string        `json:"completed"`
	Scope             syncScanScope `json:"scope"`
	Status            string        `json:"status"`
	Error             string        `json:"error,omitempty"`
	LocalFiles        int           `json:"localFiles"`
	RemoteFiles       int           `json:"remoteFiles"`
	LocalDirectories  int           `json:"localDirectories"`
	RemoteDirectories int           `json:"remoteDirectories"`
	Actions           int           `json:"actions"`
	Uploads           int           `json:"uploads"`
	Downloads         int           `json:"downloads"`
	Deletes           int           `json:"deletes"`
	Conflicts         int           `json:"conflicts"`
	Skipped           int           `json:"skipped"`
	UploadBytes       int64         `json:"uploadBytes"`
	DownloadBytes     int64         `json:"downloadBytes"`
}

func (s *Service) ensureSyncScanHistorySchema() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS sync_scan_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job TEXT NOT NULL,
		started TEXT NOT NULL,
		completed TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL,
		status TEXT NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		local_files INTEGER NOT NULL DEFAULT 0,
		remote_files INTEGER NOT NULL DEFAULT 0,
		local_directories INTEGER NOT NULL DEFAULT 0,
		remote_directories INTEGER NOT NULL DEFAULT 0,
		actions INTEGER NOT NULL DEFAULT 0,
		uploads INTEGER NOT NULL DEFAULT 0,
		downloads INTEGER NOT NULL DEFAULT 0,
		deletes INTEGER NOT NULL DEFAULT 0,
		conflicts INTEGER NOT NULL DEFAULT 0,
		skipped INTEGER NOT NULL DEFAULT 0,
		upload_bytes INTEGER NOT NULL DEFAULT 0,
		download_bytes INTEGER NOT NULL DEFAULT 0
	)`)
	return err
}

func (s *Service) beginSyncScan(job Job) (int64, error) {
	if err := s.ensureSyncScanHistorySchema(); err != nil {
		return 0, err
	}
	scope, err := json.Marshal(syncScanScope{LocalPath: job.LocalPath, RemotePath: job.RemotePath, Direction: job.Direction, Exclude: append([]string{}, job.Exclude...)})
	if err != nil {
		return 0, err
	}
	started := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(`INSERT INTO sync_scan_history(job,started,scope,status) VALUES(?,?,?,'scanning')`, job.ID, started, string(scope))
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if _, err = tx.Exec(`DELETE FROM sync_scan_history WHERE job=? AND id IN (
		SELECT id FROM sync_scan_history WHERE job=? ORDER BY id DESC LIMIT -1 OFFSET ?
	)`, job.ID, job.ID, syncScanHistoryPerJob); err == nil {
		_, err = tx.Exec(`DELETE FROM sync_scan_history WHERE id NOT IN (SELECT id FROM sync_scan_history ORDER BY id DESC LIMIT ?)`, syncScanHistoryGlobal)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		return 0, err
	}

	// CancelJob can run while a preview is scanning. Only mark eligible tasks as
	// scanning, and only let the matching scan later restore its own status.
	s.mu.Lock()
	for i := range s.cfg.Jobs {
		j := &s.cfg.Jobs[i]
		if j.ID != job.ID {
			continue
		}
		if j.Enabled && j.Status != "running" && j.Status != "paused" && j.Status != "cancelled" {
			j.Status = "scanning"
			j.Detail = "正在扫描本地与远端文件…"
			if err = s.saveLocked(); err != nil {
				_ = s.closeSyncScan(id, "error", err.Error(), SyncScanHistory{})
			}
		}
		break
	}
	s.mu.Unlock()
	if err != nil {
		return id, err
	}
	return id, nil
}

func (s *Service) finishSyncScan(id int64, job Job, preview Plan, scanErr error, localFiles, remoteFiles, localDirectories, remoteDirectories int) {
	status, message := "success", ""
	summary := scanSummary(preview)
	if scanErr != nil {
		message = scanErr.Error()
		if errors.Is(scanErr, context.Canceled) {
			status = "cancelled"
			summary = "核对已取消"
		} else {
			status = "error"
			summary = "核对失败：" + scanErr.Error()
		}
	} else {
		if scanHasAttention(preview) {
			status = "needs_attention"
		}
	}
	counts := scanCounts(preview)
	counts.LocalFiles, counts.RemoteFiles = localFiles, remoteFiles
	counts.LocalDirectories, counts.RemoteDirectories = localDirectories, remoteDirectories
	if err := s.closeSyncScan(id, status, message, counts); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		current := &s.cfg.Jobs[i]
		if current.ID != job.ID {
			continue
		}
		current.LastScanAt = time.Now().UTC().Format(time.RFC3339)
		current.LastScanSummary = summary
		if current.Status != "scanning" || !current.Enabled {
			_ = s.saveLocked()
			break
		}
		if scanErr != nil {
			if errors.Is(scanErr, context.Canceled) {
				current.Status = "idle"
				current.Detail = "核对已取消，请重新预览"
			} else {
				current.Status = "error"
				current.Detail = "核对失败：" + scanErr.Error()
			}
		} else if job.Status == "retrying" {
			current.Status = "retrying"
			current.Detail = scanSummary(preview)
		} else if status == "needs_attention" {
			current.Status = "needs_attention"
			current.Detail = scanSummary(preview)
		} else if !scanHasPendingWork(preview) {
			current.Status = "synced"
			current.Detail = "已核对，没有待同步的改动"
		} else {
			current.Status = "idle"
			current.Detail = scanSummary(preview)
		}
		_ = s.saveLocked()
		break
	}
}

func scanCounts(preview Plan) SyncScanHistory {
	counts := SyncScanHistory{Actions: len(preview.Actions), Deletes: preview.DeleteCount, UploadBytes: preview.UploadBytes, DownloadBytes: preview.DownloadBytes}
	for _, action := range preview.Actions {
		switch action.Kind {
		case "upload":
			counts.Uploads++
		case "download":
			counts.Downloads++
		case "conflict":
			counts.Conflicts++
		case "skip":
			counts.Skipped++
		}
	}
	return counts
}

func scanHasAttention(preview Plan) bool {
	if preview.RequiresDeleteConfirmation {
		return true
	}
	for _, action := range preview.Actions {
		if action.Kind == "conflict" {
			return true
		}
	}
	return false
}

func scanHasPendingWork(preview Plan) bool {
	for _, action := range preview.Actions {
		if action.Kind != "skip" || action.BaselineMerge || action.ForgetBaseline {
			return true
		}
	}
	return false
}

func scanSummary(preview Plan) string {
	counts := scanCounts(preview)
	var parts []string
	if counts.Uploads > 0 {
		parts = append(parts, fmt.Sprintf("待上传 %d 项，%s", counts.Uploads, formatSyncBytes(preview.UploadBytes)))
	}
	if counts.Downloads > 0 {
		parts = append(parts, fmt.Sprintf("待下载 %d 项，%s", counts.Downloads, formatSyncBytes(preview.DownloadBytes)))
	}
	if counts.Deletes > 0 {
		parts = append(parts, fmt.Sprintf("待删除 %d 项", counts.Deletes))
	}
	if counts.Conflicts > 0 {
		parts = append(parts, fmt.Sprintf("冲突 %d 项", counts.Conflicts))
	}
	if len(parts) == 0 {
		return "已核对，没有待同步的改动"
	}
	return "核对完成：" + strings.Join(parts, "；")
}

func formatSyncBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes) / 1024
	index := 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

func (s *Service) closeSyncScan(id int64, status, message string, counts SyncScanHistory) error {
	_, err := s.db.Exec(`UPDATE sync_scan_history SET completed=?,status=?,error=?,local_files=?,remote_files=?,local_directories=?,remote_directories=?,actions=?,uploads=?,downloads=?,deletes=?,conflicts=?,skipped=?,upload_bytes=?,download_bytes=? WHERE id=? AND status='scanning'`,
		time.Now().UTC().Format(time.RFC3339Nano), status, message, counts.LocalFiles, counts.RemoteFiles, counts.LocalDirectories, counts.RemoteDirectories, counts.Actions, counts.Uploads, counts.Downloads, counts.Deletes, counts.Conflicts, counts.Skipped, counts.UploadBytes, counts.DownloadBytes, id)
	return err
}

func (s *Service) recoverInterruptedSyncScans() error {
	if err := s.ensureSyncScanHistorySchema(); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE sync_scan_history SET completed=?,status='interrupted',error='应用上次退出时核对中断' WHERE status='scanning'`, now); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].Status == "scanning" {
			s.cfg.Jobs[i].Status = "error"
			s.cfg.Jobs[i].Detail = "上次核对中断，请重新预览"
			s.cfg.Jobs[i].LastScanAt = now
			s.cfg.Jobs[i].LastScanSummary = "上次核对中断，请重新预览"
			changed = true
		}
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

func (s *Service) scanHistoryGet(r *http.Request) (any, error, bool) {
	if r.URL.Path != "/api/jobs/scan-history" {
		return nil, nil, false
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("id"))
	if jobID == "" {
		return nil, errors.New("缺少同步任务 ID"), true
	}
	if err := s.ensureSyncScanHistorySchema(); err != nil {
		return nil, err, true
	}
	if _, err := s.job(jobID); err != nil {
		return nil, err, true
	}
	rows, err := s.db.Query(`SELECT id,job,started,completed,scope,status,error,local_files,remote_files,local_directories,remote_directories,actions,uploads,downloads,deletes,conflicts,skipped,upload_bytes,download_bytes
		FROM sync_scan_history WHERE job=? ORDER BY id DESC LIMIT ?`, jobID, syncScanHistoryPerJob)
	if err != nil {
		return nil, err, true
	}
	defer rows.Close()
	history := make([]SyncScanHistory, 0)
	for rows.Next() {
		var item SyncScanHistory
		var rawScope string
		if err = rows.Scan(&item.ID, &item.JobID, &item.Started, &item.Completed, &rawScope, &item.Status, &item.Error, &item.LocalFiles, &item.RemoteFiles, &item.LocalDirectories, &item.RemoteDirectories, &item.Actions, &item.Uploads, &item.Downloads, &item.Deletes, &item.Conflicts, &item.Skipped, &item.UploadBytes, &item.DownloadBytes); err != nil {
			return nil, err, true
		}
		if json.Unmarshal([]byte(rawScope), &item.Scope) != nil {
			item.Scope = syncScanScope{}
		}
		history = append(history, item)
	}
	return history, rows.Err(), true
}
