package core

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tamiops/internal/storage"
)

type ignoredConditionsNoPutETagStore struct{ storage.Store }

func (s ignoredConditionsNoPutETagStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, _ storage.Condition) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, body, size, storage.Condition{})
	entry.ETag = ""
	return entry, err
}

type unknownOpenSizeStore struct{ storage.Store }

func (s unknownOpenSizeStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	body, entry, err := s.Store.Open(ctx, key, etag)
	entry.Size = -1
	return body, entry, err
}

func setSyncConnectionPolicy(t *testing.T, s *Service, id, mode string, capabilities storage.Capabilities) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID != id {
			continue
		}
		s.cfg.Connections[i].WriteMode = mode
		s.cfg.Connections[i].Tested = true
		s.cfg.Connections[i].Capabilities = capabilities
		s.cfg.Connections[i].Error = ""
		return
	}
	t.Fatalf("connection %q not found", id)
}

func TestCompatibleSyncUploadVerifiesMissingPutETag(t *testing.T) {
	s, id := testService(t)
	s.mu.Lock()
	store := s.stores[id]
	s.stores[id] = ignoredConditionsNoPutETagStore{Store: store}
	s.mu.Unlock()
	setSyncConnectionPolicy(t, s, id, storage.WriteModeCompatible, storage.Capabilities{RangeRead: true})

	local := t.TempDir()
	if err := os.Mkdir(filepath.Join(local, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "nested", "item.txt"), []byte("verified compatible upload"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, id, local, "upload", Job{})
	plan, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := actionOf(plan, "nested"); !ok {
		t.Fatal("preview omitted the remote directory action")
	}
	if _, err = s.RunPlan(context.Background(), job.ID, plan.Token); err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, Backend{Service: s, ConnectionID: id}, "nested/item.txt"); got != "verified compatible upload" {
		t.Fatalf("uploaded content = %q", got)
	}
	var localHash, remoteETag string
	if err = s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", job.ID, "nested/item.txt").Scan(&localHash, &remoteETag); err != nil {
		t.Fatal(err)
	}
	if localHash == "" || !strongTag(remoteETag) {
		t.Fatalf("compatible upload did not save verified baseline: hash=%q etag=%q", localHash, remoteETag)
	}
}

func TestCopyModeRejectsSyncUploadBeforePlanMutation(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "new.txt"), []byte("copy mode"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, id, local, "upload", Job{})
	setSyncConnectionPolicy(t, s, id, storage.WriteModeCopy, storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true})

	if _, err := s.Preview(context.Background(), job.ID); err == nil || !strings.Contains(err.Error(), "远端写入") {
		t.Fatalf("copy mode preview error = %v", err)
	}
	var plans, baselines, queued int
	if err := s.db.QueryRow("SELECT count(*) FROM sync_plans WHERE job=?", job.ID).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=?", job.ID).Scan(&baselines); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM sync_queue WHERE job=?", job.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if plans != 0 || baselines != 0 || queued != 0 {
		t.Fatalf("rejected preview mutated sync state: plans=%d baselines=%d queue=%d", plans, baselines, queued)
	}
}

func TestSyncRemoteDeletePlanRechecksCapabilityBeforeConsumption(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "old.txt"), []byte("same revision"), 0600); err != nil {
		t.Fatal(err)
	}
	job := makeBaseline(t, s, id, local, "mirror-upload", "old.txt")
	if err := os.Remove(filepath.Join(local, "old.txt")); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	action, ok := actionOf(plan, "old.txt")
	if !ok || action.Kind != "delete-remote" {
		t.Fatalf("expected remote delete action, got %+v", action)
	}
	setSyncConnectionPolicy(t, s, id, storage.WriteModeCompatible, storage.Capabilities{ConditionalWrite: true, RangeRead: true})

	if _, err = s.RunPlan(context.Background(), job.ID, plan.Token, true); err == nil || !strings.Contains(err.Error(), "条件删除") {
		t.Fatalf("run rejected without a conditional-delete capability: %v", err)
	}
	if _, err = (Backend{Service: s, ConnectionID: id}).Stat(context.Background(), "old.txt"); err != nil {
		t.Fatalf("remote object was deleted: %v", err)
	}
	var token string
	if err = s.db.QueryRow("SELECT token FROM sync_plans WHERE job=?", job.ID).Scan(&token); err != nil || token != plan.Token {
		t.Fatalf("rejected run consumed its saved plan: token=%q err=%v", token, err)
	}
	var queued int
	if err = s.db.QueryRow("SELECT count(*) FROM sync_queue WHERE job=? AND state IN ('pending','running')", job.ID).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("rejected run changed the action queue: pending=%d err=%v", queued, err)
	}
}

