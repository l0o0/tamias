package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"testing"
)

type failBackupFileStore struct{ storage.Store }

type countBackupManifestStore struct {
	storage.Store
	mu    sync.Mutex
	count int
}

func (s *countBackupManifestStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond storage.Condition) (storage.Entry, error) {
	if strings.HasSuffix(key, "/manifest.json") {
		s.mu.Lock()
		s.count++
		s.mu.Unlock()
	}
	return s.Store.Put(ctx, key, body, size, cond)
}

func (s failBackupFileStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond storage.Condition) (storage.Entry, error) {
	if strings.HasSuffix(key, "/files/one.txt") {
		return storage.Entry{}, storage.ErrUnsupported
	}
	return s.Store.Put(ctx, key, body, size, cond)
}

func TestBackupSnapshotRestoreAndRemoteManifest(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.Mkdir(filepath.Join(local, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "nested", "report.txt"), []byte("snapshot-one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "ignored.tmp"), []byte("ignore"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{
		Name: "local archive", ConnectionID: id, LocalPath: local, RemotePrefix: "archives",
		Exclude: []string{"*.tmp"}, RetainRecent: 5, RetainDaily: 7, RetainMonthly: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil || len(preview.Files) != 1 || preview.Files[0].Path != "nested/report.txt" || preview.Excluded != 1 {
		t.Fatalf("preview = %#v, err=%v", preview, err)
	}
	snapshot, err := s.RunBackup(context.Background(), job.ID, preview.Token)
	if err != nil || snapshot.Status != "complete" || snapshot.FilesDone != 1 || snapshot.FilesFailed != 0 {
		t.Fatalf("snapshot = %#v, err=%v", snapshot, err)
	}
	remote, err := s.ListRemoteBackupSnapshots(context.Background(), id, "archives")
	if err != nil || len(remote) != 1 || remote[0].ID != snapshot.ID || remote[0].Status != "complete" {
		t.Fatalf("remote snapshots = %#v, err=%v", remote, err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err = s.RestoreBackupSnapshot(context.Background(), snapshot.ID, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "nested", "report.txt"))
	if err != nil || string(got) != "snapshot-one" {
		t.Fatalf("restored body %q, err=%v", got, err)
	}
	// The source remains untouched, and a restore never writes into an existing directory.
	if source, e := os.ReadFile(filepath.Join(local, "nested", "report.txt")); e != nil || string(source) != "snapshot-one" {
		t.Fatalf("source changed: %q, err=%v", source, e)
	}
	if err = os.WriteFile(filepath.Join(dest, "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RestoreRemoteBackupSnapshot(context.Background(), id, "archives", snapshot.ID, dest); err == nil {
		t.Fatal("restore accepted an existing destination")
	}
	if sentinel, e := os.ReadFile(filepath.Join(dest, "sentinel")); e != nil || string(sentinel) != "keep" {
		t.Fatalf("existing destination changed: %q, err=%v", sentinel, e)
	}
}

func TestBackupRestoreRejectsInvalidSnapshotIDs(t *testing.T) {
	s, id := testService(t)
	for _, snapshotID := range []string{".", "..", "../outside", "not-a-snapshot"} {
		if err := s.RestoreRemoteBackupSnapshot(context.Background(), id, "archives", snapshotID, filepath.Join(t.TempDir(), "restore")); err == nil {
			t.Fatalf("restore accepted invalid snapshot id %q", snapshotID)
		}
	}
}

func TestBackupRestoreRemovesPartialDestinationOnVerificationFailure(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "restore verify", ConnectionID: id, LocalPath: local, RemotePrefix: "restore-verify", RetainRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	key := "restore-verify/.tamiops-backup/" + snapshot.ID + "/files/file.txt"
	entry, err := st.Stat(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Put(context.Background(), key, strings.NewReader("tampered"), int64(len("tampered")), storage.Condition{IfMatch: entry.ETag}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restore")
	if err = s.RestoreBackupSnapshot(context.Background(), snapshot.ID, dest); err == nil {
		t.Fatal("restore accepted tampered content")
	}
	if _, err = os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore left a partial destination: stat err=%v", err)
	}
}

func TestRunDueBackupsPersistsPreviewFailure(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "note.txt"), []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{
		Name: "scheduled", ConnectionID: id, LocalPath: local, RemotePrefix: "scheduled",
		ScheduleMinutes: 1, Enabled: true, RetainRecent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(local); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunDueBackups(context.Background()); err == nil {
		t.Fatal("scheduled run hid its preview failure")
	}
	updated, err := s.backupJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastRun == "" || updated.Status != "error" || updated.Detail == "" {
		t.Fatalf("failed attempt was not persisted: %#v", updated)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind='backup' AND status='error' AND path=?`, job.ID).Scan(&count); err != nil || count == 0 {
		t.Fatalf("failed attempt did not create activity: count=%d err=%v", count, err)
	}
}

func TestBackupWritesRemoteManifestOnlyAtStartAndFinish(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	for i := 0; i < 65; i++ {
		name := filepath.Join(local, fmt.Sprintf("file-%02d.txt", i))
		if err := os.WriteFile(name, []byte("value"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "manifest count", ConnectionID: id, LocalPath: local, RemotePrefix: "manifest-count", RetainRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	store := &countBackupManifestStore{Store: s.stores[id]}
	s.mu.Lock()
	s.stores[id] = store
	s.mu.Unlock()
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunBackup(context.Background(), job.ID, plan.Token); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	count := store.count
	store.mu.Unlock()
	if count != 2 {
		t.Fatalf("wrote remote manifest %d times; want initial and final only", count)
	}
}

func TestBackupPreviewRejectsChangedSourceAndRetentionPreservesLatest(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	file := filepath.Join(local, "note.txt")
	if err := os.WriteFile(file, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "retain", ConnectionID: id, LocalPath: local, RemotePrefix: "history", RetainRecent: 1, RetainDaily: 0, RetainMonthly: 0})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunBackup(context.Background(), job.ID, plan.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("changed source accepted: %v", err)
	}
	plan, err = s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil || first.Status != "complete" {
		t.Fatalf("first = %#v, err=%v", first, err)
	}
	if err = os.WriteFile(file, []byte("third"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err = s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil || second.Status != "complete" {
		t.Fatalf("second = %#v, err=%v", second, err)
	}
	remote, err := s.ListRemoteBackupSnapshots(context.Background(), id, "history")
	if err != nil || len(remote) != 1 || remote[0].ID != second.ID {
		t.Fatalf("retention remote list = %#v, err=%v", remote, err)
	}
	localSnapshots, err := s.ListBackupSnapshots(job.ID)
	if err != nil || len(localSnapshots) != 1 || localSnapshots[0].ID != second.ID {
		t.Fatalf("retention local list = %#v, err=%v", localSnapshots, err)
	}
}

func TestBackupPartialSnapshotDoesNotClaimSuccess(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "one.txt"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "partial", ConnectionID: id, LocalPath: local, RemotePrefix: "partial-backup"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(local, "one.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if !errors.Is(err, storage.ErrConflict) || snapshot.Status != "" {
		// Source changes before Run are rejected without creating a snapshot.
		t.Fatalf("stale preview result = %#v, err=%v", snapshot, err)
	}
	plan, err = s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[id] = failBackupFileStore{s.stores[id]}
	s.mu.Unlock()
	snapshot, err = s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil || snapshot.Status != "partial" || snapshot.FilesDone != 0 || snapshot.FilesFailed != 1 {
		t.Fatalf("partial snapshot = %#v, err=%v", snapshot, err)
	}
	if got := readBody(t, Backend{s, id}, "partial-backup/.tamiops-backup/"+snapshot.ID+"/manifest.json"); !strings.Contains(got, `"status":"partial"`) {
		t.Fatalf("remote manifest did not retain partial status: %s", got)
	}
}

func TestBackupSnapshotKeepsOriginalRemoteLocation(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("old-location"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "location", ConnectionID: id, LocalPath: local, RemotePrefix: "vault-old"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil || first.Status != "complete" {
		t.Fatalf("first snapshot = %#v, err=%v", first, err)
	}
	job.RemotePrefix = "vault-new"
	if _, err = s.UpdateBackupJob(job); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restore-old")
	if err = s.RestoreBackupSnapshot(context.Background(), first.ID, dest); err != nil {
		t.Fatalf("snapshot followed edited job instead of its stored location: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "file.txt"))
	if err != nil || string(got) != "old-location" {
		t.Fatalf("old snapshot restore %q, err=%v", got, err)
	}
}

func TestBackupJobCRUDSoftDeletePreservesSnapshotHistory(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "x"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "remove", ConnectionID: id, LocalPath: local, RemotePrefix: "remove-me"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteBackupJob(job.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListBackupJobs()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs after soft delete = %#v, err=%v", jobs, err)
	}
	if _, err = s.backupJob(job.ID); err != nil {
		t.Fatal("soft deletion discarded backup history config", err)
	}
	if !strings.Contains(job.RemotePrefix, "remove-me") {
		t.Fatal("test setup")
	}
}
