package core

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"testing"
	"time"
)

func createPartialBackupForCleanup(t *testing.T, s *Service, connectionID string) (BackupJob, BackupSnapshot) {
	t.Helper()
	local := t.TempDir()
	for name, body := range map[string]string{"one.txt": "intentionally rejected", "two.txt": "verified snapshot file"} {
		if err := os.WriteFile(filepath.Join(local, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "cleanup partial", ConnectionID: connectionID, LocalPath: local, RemotePrefix: "cleanup-tests", RetainRecent: 3})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.store(connectionID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[connectionID] = failBackupFileStore{base}
	s.mu.Unlock()
	snapshot, err := s.RunBackup(context.Background(), job.ID, preview.Token)
	s.mu.Lock()
	s.stores[connectionID] = base
	s.mu.Unlock()
	if err != nil || snapshot.Status != "partial" || snapshot.FilesDone != 1 || snapshot.FilesFailed != 1 {
		t.Fatalf("partial backup = %#v, err = %v", snapshot, err)
	}
	return job, snapshot
}

func TestBackupCleanupPreviewAndConditionalDelete(t *testing.T) {
	s, connectionID := testService(t)
	_, snapshot := createPartialBackupForCleanup(t, s, connectionID)
	preview, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil || preview.Status != "ready" || len(preview.Objects) != 2 || preview.Bytes <= 0 || preview.Token == "" {
		t.Fatalf("cleanup preview = %#v, err = %v", preview, err)
	}
	if !strings.Contains(strings.Join(cleanupObjectPaths(preview.Objects), ","), "manifest.json") || !strings.Contains(strings.Join(cleanupObjectPaths(preview.Objects), ","), "files/two.txt") {
		t.Fatalf("preview did not list exact remote objects: %#v", preview.Objects)
	}
	result, err := s.RunBackupCleanup(context.Background(), snapshot.ID, preview.Token)
	if err != nil || result.Status != "complete" || len(result.Deleted) != 2 || len(result.Remaining) != 0 {
		t.Fatalf("cleanup result = %#v, err = %v", result, err)
	}
	got, _, err := s.backupSnapshot(snapshot.ID)
	if err != nil || got.Status != "cleaned" {
		t.Fatalf("snapshot status after cleanup = %#v, err = %v", got, err)
	}
}

func cleanupObjectPaths(objects []BackupCleanupObject) []string {
	paths := make([]string, 0, len(objects))
	for _, object := range objects {
		paths = append(paths, object.Path)
	}
	return paths
}

func TestBackupCleanupRejectsActiveAndPreservesCompleteSnapshot(t *testing.T) {
	s, connectionID := testService(t)
	_, partial := createPartialBackupForCleanup(t, s, connectionID)
	if _, err := s.db.Exec("UPDATE backup_snapshots SET status='running' WHERE id=?", partial.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewBackupCleanup(context.Background(), partial.ID); err == nil {
		t.Fatal("active snapshot unexpectedly allowed cleanup preview")
	}
	if _, err := s.db.Exec("UPDATE backup_snapshots SET status='partial' WHERE id=?", partial.ID); err != nil {
		t.Fatal(err)
	}

	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "keep.txt"), []byte("complete remains"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "cleanup complete", ConnectionID: connectionID, LocalPath: local, RemotePrefix: "cleanup-complete", RetainRecent: 2})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil || complete.Status != "complete" {
		t.Fatalf("complete backup = %#v, err = %v", complete, err)
	}
	if _, err = s.PreviewBackupCleanup(context.Background(), complete.ID); err == nil {
		t.Fatal("complete snapshot unexpectedly allowed cleanup")
	}
	key := path.Join(backupSnapshotBase(complete.RemotePrefix, complete.ID), "files", "keep.txt")
	if _, err = (Backend{Service: s, ConnectionID: connectionID}).Stat(context.Background(), key); err != nil {
		t.Fatalf("complete remote object was not retained: %v", err)
	}
}

func TestBackupCleanupRechecksETagAndPreviewExpiry(t *testing.T) {
	s, connectionID := testService(t)
	_, snapshot := createPartialBackupForCleanup(t, s, connectionID)
	preview, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	object := previewObject(t, preview.Objects, "files/two.txt")
	key := path.Join(backupSnapshotBase(snapshot.RemotePrefix, snapshot.ID), object.Path)
	store, _ := s.store(connectionID)
	if _, err = store.Put(context.Background(), key, strings.NewReader("external replacement"), int64(len("external replacement")), storage.Condition{IfMatch: object.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunBackupCleanup(context.Background(), snapshot.ID, preview.Token); err == nil {
		t.Fatal("cleanup accepted an object whose ETag and content changed after preview")
	}
	if _, err = (Backend{Service: s, ConnectionID: connectionID}).Stat(context.Background(), key); err != nil {
		t.Fatalf("changed object was deleted: %v", err)
	}

}

func TestBackupCleanupPreviewExpires(t *testing.T) {
	s, connectionID := testService(t)
	_, snapshot := createPartialBackupForCleanup(t, s, connectionID)
	preview, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE backup_cleanup_plans SET created=? WHERE snapshot_id=?", time.Now().Add(-16*time.Minute).UTC().Format(time.RFC3339Nano), snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunBackupCleanup(context.Background(), snapshot.ID, preview.Token); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("expired cleanup token accepted: %v", err)
	}
}

func TestBackupCleanupRecoversCommittedDeleteWithoutReceipt(t *testing.T) {
	s, connectionID := testService(t)
	_, snapshot := createPartialBackupForCleanup(t, s, connectionID)
	preview, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	file := previewObject(t, preview.Objects, "files/two.txt")
	key := path.Join(backupSnapshotBase(snapshot.RemotePrefix, snapshot.ID), file.Path)
	ctx := withManagedBackup(context.Background())
	if err = (Backend{Service: s, ConnectionID: connectionID}).Delete(ctx, key, storage.Condition{IfMatch: file.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DELETE FROM backup_cleanup_receipts WHERE snapshot_id=?", snapshot.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil || len(recovered.Objects) != 1 || recovered.Objects[0].Path != "manifest.json" {
		t.Fatalf("recovered cleanup preview = %#v, err = %v", recovered, err)
	}
	var receipts int
	if err = s.db.QueryRow("SELECT count(*) FROM backup_cleanup_receipts WHERE snapshot_id=? AND object_path=?", snapshot.ID, key).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("journal did not restore receipt: count=%d err=%v", receipts, err)
	}
}

func TestBackupCleanupRecoversRestartedPlannedRemoteObject(t *testing.T) {
	s, connectionID := testService(t)
	local := t.TempDir()
	body := "committed before process interruption"
	if err := os.WriteFile(filepath.Join(local, "restart.txt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "restart cleanup", ConnectionID: connectionID, LocalPath: local, RemotePrefix: "restart-cleanup", RetainRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil || len(preview.Files) != 1 {
		t.Fatalf("backup preview = %#v, err = %v", preview, err)
	}
	file := preview.Files[0]
	snapshotID, started := ID(), time.Now().UTC().Format(time.RFC3339Nano)
	base := backupSnapshotBase(job.RemotePrefix, snapshotID)
	backend := Backend{Service: s, ConnectionID: connectionID}
	ctx := withManagedBackup(context.Background())
	if err = ensureRemoteDirectory(ctx, backend, base); err != nil {
		t.Fatal(err)
	}
	manifest := backupManifest{Version: backupManifestVersion, Snapshot: snapshotID, JobID: job.ID, Started: started, Status: "running", Files: []BackupFile{file}, Bytes: int64(len(body))}
	if _, err = saveBackupManifest(ctx, backend, path.Join(base, "manifest.json"), manifest, ""); err != nil {
		t.Fatal(err)
	}
	if err = ensureRemoteDirectory(ctx, backend, path.Join(base, "files")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(local, file.Path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.put(ctx, path.Join(base, "files", file.Path), f, file.Size, storage.Condition{IfNoneMatch: true}, file.SHA256)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO backup_snapshots(id,job_id,remote_connection,remote_prefix,started,completed,status,files_total,files_done,files_failed,bytes_total,manifest_version,error)
	 VALUES(?,?,?,?,?,'','partial',1,0,0,?,?,'')`, snapshotID, job.ID, connectionID, job.RemotePrefix, started, file.Size, backupManifestVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO backup_items(snapshot_id,path,sha256,size,remote_etag,state,error,modified) VALUES(?,?,?,?,?,'planned','','')`, snapshotID, file.Path, file.SHA256, file.Size, ""); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.PreviewBackupCleanup(context.Background(), snapshotID)
	if err != nil || len(recovered.Objects) != 2 || previewObject(t, recovered.Objects, "files/restart.txt").Size != file.Size {
		t.Fatalf("planned interrupted snapshot preview = %#v, err = %v", recovered, err)
	}
	result, err := s.RunBackupCleanup(context.Background(), snapshotID, recovered.Token)
	if err != nil || result.Status != "complete" {
		t.Fatalf("interrupted snapshot cleanup = %#v, err = %v", result, err)
	}
}

func TestBackupCleanupMarksFullyReceiptedNoObjectSnapshotCleaned(t *testing.T) {
	s, connectionID := testService(t)
	_, snapshot := createPartialBackupForCleanup(t, s, connectionID)
	preview, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	backend := Backend{Service: s, ConnectionID: connectionID}
	ctx := withManagedBackup(context.Background())
	base := backupSnapshotBase(snapshot.RemotePrefix, snapshot.ID)
	for _, object := range preview.Objects {
		key := path.Join(base, object.Path)
		if err = backend.Delete(ctx, key, storage.Condition{IfMatch: object.ETag}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`INSERT INTO backup_cleanup_receipts(snapshot_id,object_path,etag,size,deleted) VALUES(?,?,?,?,?)`, snapshot.ID, key, object.ETag, object.Size, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := s.PreviewBackupCleanup(context.Background(), snapshot.ID)
	if err != nil || recovered.Status != "cleaned" || recovered.Token != "" || len(recovered.Objects) != 0 {
		t.Fatalf("fully receipted preview = %#v, err = %v", recovered, err)
	}
	stored, _, err := s.backupSnapshot(snapshot.ID)
	if err != nil || stored.Status != "cleaned" {
		t.Fatalf("snapshot status = %#v, err = %v", stored, err)
	}
}

func previewObject(t *testing.T, objects []BackupCleanupObject, key string) BackupCleanupObject {
	t.Helper()
	for _, object := range objects {
		if object.Path == key {
			return object
		}
	}
	t.Fatalf("preview does not contain %q: %#v", key, objects)
	return BackupCleanupObject{}
}
