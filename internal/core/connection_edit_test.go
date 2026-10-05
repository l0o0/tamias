package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type editDAVObject struct {
	data []byte
	etag string
}

type editDAV struct {
	mu                    sync.Mutex
	objects               map[string]editDAVObject
	next                  int
	ignoreCreateCondition bool
}

func newEditDAV(t *testing.T) (string, *editDAV) {
	t.Helper()
	dav := &editDAV{objects: map[string]editDAVObject{}}
	server := httptest.NewServer(http.HandlerFunc(dav.serveHTTP))
	t.Cleanup(server.Close)
	return server.URL + "/dav", dav
}

func (d *editDAV) serveHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/dav/")
	if key == "/dav" || key == r.URL.Path || r.URL.Path == "/dav/" {
		key = ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch r.Method {
	case "PROPFIND":
		obj, exists := d.objects[key]
		if exists {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>%s</d:getetag><d:getcontentlength>%d</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.EscapedPath(), obj.etag, len(obj.data))
			return
		}
		if key != "" && !strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype><d:getcontentlength>0</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.EscapedPath())
	case http.MethodPut:
		old, exists := d.objects[key]
		if (r.Header.Get("If-None-Match") == "*" && exists && !d.ignoreCreateCondition) || r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != old.etag) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.next++
		etag := fmt.Sprintf(`"dav-%d"`, d.next)
		d.objects[key] = editDAVObject{data: body, etag: etag}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		obj, ok := d.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != obj.etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("ETag", obj.etag)
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(obj.data)))
			w.WriteHeader(http.StatusPartialContent)
			if len(obj.data) > 0 {
				_, _ = w.Write(obj.data[:1])
			}
			return
		}
		_, _ = w.Write(obj.data)
	case http.MethodDelete:
		obj, ok := d.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != obj.etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(d.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setupEditableConnection(t *testing.T) (*Service, *memoryVault, Connection, *editDAV) {
	t.Helper()
	endpoint, dav := newEditDAV(t)
	vault := &memoryVault{m: map[string]string{}}
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := storage.Config{ID: ID(), Name: "remote", Kind: "webdav", Endpoint: endpoint, Username: "alice"}
	creds := storage.Credentials{Username: "alice", Password: "old-password"}
	raw, _ := json.Marshal(creds)
	if err = vault.Set("connection:"+cfg.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	st, err := storage.New(cfg, creds)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, Connection{Config: cfg})
	s.stores[cfg.ID] = st
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return s, vault, Connection{Config: cfg}, dav
}

func editInput(c Connection) ConnectionInput {
	return ConnectionInput{Config: c.Config}
}

func TestConnectionEditKeepsIDAndBlankPasswordUsesStoredCredential(t *testing.T) {
	s, vault, old, _ := setupEditableConnection(t)
	s.mu.Lock()
	s.cfg.Connections[0].WriteRestriction = "旧连接的安全写入限制"
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	in := editInput(old)
	in.Name = "renamed"
	in.Username = ""
	in.WriteMode = storage.WriteModeStrict
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" || !preview.Capabilities.ConditionalWrite || !preview.Capabilities.ConditionalDelete {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	encoded, _ := json.Marshal(preview)
	if strings.Contains(string(encoded), "old-password") {
		t.Fatal("preview exposed a stored password")
	}
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != old.ID || updated.Name != "renamed" || updated.Config.Endpoint != old.Endpoint || updated.Username != "alice" {
		t.Fatalf("connection identity/config changed unexpectedly: %+v", updated)
	}
	if updated.WriteRestriction != "" {
		t.Fatalf("configuration edit retained the old write restriction: %q", updated.WriteRestriction)
	}
	creds, _, err := loadConnectionCredentials(s, old.ID)
	if err != nil || creds.Password != "old-password" || creds.Username != "alice" {
		t.Fatalf("blank password did not retain credentials: %+v err=%v", creds, err)
	}
	if _, ok := vault.m["connection:"+old.ID]; !ok {
		t.Fatal("connection credential was removed")
	}
}

func TestConnectionEditAllowsUnsupportedConditionalWritesAndPersistsMode(t *testing.T) {
	s, _, old, dav := setupEditableConnection(t)
	dav.mu.Lock()
	dav.ignoreCreateCondition = true
	dav.mu.Unlock()
	in := editInput(old)
	in.Name = "readable without conditions"
	in.WriteMode = storage.WriteModeStrict
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" {
		t.Fatalf("unsupported conditional operations blocked an editable connection: preview=%+v err=%v", preview, err)
	}
	if preview.Capabilities.ConditionalWrite || preview.Capabilities.ConditionalDelete || preview.WriteRestriction == "" || preview.CompatibilityProfile != connectionProfileCompatible {
		t.Fatalf("conditional capability failure was not reported truthfully: %+v", preview)
	}
	dav.mu.Lock()
	probeWrites := dav.next
	dav.mu.Unlock()
	if probeWrites == 0 {
		t.Fatal("preview did not automatically verify conditional writes")
	}
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatalf("apply failed after an explicitly unsupported conditional probe: %v", err)
	}
	if updated.WriteMode != storage.WriteModeStrict || updated.Capabilities.ConditionalWrite || updated.Capabilities.ConditionalDelete || updated.WriteRestriction == "" {
		t.Fatalf("saved connection lost selected mode or restriction: %+v", updated)
	}
	if updated.CompatibilityProfile != connectionProfileCompatible || !connectionDetectionCurrent(updated) {
		t.Fatalf("saved connection lost its measured compatibility profile: %+v", updated)
	}
	dav.mu.Lock()
	if dav.next != probeWrites {
		dav.mu.Unlock()
		t.Fatalf("apply repeated the full conditional probe: PUT count %d -> %d", probeWrites, dav.next)
	}
	dav.mu.Unlock()
	var raw string
	if err = s.db.QueryRow(`SELECT data FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved config
	if err = json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Connections) != 1 || saved.Connections[0].WriteMode != storage.WriteModeStrict {
		t.Fatalf("write mode was not persisted in the main config: %+v", saved.Connections)
	}
}

func TestConnectionEditModeIsFingerprintBoundAndUnknownModeRejected(t *testing.T) {
	s, _, old, _ := setupEditableConnection(t)
	in := editInput(old)
	in.WriteMode = storage.WriteModeCompatible
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" {
		t.Fatalf("valid mode preview failed: preview=%+v err=%v", preview, err)
	}
	stale := in
	stale.WriteMode = storage.WriteModeCopy
	if _, err = s.applyConnectionEdit(context.Background(), stale, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("preview token accepted a different write mode: %v", err)
	}
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WriteMode != storage.WriteModeCompatible {
		t.Fatalf("selected mode did not persist: got %q", updated.WriteMode)
	}
	invalid := editInput(old)
	invalid.WriteMode = "permissive"
	if _, err = s.previewConnectionEdit(context.Background(), invalid); err == nil {
		t.Fatal("connection edit accepted an unknown write mode")
	}
	if _, err = s.AddConnection(context.Background(), ConnectionInput{Config: storage.Config{Name: "invalid", Kind: "webdav", Endpoint: "https://example.test/dav", WriteMode: "permissive"}}); err == nil {
		t.Fatal("connection creation accepted an unknown write mode")
	}
}

func TestWriteModeEditPausesJobsAndInvalidatesPlansButKeepsBaselines(t *testing.T) {
	s, _, old, _ := setupEditableConnection(t)
	jobID := ID()
	s.mu.Lock()
	s.cfg.Jobs = append(s.cfg.Jobs, Job{ID: jobID, Name: "sync", ConnectionID: old.ID, Enabled: true, Status: "idle"})
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	if err := s.ensureSyncSchema(); err != nil {
		t.Fatal(err)
	}
	if err := ensureConnectionEditSchema(s); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?)`, jobID, "keep.txt", "local-hash", `"remote-etag"`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_queue(job,path,kind,state,updated) VALUES(?,?,?,'pending',?)`, jobID, "keep.txt", "upload", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?)`, jobID, "old-token", "{}", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_dirty(job,token,updated) VALUES(?,?,?)`, jobID, "dirty-token", now); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.plans["old-token"] = Plan{Token: "old-token", JobID: jobID}
	s.mu.Unlock()
	in := editInput(old)
	in.WriteMode = storage.WriteModeCompatible
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" || !preview.WriteModeChanged || preview.Impact.ScopeChanged {
		t.Fatalf("mode-change preview incorrect: preview=%+v err=%v", preview, err)
	}
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WriteMode != storage.WriteModeCompatible {
		t.Fatalf("mode was not saved: %+v", updated)
	}
	for _, table := range []string{"sync_queue", "sync_plans", "sync_dirty"} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE job=?`, jobID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained the old plan state count=%d err=%v", table, count, err)
		}
	}
	var baselines int
	if err = s.db.QueryRow(`SELECT count(*) FROM baseline WHERE job=?`, jobID).Scan(&baselines); err != nil || baselines != 1 {
		t.Fatalf("mode change did not preserve the sync baseline: count=%d err=%v", baselines, err)
	}
	s.mu.Lock()
	var after Job
	for _, job := range s.cfg.Jobs {
		if job.ID == jobID {
			after = job
		}
	}
	_, hasOldPlan := s.plans["old-token"]
	s.mu.Unlock()
	if after.Enabled || after.Status != "paused" || !strings.Contains(after.Detail, "重新预览") || hasOldPlan {
		t.Fatalf("mode change did not pause and invalidate sync preview state: job=%+v oldPlanRetained=%v", after, hasOldPlan)
	}
}