func TestDownloadOnlySyncWorksInCopyAndStrictReadOnlyModes(t *testing.T) {
	for _, mode := range []string{storage.WriteModeCopy, storage.WriteModeStrict} {
		t.Run(mode, func(t *testing.T) {
			s, id := testService(t)
			seedRemote(t, Backend{Service: s, ConnectionID: id}, "remote.txt", "read from readonly connection")
			local := t.TempDir()
			job := syncJob(t, s, id, local, "download", Job{})
			setSyncConnectionPolicy(t, s, id, mode, storage.Capabilities{RangeRead: true})

			plan, err := s.Preview(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action, ok := actionOf(plan, "remote.txt"); !ok || action.Kind != "download" {
				t.Fatalf("preview action = %+v", action)
			}
			if _, err = s.RunPlan(context.Background(), job.ID, plan.Token); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(local, "remote.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "read from readonly connection" {
				t.Fatalf("downloaded content = %q", data)
			}
		})
	}
}

func TestSyncRemoteHashUsesStatSizeForChunkedOpen(t *testing.T) {
	s, id := testService(t)
	entry := seedRemote(t, Backend{Service: s, ConnectionID: id}, "chunked.txt", "chunked response without a length")
	s.mu.Lock()
	s.stores[id] = unknownOpenSizeStore{Store: s.stores[id]}
	s.mu.Unlock()

	hashed, opened, err := hashRemote(context.Background(), Backend{Service: s, ConnectionID: id}, "chunked.txt", entry.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if hashed.Size != int64(len("chunked response without a length")) || opened.Size != hashed.Size {
		t.Fatalf("hash result size=%d entry size=%d", hashed.Size, opened.Size)
	}
}

func TestCopyModeRejectsKeepBothBeforeCreatingLocalCopy(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	job := makeBaseline(t, s, id, local, "both", "file.txt")
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("local revision"), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{Service: s, ConnectionID: id}
	before, err := b.Stat(context.Background(), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(context.Background(), "file.txt", strings.NewReader("remote revision"), int64(len("remote revision")), storage.Condition{IfMatch: before.ETag}); err != nil {
		t.Fatal(err)
	}
	setSyncConnectionPolicy(t, s, id, storage.WriteModeCopy, storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true})

	if _, err = s.ResolveConflict(context.Background(), job.ID, "file.txt", "keep-both"); err == nil || !strings.Contains(err.Error(), "远端写入") {
		t.Fatalf("keep-both error = %v", err)
	}
	copyPath := conflictCopyPath("file.txt", "remote")
	if _, err = os.Stat(filepath.Join(local, filepath.FromSlash(copyPath))); !os.IsNotExist(err) {
		t.Fatalf("copy mode created local conflict copy %q: %v", copyPath, err)
	}
	if got, err := os.ReadFile(filepath.Join(local, "file.txt")); err != nil || string(got) != "local revision" {
		t.Fatalf("local source changed: content=%q err=%v", got, err)
	}
}

func TestCopyModeDoesNotEnableBackupMigrationOrWritableGateway(t *testing.T) {
	s, id := testService(t)
	setSyncConnectionPolicy(t, s, id, storage.WriteModeCopy, storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true})

	if _, err := s.CreateBackupJob(BackupJob{
		Name:         "backup",
		ConnectionID: id,
		LocalPath:    t.TempDir(),
		RemotePrefix: "backup-root",
		RetainRecent: 1,
	}); err == nil {
		t.Fatal("copy mode enabled a backup job")
	}
	if _, err := s.CreateMigrationJob(MigrationJob{
		Name:             "migration",
		SourceConnection: id,
		SourcePrefix:     "source",
		TargetConnection: id,
		TargetPrefix:     "target",
	}); err == nil {
		t.Fatal("copy mode enabled a migration job")
	}
	if _, _, err := s.AddGatewayContext(context.Background(), Gateway{
		Name:         "write gateway",
		ConnectionID: id,
	}); err == nil {
		t.Fatal("copy mode enabled a writable gateway")
	}
}
