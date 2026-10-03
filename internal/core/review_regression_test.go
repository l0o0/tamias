package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"testing"
	"time"
)

func TestManagedBackupTreeCannotBeDeletedOrCopied(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	owned := withManagedBackup(ctx)
	if err := b.Mkdir(ctx, "archives"); err != nil {
		t.Fatal(err)
	}
	if err := b.Mkdir(owned, "archives/.tamiops-backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(owned, "archives/.tamiops-backup/manifest", strings.NewReader("history"), 7, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "archives/user.txt", strings.NewReader("user"), 4, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteTree(ctx, "archives"); err == nil {
		t.Fatal("deleted protected tree")
	}
	if _, err := b.Stat(ctx, "archives/user.txt"); err != nil {
		t.Fatal("partial deletion before protection", err)
	}
	if _, err := b.Copy(ctx, "archives", "copy", false); err == nil {
		t.Fatal("copied reserved metadata")
	}
	if _, err := b.Stat(ctx, "copy"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("destination was created", err)
	}
	if _, err := b.Put(ctx, "archives/.tamiops-backup/new", strings.NewReader("x"), 1, storage.Condition{}); err == nil {
		t.Fatal("wrote reserved path")
	}
}

func TestStagingAdmissionAccountsForRemainingReservations(t *testing.T) {
	s, _ := testService(t)
	p := s.Preferences()
	p.StagingBytes = 16 << 20
	p.MaxFileBytes = 16 << 20
	if err := s.SetPreferences(p); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "reserve-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer os.Remove(f.Name())
	w, release, err := s.reserveStaging(f, 12<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = w.Write(make([]byte, 2<<20)); err != nil {
		t.Fatal(err)
	}
	g, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "reserve-")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	defer os.Remove(g.Name())
	if _, rel, err := s.reserveStaging(g, 5<<20); err == nil {
		rel()
		t.Fatal("overcommitted staging budget")
	}
	_, rel, err := s.reserveStaging(g, 4<<20)
	if err != nil {
		t.Fatal("double-counted written bytes", err)
	}
	rel()
}

