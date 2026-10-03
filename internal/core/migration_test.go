package core

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"testing"
	"time"
)

func addMemoryConnection(t *testing.T, s *Service, name string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cfg.Connections) == 0 {
		t.Fatal("missing test connection")
	}
	c := s.cfg.Connections[0]
	c.ID, c.Name = ID(), name
	s.cfg.Connections = append(s.cfg.Connections, c)
	s.stores[c.ID] = storage.NewMemory()
	return c.ID
}

func waitMigration(t *testing.T, s *Service, runID, jobID string) MigrationRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.ListMigrationRuns(jobID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.ID == runID && run.State != "queued" && run.State != "running" && run.State != "cancel_requested" {
				return run
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("migration did not finish before timeout")
	return MigrationRun{}
}

func seedRemoteFile(t *testing.T, b Backend, key, content string) {
	t.Helper()
	if err := ensureRemoteDirectory(context.Background(), b, pathDir(key)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(context.Background(), key, strings.NewReader(content), int64(len(content)), storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
}

func pathDir(key string) string {
	i := strings.LastIndex(key, "/")
	if i < 0 {
		return ""
	}
	return key[:i]
}

func TestMigrationCopiesVerifiesAndCleanupRechecksDestination(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	source := Backend{Service: s, ConnectionID: sourceID}
	target := Backend{Service: s, ConnectionID: targetID}
	seedRemoteFile(t, source, "source/docs/a.txt", "alpha")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "move provider", SourceConnection: sourceID, SourcePrefix: "source", TargetConnection: targetID, TargetPrefix: "destination"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil || len(preview.Items) != 1 || preview.Conflicts != 0 {
		t.Fatalf("preview = %#v, err=%v", preview, err)
	}
	run, err := s.StartMigration(context.Background(), job.ID, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	finished := waitMigration(t, s, run.ID, job.ID)
	if finished.State != "complete" || finished.Done != 1 || finished.Failed != 0 {
		t.Fatalf("migration run = %#v", finished)
	}
	if got := readBody(t, target, "destination/docs/a.txt"); got != "alpha" {
		t.Fatalf("destination = %q", got)
	}
	if got := readBody(t, source, "source/docs/a.txt"); got != "alpha" {
		t.Fatalf("source was removed during copy: %q", got)
	}
	updated := job
	updated.TargetPrefix = "different"
	if _, err = s.UpdateMigrationJob(updated); err == nil {
		t.Fatal("task location changed after a run, invalidating its cleanup history")
	}
	updated = job
	updated.Exclude = []string{"private/**"}
	if _, err = s.UpdateMigrationJob(updated); err == nil {
		t.Fatal("exclude rules changed after a run, invalidating its scan history")
	}
	cleanup, err := s.PreviewMigrationCleanup(context.Background(), run.ID)
	if err != nil || len(cleanup.Items) != 1 {
		t.Fatalf("cleanup preview = %#v, err=%v", cleanup, err)
	}
	entry, err := target.Stat(context.Background(), "destination/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = target.Put(context.Background(), "destination/docs/a.txt", strings.NewReader("external"), 8, storage.Condition{IfMatch: entry.ETag}); err != nil {
		t.Fatal(err)
	}
	if err = s.CleanupMigrationSource(context.Background(), run.ID, cleanup.Token); err == nil {
		t.Fatal("source cleanup accepted a changed destination")
	}
	if got := readBody(t, source, "source/docs/a.txt"); got != "alpha" {
		t.Fatalf("source was removed after destination changed: %q", got)
	}
}

func TestMigrationCleanupDeletesOnlyAfterFreshVerifiedPreview(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	source := Backend{Service: s, ConnectionID: sourceID}
	target := Backend{Service: s, ConnectionID: targetID}
	seedRemoteFile(t, source, "src/item.bin", "verified-by-sha")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "verified cleanup", SourceConnection: sourceID, SourcePrefix: "src", TargetConnection: targetID, TargetPrefix: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartMigration(context.Background(), job.ID, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	finished := waitMigration(t, s, run.ID, job.ID)
	if finished.State != "complete" {
		t.Fatalf("run = %#v", finished)
	}
	cleanup, err := s.PreviewMigrationCleanup(context.Background(), run.ID)
	if err != nil || len(cleanup.Items) != 1 {
		t.Fatalf("preview = %#v, err=%v", cleanup, err)
	}
	if err = s.CleanupMigrationSource(context.Background(), run.ID, cleanup.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Stat(context.Background(), "src/item.bin"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("source remains after confirmed cleanup: %v", err)
	}
	if got := readBody(t, target, "dst/item.bin"); got != "verified-by-sha" {
		t.Fatalf("verified destination missing: %q", got)
	}
}

func TestMigrationStalePlanAndConflictNeverOverwrite(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	source := Backend{Service: s, ConnectionID: sourceID}
	target := Backend{Service: s, ConnectionID: targetID}
	seedRemoteFile(t, source, "src/file", "source")
	seedRemoteFile(t, target, "dst/file", "target")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "conflicts", SourceConnection: sourceID, SourcePrefix: "src", TargetConnection: targetID, TargetPrefix: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil || preview.Conflicts != 1 || preview.Items[0].State != "conflict" {
		t.Fatalf("conflict preview = %#v, err=%v", preview, err)
	}
	if _, err = source.Put(context.Background(), "src/file", strings.NewReader("newer"), 5, storage.Condition{IfMatch: preview.Items[0].SourceETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartMigration(context.Background(), job.ID, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale migration preview was accepted: %v", err)
	}
	if got := readBody(t, target, "dst/file"); got != "target" {
		t.Fatalf("conflicting target changed: %q", got)
	}
}

func TestMigrationUnknownCommitRequiresReconciliationBeforeResume(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	source := Backend{Service: s, ConnectionID: sourceID}
	target := Backend{Service: s, ConnectionID: targetID}
	seedRemoteFile(t, source, "src/file", "payload")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "unknown", SourceConnection: sourceID, SourcePrefix: "src", TargetConnection: targetID, TargetPrefix: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[targetID] = uncertainStore{s.stores[targetID]}
	s.mu.Unlock()
	run, err := s.StartMigration(context.Background(), job.ID, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	finished := waitMigration(t, s, run.ID, job.ID)
	if finished.State != "uncertain" {
		t.Fatalf("unknown destination result was not kept uncertain: %#v", finished)
	}
	if got := readBody(t, source, "src/file"); got != "payload" {
		t.Fatalf("source was removed during uncertain copy: %q", got)
	}
	if _, err = s.ResumeMigration(context.Background(), run.ID); err == nil {
		t.Fatal("resume bypassed the pending core reconciliation")
	}
	pending, err := s.PendingOperations()
	if err != nil || len(pending) == 0 {
		t.Fatalf("pending = %#v, err=%v", pending, err)
	}
	for _, operation := range pending {
		if err = s.ReconcileOperation(context.Background(), operation.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ResumeMigration(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	finished = waitMigration(t, s, run.ID, job.ID)
	if finished.State != "complete" || finished.Done != 1 {
		t.Fatalf("reconciled migration did not complete: %#v", finished)
	}
	if got := readBody(t, target, "dst/file"); got != "payload" {
		t.Fatalf("destination mismatch after reconciliation: %q", got)
	}
}

func TestMigrationRejectsAliasedOverlappingNamespace(t *testing.T) {
	s, sourceID := testService(t)
	s.mu.Lock()
	s.cfg.Connections[0].Kind = "webdav"
	s.cfg.Connections[0].Endpoint = "https://dav.example.test/root"
	s.cfg.Connections[0].Prefix = ""
	alias := s.cfg.Connections[0]
	alias.ID, alias.Name = ID(), "same DAV alias"
	s.cfg.Connections = append(s.cfg.Connections, alias)
	s.stores[alias.ID] = s.stores[sourceID]
	aliasID := alias.ID
	s.mu.Unlock()
	_, err := s.CreateMigrationJob(MigrationJob{Name: "unsafe alias", SourceConnection: sourceID, SourcePrefix: "parent", TargetConnection: aliasID, TargetPrefix: "parent/child"})
	if err == nil {
		t.Fatal("aliased connection overlap accepted")
	}
}

func TestMigrationExcludesManagedBackupNamespaceAndCustomPatterns(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "migration target")
	source := Backend{Service: s, ConnectionID: sourceID}
	seedRemoteFile(t, source, "source/keep.txt", "keep")
	seedRemoteFile(t, source, "source/ignored/private.txt", "ignore")
	raw, err := s.store(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Mkdir(context.Background(), "source/.tamiops-backup"); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Put(context.Background(), "source/.tamiops-backup/manifest.json", strings.NewReader("managed"), int64(len("managed")), storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateMigrationJob(MigrationJob{
		Name: "filtered", SourceConnection: sourceID, SourcePrefix: "source",
		TargetConnection: targetID, TargetPrefix: "target", Exclude: []string{"ignored/**"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Exclude) != 2 || job.Exclude[0] != ".tamiops-backup" {
		t.Fatalf("migration excludes lack reserved backup namespace: %#v", job.Exclude)
	}
	preview, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil || preview.SourceFiles != 1 || len(preview.Items) != 1 || preview.Items[0].SourcePath != "source/keep.txt" {
		t.Fatalf("filtered preview = %#v, err=%v", preview, err)
	}
	run, err := s.StartMigration(context.Background(), job.ID, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if finished := waitMigration(t, s, run.ID, job.ID); finished.State != "complete" || finished.Done != 1 {
		t.Fatalf("filtered run = %#v", finished)
	}
	if got := readBody(t, Backend{Service: s, ConnectionID: targetID}, "target/keep.txt"); got != "keep" {
		t.Fatalf("included file was not copied: %q", got)
	}
}

func TestMigrationRejectsManagedPathsThroughConnectionPrefix(t *testing.T) {
	s, sourceID := testService(t)
	s.mu.Lock()
	base := s.cfg.Connections[0]
	base.Kind = "webdav"
	base.Endpoint = "https://dav.example.test/root"
	base.Prefix = ""
	s.cfg.Connections[0] = base
	alias := base
	alias.ID, alias.Name = ID(), "managed-prefix alias"
	alias.Prefix = "workspace/.tamiops-backup"
	s.cfg.Connections = append(s.cfg.Connections, alias)
	s.stores[alias.ID] = s.stores[sourceID]
	aliasID := alias.ID
	s.mu.Unlock()
	_, err := s.CreateMigrationJob(MigrationJob{
		Name: "unsafe alias", SourceConnection: sourceID, SourcePrefix: "source",
		TargetConnection: aliasID, TargetPrefix: "destination",
	})
	if err == nil {
		t.Fatal("migration accepted a connection prefix aliased into the managed backup namespace")
	}
}

type migrationBlockingOpenStore struct {
	storage.Store
	started chan struct{}
}

type migrationBlockingListStore struct {
	storage.Store
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *migrationBlockingListStore) List(ctx context.Context, key string) ([]storage.Entry, error) {
	var blockErr error
	b.once.Do(func() {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			blockErr = ctx.Err()
		}
	})
	if blockErr != nil {
		return nil, blockErr
	}
	return b.Store.List(ctx, key)
}

func TestStartMigrationSerializesWithConfigUpdate(t *testing.T) {
	s, sourceID := testService(t)
	secondSourceID := addMemoryConnection(t, s, "second source")
	targetID := addMemoryConnection(t, s, "target")
	source := Backend{Service: s, ConnectionID: sourceID}
	seedRemoteFile(t, source, "src/file", "source one")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "serialized start", SourceConnection: sourceID, SourcePrefix: "src", TargetConnection: targetID, TargetPrefix: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}

	s.mu.Lock()
	original := s.stores[sourceID]
	release := make(chan struct{})
	blocked := &migrationBlockingListStore{Store: original, started: make(chan struct{}), release: release}
	s.stores[sourceID] = blocked
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stores[sourceID] = original
		s.mu.Unlock()
	}()

	type startResult struct {
		run MigrationRun
		err error
	}
	started := make(chan startResult, 1)
	go func() {
		run, err := s.StartMigration(context.Background(), job.ID, preview.Token)
		started <- startResult{run, err}
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("start did not enter the fresh source scan")
	}

	updated := job
	updated.SourceConnection = secondSourceID
	updateDone := make(chan error, 1)
	go func() {
		_, err := s.UpdateMigrationJob(updated)
		updateDone <- err
	}()
	select {
	case err := <-updateDone:
		close(release)
		t.Fatalf("config update bypassed the migration job lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	start := <-started
	if start.err != nil {
		t.Fatalf("start migration: %v", start.err)
	}
	if err = <-updateDone; err == nil {
		t.Fatal("migration source changed after a run was committed")
	}
	finished := waitMigration(t, s, start.run.ID, job.ID)
	if finished.State != "complete" {
		t.Fatalf("run = %#v", finished)
	}
	if got := readBody(t, Backend{Service: s, ConnectionID: targetID}, "dst/file"); got != "source one" {
		t.Fatalf("migration did not use the source bound to its preview: %q", got)
	}
}

func TestCancelQueuedMigrationWithoutLiveWorkerIsTerminal(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "orphan queued", SourceConnection: sourceID, TargetConnection: targetID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ensureMigrationSchema(); err != nil {
		t.Fatal(err)
	}
	runID := ID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = s.db.Exec(`INSERT INTO migration_runs(id,job_id,state,preview_token,started,updated,files_total,files_done,files_failed,cancel_requested,detail) VALUES(?,?,'queued','orphan',?,?,0,0,0,0,'')`, runID, job.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if err = s.CancelMigration(runID); err != nil {
		t.Fatal(err)
	}
	run, _, err := s.migrationRunAndJob(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "cancelled" || !run.Cancelled {
		t.Fatalf("queued run without a worker did not become terminally cancelled: %#v", run)
	}
}

func TestMigrationLaunchFailureLeavesResumablePausedRun(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "launch failure", SourceConnection: sourceID, SourcePrefix: "src", TargetConnection: targetID, TargetPrefix: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ensureMigrationSchema(); err != nil {
		t.Fatal(err)
	}
	runID := ID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = s.db.Exec(`INSERT INTO migration_runs(id,job_id,state,preview_token,started,updated,files_total,files_done,files_failed,cancel_requested,detail) VALUES(?,?,'queued','failed-start',?,?,0,0,0,0,'')`, runID, job.ID, now, now); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	err = s.launchMigration(runID)
	if err == nil {
		t.Fatal("start unexpectedly succeeded while the service was closing")
	}
	persisted, _, err := s.migrationRunAndJob(runID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "paused" {
		t.Fatalf("launch failure left a non-resumable state: %#v", persisted)
	}
}

func TestMigrationRejectsRealSourceIntoDemoTarget(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "demo target")
	s.mu.Lock()
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == sourceID {
			s.cfg.Connections[i].Kind = "webdav"
			s.cfg.Connections[i].Endpoint = "https://source.example.test"
		}
	}
	s.mu.Unlock()
	if _, err := s.CreateMigrationJob(MigrationJob{Name: "real to demo", SourceConnection: sourceID, TargetConnection: targetID}); err == nil {
		t.Fatal("migration accepted a real source and ephemeral demo target")
	}
}

func (b migrationBlockingOpenStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	close(b.started)
	<-ctx.Done()
	return nil, storage.Entry{}, ctx.Err()
}

func TestCancelMigrationPersistsCancelledRunAndKeepsSource(t *testing.T) {
	s, sourceID := testService(t)
	targetID := addMemoryConnection(t, s, "target")
	source := Backend{Service: s, ConnectionID: sourceID}
	seedRemoteFile(t, source, "src/file", "payload")
	job, err := s.CreateMigrationJob(MigrationJob{Name: "cancel", SourceConnection: sourceID, SourcePrefix: "src", TargetConnection: targetID, TargetPrefix: "dst"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewMigration(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	original := s.stores[sourceID]
	blocked := migrationBlockingOpenStore{Store: original, started: make(chan struct{})}
	s.stores[sourceID] = blocked
	s.mu.Unlock()
	run, err := s.StartMigration(context.Background(), job.ID, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("migration did not begin reading its source")
	}
	if err = s.CancelMigration(run.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitMigration(t, s, run.ID, job.ID)
	if finished.State != "cancelled" {
		t.Fatalf("cancel state = %#v", finished)
	}
	// Restore the normal reader for this assertion.
	s.mu.Lock()
	s.stores[sourceID] = original
	s.mu.Unlock()
	if got := readBody(t, source, "src/file"); got != "payload" {
		t.Fatalf("cancelled migration lost source: %q", got)
	}
}
