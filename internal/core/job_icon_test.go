package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tamiops/internal/storage"
)

func TestJobIconPersistsAndDoesNotChangeSyncState(t *testing.T) {
	s, vault, connection, _ := setupEditableConnection(t)
	dataDir := s.dir
	local := t.TempDir()
	job, err := s.AddJob(Job{
		Name: "icon persistence", ConnectionID: connection.ID,
		LocalPath: local, RemotePath: "icons", Direction: "upload", Icon: "folder",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Normal edits can set a built-in icon and persist it along with the job.
	job.Name = "renamed icon task"
	job.Icon = "book"
	job, err = s.UpdateJob(job)
	if err != nil || job.Icon != "book" {
		t.Fatalf("UpdateJob icon=%q err=%v", job.Icon, err)
	}

	// Model an active job with a preview, queued work, and learned baseline.
	// The appearance-only endpoint must leave all of those records untouched.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = s.db.Exec(`INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?)`, job.ID, "keep.txt", "local", "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO sync_queue(job,path,kind,state,updated) VALUES(?,?,?,'pending',?)`, job.ID, "keep.txt", "upload", now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?)`, job.ID, "preview-token", `{}`, now); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == job.ID {
			s.cfg.Jobs[i].Status = "running"
			s.cfg.Jobs[i].Enabled = true
			s.cfg.Jobs[i].ActiveAction = "uploading keep.txt"
			s.cfg.Jobs[i].Progress = 37
			s.cfg.Jobs[i].QueueTotal = 4
			s.cfg.Jobs[i].QueueDone = 1
			s.cfg.Jobs[i].LastScanSummary = "preview remains valid"
		}
	}
	s.plans["preview-token"] = Plan{JobID: job.ID, Token: "preview-token"}
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"id": job.ID, "icon": "squirrel"})
	req := httptest.NewRequest(http.MethodPost, "/api/jobs/icon", strings.NewReader(string(body)))
	req.Header.Set("X-Tami-Client", "desktop")
	response := httptest.NewRecorder()
	s.Handler(nil).ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/jobs/icon status=%d body=%s", response.Code, response.Body.String())
	}

	s.mu.Lock()
	var current Job
	for _, candidate := range s.cfg.Jobs {
		if candidate.ID == job.ID {
			current = candidate
			break
		}
	}
	s.mu.Unlock()
	if current.Icon != "squirrel" || current.Status != "running" || !current.Enabled || current.ActiveAction != "uploading keep.txt" || current.Progress != 37 || current.QueueTotal != 4 || current.QueueDone != 1 || current.LastScanSummary != "preview remains valid" {
		t.Fatalf("appearance update changed job state: %+v", current)
	}
	for name, query := range map[string]string{
		"baseline": `SELECT count(*) FROM baseline WHERE job=? AND path='keep.txt'`,
		"queue":    `SELECT count(*) FROM sync_queue WHERE job=? AND path='keep.txt' AND state='pending'`,
		"plan":     `SELECT count(*) FROM sync_plans WHERE job=? AND token='preview-token'`,
	} {
		var count int
		if err := s.db.QueryRow(query, job.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s records count=%d err=%v", name, count, err)
		}
	}
	if _, ok := s.plans["preview-token"]; !ok {
		t.Fatal("appearance update invalidated the in-memory preview")
	}

	// A sync completion writes run state back to the live record and must keep
	// the concurrently selected icon.
	if _, err := s.finishJob(job.ID, nil, syncRunCounts{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(dataDir, vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	var restartedJob Job
	for _, candidate := range restarted.Snapshot()["jobs"].([]Job) {
		if candidate.ID == job.ID {
			restartedJob = candidate
		}
	}
	if restartedJob.Icon != "squirrel" {
		t.Fatalf("restart lost the selected icon: %+v", restartedJob)
	}

	exported, err := restarted.ExportConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	if len(exported.Jobs) != 1 || exported.Jobs[0].Icon != "squirrel" {
		t.Fatalf("exported jobs=%+v", exported.Jobs)
	}
	imported, err := New(filepath.Join(t.TempDir(), "imported"), &memoryVault{m: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = imported.Close() })
	if _, err := imported.ImportConfiguration(exported); err != nil {
		t.Fatal(err)
	}
	jobs := imported.Snapshot()["jobs"].([]Job)
	if len(jobs) != 1 || jobs[0].Icon != "squirrel" {
		t.Fatalf("imported jobs=%+v", jobs)
	}
}

func TestJobIconRejectsUnknownValuesOnCreateEditAndImport(t *testing.T) {
	if _, err := (&Service{}).AddJob(Job{Icon: `data:image/svg+xml,<svg/>`}); err == nil || err.Error() != "无效任务图标" {
		t.Fatalf("AddJob accepted an arbitrary icon: %v", err)
	}
	if _, err := (&Service{}).UpdateJob(Job{Icon: "not-a-built-in"}); err == nil || err.Error() != "无效任务图标" {
		t.Fatalf("UpdateJob accepted an unknown icon: %v", err)
	}
	s, err := New(t.TempDir(), &memoryVault{m: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := ConfigurationExport{
		Version:     1,
		Connections: []storage.Config{{ID: "source", Name: "source", Kind: "webdav", Endpoint: "https://example.test/dav"}},
		Jobs:        []Job{{ConnectionID: "source", Direction: "upload", Icon: "https://example.test/icon.svg"}},
	}
	if _, err := s.ImportConfiguration(input); err == nil || err.Error() != "无效任务图标" {
		t.Fatalf("ImportConfiguration accepted an arbitrary icon: %v", err)
	}

	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, Connection{Config: storage.Config{ID: "rollback-connection", Kind: "webdav", Endpoint: "https://example.test/dav"}})
	s.cfg.Jobs = append(s.cfg.Jobs, Job{ID: "rollback-job", ConnectionID: "rollback-connection", Icon: "folder"})
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJobIcon("rollback-job", "star"); err == nil {
		t.Fatal("UpdateJobIcon succeeded with a closed settings database")
	}
	s.mu.Lock()
	iconAfterSaveFailure := s.cfg.Jobs[len(s.cfg.Jobs)-1].Icon
	s.mu.Unlock()
	if iconAfterSaveFailure != "folder" {
		t.Fatalf("failed save left the in-memory icon changed to %q", iconAfterSaveFailure)
	}
}