type gatedReader struct {
	started chan struct{}
	resume  chan struct{}
	done    bool
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	close(r.started)
	<-r.resume
	r.done = true
	p[0] = 'x'
	return 1, nil
}
func TestSlowUploadReceptionDoesNotHoldCommitLock(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	r := &gatedReader{started: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, e := b.Put(context.Background(), "slow", r, 1, storage.Condition{}); done <- e }()
	<-r.started
	mkdir := make(chan error, 1)
	go func() { mkdir <- b.Mkdir(context.Background(), "independent") }()
	select {
	case err := <-mkdir:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(r.resume)
		<-done
		t.Fatal("slow request blocked unrelated commit")
	}
	close(r.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestPinnedCacheExplicitRefreshRetainsPreviousContent(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	first, err := b.Put(ctx, "version", strings.NewReader("before"), 6, storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.FetchCache(ctx, id, "version", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(ctx, "version", strings.NewReader("after!"), 6, storage.Condition{IfMatch: first.ETag}); err != nil {
		t.Fatal(err)
	}
	stale, err := s.FetchCache(ctx, id, "version", true, false)
	if err != nil || stale.ID != old.ID || !stale.RemoteChanged {
		t.Fatal(stale, err)
	}
	next, err := s.fetchCache(ctx, id, "version", true, false, true)
	if err != nil || next.ID == old.ID || !next.Pinned {
		t.Fatal(next, err)
	}
	data, err := os.ReadFile(next.LocalPath)
	if err != nil || string(data) != "after!" {
		t.Fatal(string(data), err)
	}
	entries, err := s.recoveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected overwrite and refresh recovery copies, got %d", len(entries))
	}
}
func TestFailedCacheRefreshDoesNotEvictPreviousVersion(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	p := s.Preferences()
	p.CacheBytes = 16 << 20
	if err := s.SetPreferences(p); err != nil {
		t.Fatal(err)
	}
	data := strings.Repeat("a", 9<<20)
	first, err := b.Put(ctx, "large", strings.NewReader(data), int64(len(data)), storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.FetchCache(ctx, id, "large", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(ctx, "large", strings.NewReader(strings.Repeat("b", len(data))), int64(len(data)), storage.Condition{IfMatch: first.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FetchCache(ctx, id, "large", false, false); err == nil {
		t.Fatal("expected protected cache budget rejection")
	}
	h, _, err := hashPlain(old.LocalPath)
	if err != nil || h != old.Hash {
		t.Fatal("previous offline copy lost", err)
	}
}
func TestUnknownMkdirCanBeExplicitlyAdoptedWithoutMutation(t *testing.T) {
	s, id := testService(t)
	c, _ := s.connection(id)
	op := ID()
	ev := operationReceipt{Kind: "mkdir", DestinationConnection: id, DestinationPath: "uncertain", DestinationAbsent: true}
	if _, err := s.beginWithEvidence(c, "uncertain", "mkdir", "", op, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE operations SET state='uncertain' WHERE id=?", op); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewOperationAdoption(context.Background(), op)
	if err != nil || preview.Exists {
		t.Fatal(preview, err)
	}
	if err = s.AdoptOperation(context.Background(), op, "stale"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if err = s.AdoptOperation(context.Background(), op, preview.Token); err != nil {
		t.Fatal(err)
	}
	if err = (Backend{s, id}).Mkdir(context.Background(), "uncertain"); err != nil {
		t.Fatal("write not unblocked", err)
	}
}
func TestConfigurationImportAllDefinitionsIsAtomic(t *testing.T) {
	s, _ := testService(t)
	in := ConfigurationExport{Version: 1, Connections: []storage.Config{{ID: "a", Kind: "s3", Name: "origin", Endpoint: "https://example.test", Bucket: "bucket"}}, BackupJobs: []BackupJob{{Name: "backup", ConnectionID: "a", RemotePrefix: "archive", RetainRecent: 2, LocalPath: "/private/root"}}, MigrationJobs: []MigrationJob{{Name: "copy", SourceConnection: "a", TargetConnection: "a", SourcePrefix: "from", TargetPrefix: "to"}}}
	if err := s.ensureMigrationSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_import BEFORE INSERT ON migration_jobs BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := len(s.cfg.Connections)
	if _, err := s.ImportConfiguration(in); err == nil {
		t.Fatal("ignored database failure")
	}
	if len(s.cfg.Connections) != before {
		t.Fatal("in-memory configuration partially committed")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM backup_jobs").Scan(&n); err != nil || n != 0 {
		t.Fatal("backup insert was not rolled back", n, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_import"); err != nil {
		t.Fatal(err)
	}
	counts, err := s.ImportConfiguration(in)
	if err != nil || counts["backupJobs"] != 1 || counts["migrationJobs"] != 1 {
		t.Fatal(counts, err)
	}
	out, err := s.ExportConfiguration()
	if err != nil || len(out.BackupJobs) != 1 || len(out.MigrationJobs) != 1 {
		t.Fatal(out, err)
	}
	if out.BackupJobs[0].LocalPath != "" || out.BackupJobs[0].Enabled || out.MigrationJobs[0].Enabled {
		t.Fatal("unsafe imported task state")
	}
}

func TestLockOwnerCanMoveAndDeleteAcrossCommitContext(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	if _, err := b.Put(ctx, "locked", strings.NewReader("data"), 4, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	token, _, err := b.AcquireDAVLock(ctx, "locked", "owner", false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authenticated := storage.WithLockTokens(ctx, []string{token})
	if _, err = b.Move(authenticated, "locked", "moved", false); err != nil {
		t.Fatal("lock value lost in detached move", err)
	}
	e, err := b.Stat(ctx, "moved")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := b.AcquireDAVLock(ctx, "moved", "owner", false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Delete(storage.WithLockTokens(ctx, []string{tok}), "moved", storage.Condition{IfMatch: e.ETag}); err != nil {
		t.Fatal(err)
	}
}

func TestNewBackupRespectsDisabledSchedule(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateBackupJob(BackupJob{Name: "paused", ConnectionID: id, LocalPath: dir, RemotePrefix: "paused-backup", ScheduleMinutes: 1, Enabled: false, RetainRecent: 1})
	if err != nil || j.Enabled {
		t.Fatal(j, err)
	}
	results, err := s.RunDueBackups(context.Background())
	if err != nil || len(results) != 0 {
		t.Fatal("disabled backup ran", results, err)
	}
}

func TestRetiredCachePreservesEditorWritesAcrossRestart(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	remote, err := b.Put(ctx, "editing", strings.NewReader("before"), 6, storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := s.FetchCache(ctx, id, "editing", true, false)
	if err != nil {
		t.Fatal(err)
	}
	editor, err := openCacheEditor(cached.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Close()
	if _, err = b.Put(ctx, "editing", strings.NewReader("remote"), 6, storage.Condition{IfMatch: remote.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.fetchCache(ctx, id, "editing", true, false, true); err != nil {
		t.Fatal(err)
	}
	if _, err = editor.WriteAt([]byte("edited later"), 0); err != nil {
		t.Fatal(err)
	}
	if err = editor.Sync(); err != nil {
		t.Fatal(err)
	}
	entries, err := s.recoveries()
	if err != nil {
		t.Fatal(err)
	}
	retired := ""
	for _, e := range entries {
		if e.State == "cache-retired" {
			retired = e.ID
		}
	}
	if retired == "" {
		t.Fatal("missing retired cache")
	}
	if _, err = s.PruneRecoveries(time.Now().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(s.dir, s.vault)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	dest := filepath.Join(t.TempDir(), "rescued")
	if err = restarted.saveRecovery(retired, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "edited later" {
		t.Fatal(string(got), err)
	}
}

type recursiveDeleteTrap struct {
	storage.Store
	directoryDeletes int
}

func (t *recursiveDeleteTrap) Delete(ctx context.Context, key string, c storage.Condition) error {
	e, err := t.Store.Stat(ctx, key)
	if err == nil && e.IsDir {
		t.directoryDeletes++
	}
	return t.Store.Delete(ctx, key, c)
}
func TestDirectoryCleanupRequiresNonRecursivePrimitive(t *testing.T) {
	s, id := testService(t)
	base, _ := s.store(id)
	if err := base.Mkdir(context.Background(), "empty"); err != nil {
		t.Fatal(err)
	}
	trap := &recursiveDeleteTrap{Store: base}
	s.mu.Lock()
	s.stores[id] = trap
	s.mu.Unlock()
	if err := (Backend{s, id}).DeleteTree(context.Background(), "empty"); err == nil {
		t.Fatal("unproven directory deletion accepted")
	}
	if trap.directoryDeletes != 0 {
		t.Fatal("unsafe collection DELETE was sent")
	}
	if _, err := base.Stat(context.Background(), "empty"); err != nil {
		t.Fatal(err)
	}
}