func TestWriteTestInCopyModeOnlyChecksReadAccess(t *testing.T) {
	s, _, old, dav := setupEditableConnection(t)
	restriction := "existing conditional-write restriction"
	s.mu.Lock()
	s.cfg.Connections[0].WriteMode = storage.WriteModeCopy
	s.cfg.Connections[0].WriteRestriction = restriction
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	if _, err := s.TestConnection(context.Background(), old.ID, true); err != nil {
		t.Fatal(err)
	}
	dav.mu.Lock()
	objects := len(dav.objects)
	dav.mu.Unlock()
	if objects != 0 {
		t.Fatalf("copy-mode connection test mutated remote objects: %d", objects)
	}
	updated, err := s.connection(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Tested || updated.Capabilities.ConditionalWrite || updated.Capabilities.ConditionalDelete || updated.WriteRestriction != restriction {
		t.Fatalf("copy-mode read validation changed write status: %+v", updated)
	}
}

func TestUpdateCredentialsClearsKnownWriteRestriction(t *testing.T) {
	s, _, old, _ := setupEditableConnection(t)
	s.mu.Lock()
	s.cfg.Connections[0].WriteRestriction = "旧凭据对应的安全写入限制"
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()

	input := ConnectionInput{Config: old.Config, Password: "new-password"}
	if err := s.UpdateCredentials(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	updated, err := s.connection(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WriteRestriction != "" || updated.CompatibilityProfile != connectionProfileConditional || !updated.Capabilities.ConditionalWrite || !updated.Capabilities.ConditionalDelete || !connectionDetectionCurrent(updated) {
		t.Fatalf("credential update did not persist fresh capability detection: %+v", updated)
	}
}

func TestConnectionEditRejectsExpiredAndStalePreviews(t *testing.T) {
	s, _, old, _ := setupEditableConnection(t)
	in := editInput(old)
	in.Name = "expired preview"
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE connection_edit_previews SET expires=? WHERE token=?`, time.Now().Add(-time.Minute).Format(time.RFC3339Nano), preview.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.applyConnectionEdit(context.Background(), in, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expired preview accepted: %v", err)
	}
	in.Name = "stale preview"
	preview, err = s.previewConnectionEdit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.Connections[0].Name = "concurrently renamed"
	_ = s.saveLocked()
	s.mu.Unlock()
	if _, err = s.applyConnectionEdit(context.Background(), in, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale config preview accepted: %v", err)
	}
}

func TestConnectionScopeEditResetsPausedSyncStateAndLeavesTaskDisabled(t *testing.T) {
	s, _, old, _ := setupEditableConnection(t)
	jobID := ID()
	s.mu.Lock()
	s.cfg.Jobs = append(s.cfg.Jobs, Job{ID: jobID, Name: "sync", ConnectionID: old.ID, Enabled: false, Status: "paused"})
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = ensureVersionRestoreSchema(s); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO version_restore_previews(token,connection,path,version_id,source_etag,source_size,current_etag,current_exists,expires) VALUES(?,?,?,?,?,?,?,?,?)`, "old-version-token", old.ID, "old.txt", "version-1", `"old"`, 3, `"current"`, true, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err = ensureConnectionEditSchema(s); err != nil {
		t.Fatal(err)
	}
	if err = s.ensureSyncSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?)`, jobID, "old.txt", "h", `"e"`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO sync_queue(job,path,kind,state,updated) VALUES(?,?,?,'pending',?)`, jobID, "old.txt", "upload", time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?)`, jobID, "old-token", "{}", time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO sync_dirty(job,token,updated) VALUES(?,?,?)`, jobID, "dirty-token", time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	in := editInput(old)
	in.Prefix = "next-root"
	in.Username = "bob"
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" || !preview.Impact.ScopeChanged || len(preview.Impact.SyncJobs) != 1 {
		t.Fatalf("scope preview=%+v err=%v", preview, err)
	}
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != old.ID || updated.Prefix != "next-root" || updated.Username != "bob" {
		t.Fatalf("scope edit failed: %+v", updated)
	}
	creds, _, err := loadConnectionCredentials(s, old.ID)
	if err != nil || creds.Username != "bob" || creds.Password != "old-password" {
		t.Fatalf("username scope edit did not retain the blank password: %+v err=%v", creds, err)
	}
	for _, table := range []string{"baseline", "sync_queue", "sync_plans", "sync_dirty"} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE job=?`, jobID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained old storage state count=%d err=%v", table, count, err)
		}
	}
	var staleRestorePreviews int
	if err = s.db.QueryRow(`SELECT count(*) FROM version_restore_previews WHERE connection=?`, old.ID).Scan(&staleRestorePreviews); err != nil || staleRestorePreviews != 0 {
		t.Fatalf("scope edit left version previews bound to the old store: count=%d err=%v", staleRestorePreviews, err)
	}
	s.mu.Lock()
	var after Job
	for _, job := range s.cfg.Jobs {
		if job.ID == jobID {
			after = job
		}
	}
	s.mu.Unlock()
	if after.ID == "" || after.Enabled || after.Status != "paused" || !strings.Contains(after.Detail, "重新预览") {
		t.Fatalf("dependent task was not kept safely paused: %+v", after)
	}
}

func TestUpdateCredentialsRejectsWebDAVUsernameScopeChange(t *testing.T) {
	s, vault, old, _ := setupEditableConnection(t)
	err := s.UpdateCredentials(context.Background(), ConnectionInput{Config: storage.Config{ID: old.ID, Username: "bob"}, Password: "new-password"})
	if err == nil || !strings.Contains(err.Error(), "编辑配置") {
		t.Fatalf("credential-only route accepted a WebDAV scope change: %v", err)
	}
	current, err := s.connection(old.ID)
	if err != nil || current.Username != "alice" {
		t.Fatalf("rejected username change modified config: %+v err=%v", current, err)
	}
	var creds storage.Credentials
	if err = json.Unmarshal([]byte(vault.m["connection:"+old.ID]), &creds); err != nil || creds.Username != "alice" || creds.Password != "old-password" {
		t.Fatalf("rejected username change modified credentials: %+v err=%v", creds, err)
	}
}

func TestConnectionScopeEditBlocksUncleanedSnapshotFromDeletedBackup(t *testing.T) {
	s, _, old, _ := setupEditableConnection(t)
	if err := s.ensureBackupSchema(); err != nil {
		t.Fatal(err)
	}
	jobID := ID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.Exec(`INSERT INTO backup_jobs(id,name,connection,local_path,remote_prefix,exclude_json,schedule_minutes,retain_recent,retain_daily,retain_monthly,enabled,last_run,status,detail,created) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, jobID, "deleted backup", old.ID, t.TempDir(), "backups", "[]", 60, 1, 0, 0, 0, "", "deleted", "任务已删除；历史快照仍保留", now)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := ID()
	_, err = s.db.Exec(`INSERT INTO backup_snapshots(id,job_id,remote_connection,remote_prefix,started,completed,status,files_total,files_done,files_failed,bytes_total,manifest_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, snapshotID, jobID, old.ID, "backups", now, now, "complete", 1, 1, 0, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	in := editInput(old)
	in.Prefix = "new-root"
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token != "" || preview.Impact.Backups != 0 || preview.Impact.BackupSnapshots != 1 || !strings.Contains(strings.Join(preview.Impact.Blockers, " "), "保留中的历史快照") {
		t.Fatalf("retained snapshot from deleted task did not block range change: %+v", preview)
	}
	if _, err = s.db.Exec(`UPDATE backup_snapshots SET status='cleaned' WHERE id=?`, snapshotID); err != nil {
		t.Fatal(err)
	}
	preview, err = s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" || preview.Impact.BackupSnapshots != 0 {
		t.Fatalf("cleaned snapshot still blocked range change: preview=%+v err=%v", preview, err)
	}
}

func TestConnectionScopeEditBlocksEnabledJobBeforeProbing(t *testing.T) {
	s, _, old, dav := setupEditableConnection(t)
	jobID := ID()
	s.mu.Lock()
	s.cfg.Jobs = append(s.cfg.Jobs, Job{ID: jobID, Name: "active", ConnectionID: old.ID, Enabled: true, Status: "idle"})
	s.mu.Unlock()
	in := editInput(old)
	in.Prefix = "new-root"
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token != "" || len(preview.Impact.Blockers) == 0 {
		t.Fatalf("enabled task did not block scope change: %+v", preview)
	}
	dav.mu.Lock()
	count := len(dav.objects)
	dav.mu.Unlock()
	if count != 0 {
		t.Fatalf("blocked edit wrote probe objects: %d", count)
	}
}

func TestConnectionEditProbesThroughLocalGatewayWithoutHoldingWriter(t *testing.T) {
	s, demoID := testService(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	g, secret, err := s.AddGateway(Gateway{Name: "edit-loopback", ConnectionID: demoID, Port: port, Username: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartGateway(g.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.StopGateway(g.ID) })
	connection, err := s.AddConnection(context.Background(), ConnectionInput{Config: storage.Config{Name: "loopback-webdav", Kind: "webdav", Endpoint: g.URL, Username: g.Username}, Password: secret})
	if err != nil {
		t.Fatal(err)
	}
	in := editInput(connection)
	in.Name = "loopback-webdav-renamed"
	runProbe := func(label string, probe func(context.Context) (any, error)) (any, error) {
		t.Helper()
		probeCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		done := make(chan error, 1)
		var result any
		go func() {
			var probeErr error
			result, probeErr = probe(probeCtx)
			done <- probeErr
		}()
		select {
		case probeErr := <-done:
			cancel()
			return result, probeErr
		case <-time.After(2 * time.Second):
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
			return nil, fmt.Errorf("%s through local gateway waited on the shared writer", label)
		}
	}
	previewValue, err := runProbe("preview probe", func(probeCtx context.Context) (any, error) {
		return s.previewConnectionEdit(probeCtx, in)
	})
	if err != nil {
		t.Fatalf("preview through local gateway failed: %v", err)
	}
	preview, ok := previewValue.(connectionEditPreview)
	if !ok || preview.Token == "" {
		t.Fatalf("preview through local gateway did not return a token: %#v", previewValue)
	}
	updatedValue, err := runProbe("apply probe", func(probeCtx context.Context) (any, error) {
		return s.applyConnectionEdit(probeCtx, in, preview.Token)
	})
	if err != nil {
		t.Fatalf("apply probe through local gateway failed: %v", err)
	}
	updated, ok := updatedValue.(Connection)
	if !ok || updated.Name != in.Name {
		t.Fatalf("apply through local gateway returned wrong connection: %#v", updatedValue)
	}
}

func TestWebDAVNameEditIgnoresUnusedS3Settings(t *testing.T) {
	old := storage.Config{Kind: "webdav", Endpoint: "https://example.test/dav", Username: "user"}
	candidate := old
	candidate.Name = "Renamed"
	candidate.Region = "us-east-1"
	if connectionStorageScopeChanged(old, candidate) {
		t.Fatal("unused S3 region changed a WebDAV storage scope")
	}
	candidate.Username = "other"
	if !connectionStorageScopeChanged(old, candidate) {
		t.Fatal("WebDAV account change did not change storage scope")
	}
}

func TestStandardEditDetectsCapabilitiesAndRecoveryPolicyChangeInvalidatesPlans(t *testing.T) {
	s, _, old, dav := setupEditableConnection(t)
	dav.mu.Lock()
	dav.ignoreCreateCondition = true
	dav.mu.Unlock()
	s.mu.Lock()
	s.cfg.Connections[0].WriteRestriction = "条件写入能力未验证"
	jobID := ID()
	s.cfg.Jobs = append(s.cfg.Jobs, Job{ID: jobID, Name: "sync", ConnectionID: old.ID, Enabled: true, Status: "idle"})
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	if err := s.ensureSyncSchema(); err != nil {
		t.Fatal(err)
	}
	if err := ensureConnectionEditSchema(s); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?)`, jobID, "keep.txt", "local-hash", `"remote-etag"`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_queue(job,path,kind,state,updated) VALUES(?,?,?,'pending',?)`, jobID, "keep.txt", "upload", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?)`, jobID, "old-token", "{}", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_dirty(job,token,updated) VALUES(?,?,?)`, jobID, "dirty-token", now); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.plans["old-token"] = Plan{Token: "old-token", JobID: jobID}
	s.mu.Unlock()
	in := editInput(old)
	in.WriteMode = storage.WriteModeStandard
	in.KeepRecovery = true
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" || !preview.PolicyChanged || !preview.RecoveryChanged || preview.WriteModeChanged {
		t.Fatalf("recovery-policy preview incorrect: preview=%+v err=%v", preview, err)
	}
	if preview.CompatibilityProfile != connectionProfileCompatible || !strings.Contains(preview.WriteRestriction, storage.ErrConditionalUnsupported.Error()) {
		t.Fatalf("normal edit did not record the detected conditional-write restriction: %+v", preview)
	}
	dav.mu.Lock()
	probeWrites := dav.next
	dav.mu.Unlock()
	if probeWrites == 0 {
		t.Fatal("normal edit did not automatically probe conditional-write support")
	}
	stale := in
	stale.KeepRecovery = false
	if _, err = s.applyConnectionEdit(context.Background(), stale, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("preview token accepted a changed recovery setting: %v", err)
	}
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WriteMode != storage.WriteModeStandard || !updated.KeepRecovery {
		t.Fatalf("normal mode or recovery setting was not persisted: %+v", updated.Config)
	}
	dav.mu.Lock()
	applyWrites := dav.next
	dav.mu.Unlock()
	if applyWrites != probeWrites {
		t.Fatalf("applying normal edit repeated the full probe: PUT count %d -> %d", probeWrites, applyWrites)
	}
	var raw string
	if err = s.db.QueryRow(`SELECT data FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved config
	if err = json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Connections) != 1 || !saved.Connections[0].KeepRecovery || saved.Connections[0].WriteMode != storage.WriteModeStandard {
		t.Fatalf("normal mode/recovery setting did not persist in config: %+v", saved.Connections)
	}
	for _, table := range []string{"sync_queue", "sync_plans", "sync_dirty"} {
		var count int
		if err = s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE job=?`, jobID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s retained stale plan state count=%d err=%v", table, count, err)
		}
	}
	var baselines int
	if err = s.db.QueryRow(`SELECT count(*) FROM baseline WHERE job=?`, jobID).Scan(&baselines); err != nil || baselines != 1 {
		t.Fatalf("recovery-setting change did not preserve baseline: count=%d err=%v", baselines, err)
	}
	s.mu.Lock()
	var after Job
	for _, job := range s.cfg.Jobs {
		if job.ID == jobID {
			after = job
		}
	}
	_, hasOldPlan := s.plans["old-token"]
	s.mu.Unlock()
	if after.Enabled || after.Status != "paused" || !strings.Contains(after.Detail, "重新预览") || hasOldPlan {
		t.Fatalf("recovery-setting change did not pause and invalidate sync preview state: job=%+v oldPlanRetained=%v", after, hasOldPlan)
	}
}

func TestStandardEditRedetectsCapabilitiesWhenCredentialsChange(t *testing.T) {
	s, _, old, dav := setupEditableConnection(t)
	s.mu.Lock()
	s.cfg.Connections[0].WriteMode = storage.WriteModeStandard
	s.cfg.Connections[0].Capabilities = storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true, RangeRead: true, MultipartConditional: true}
	s.cfg.Connections[0].WriteRestriction = "旧凭据能力限制"
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	in := editInput(old)
	in.WriteMode = storage.WriteModeStandard
	in.Password = "new-password"
	preview, err := s.previewConnectionEdit(context.Background(), in)
	if err != nil || preview.Token == "" {
		t.Fatalf("normal edit with readable new credentials failed: preview=%+v err=%v", preview, err)
	}
	if !preview.Capabilities.ConditionalWrite || !preview.Capabilities.ConditionalDelete || preview.CompatibilityProfile != connectionProfileConditional || preview.WriteRestriction != "" {
		t.Fatalf("normal edit did not detect capabilities using the candidate credentials: %+v", preview)
	}
	dav.mu.Lock()
	probeWrites := dav.next
	dav.mu.Unlock()
	updated, err := s.applyConnectionEdit(context.Background(), in, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Capabilities.ConditionalWrite || !updated.Capabilities.ConditionalDelete || updated.CompatibilityProfile != connectionProfileConditional || updated.WriteRestriction != "" || !connectionDetectionCurrent(updated) {
		t.Fatalf("saved connection did not retain the newly detected capabilities: %+v", updated)
	}
	dav.mu.Lock()
	applyWrites := dav.next
	dav.mu.Unlock()
	if applyWrites != probeWrites {
		t.Fatalf("apply repeated the full conditional probe: PUT count %d -> %d", probeWrites, applyWrites)
	}
}
