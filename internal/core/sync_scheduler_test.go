package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tamiops/internal/storage"
	"tamiops/internal/systemstate"
)

type listCountingStore struct {
	storage.Store
	lists atomic.Int32
}

func (s *listCountingStore) List(ctx context.Context, key string) ([]storage.Entry, error) {
	s.lists.Add(1)
	return s.Store.List(ctx, key)
}

type statCountingStore struct {
	storage.Store
	stats atomic.Int32
}

func (s *statCountingStore) Stat(ctx context.Context, key string) (storage.Entry, error) {
	s.stats.Add(1)
	return s.Store.Stat(ctx, key)
}

func replaceStore(t *testing.T, s *Service, id string, replacement storage.Store) {
	t.Helper()
	s.mu.Lock()
	old := s.stores[id]
	s.stores[id] = replacement
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		s.stores[id] = old
		s.mu.Unlock()
	})
}

func TestPreviewReusesUnchangedPersistentPlan(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("same"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	first, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var firstCreated string
	if err = s.db.QueryRow("SELECT created FROM sync_plans WHERE job=?", j.ID).Scan(&firstCreated); err != nil {
		t.Fatal(err)
	}
	second, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var secondCreated string
	if err = s.db.QueryRow("SELECT created FROM sync_plans WHERE job=?", j.ID).Scan(&secondCreated); err != nil {
		t.Fatal(err)
	}
	if first.Token != second.Token || firstCreated != secondCreated {
		t.Fatalf("unchanged preview churned its persistent plan: first=%+v second=%+v created=%q/%q", first, second, firstCreated, secondCreated)
	}
}

func TestWatchTickUsesMetadataAndDoesNotPollRemoteWhenClean(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := syncJob(t, s, id, dir, "upload", Job{Watch: true, ScheduleMinutes: 1})
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	counter := &listCountingStore{Store: base}
	replaceStore(t, s, id, counter)
	if _, err := s.Preview(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	counter.lists.Store(0)
	fingerprint, err := localMetadataFingerprint(context.Background(), dir, j.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	key := schedulerKey{s, j.ID}
	schedulerObservations.Store(key, scheduleObservation{localFingerprint: fingerprint, lastFullScan: time.Now()})
	s.schedulerCycle(context.Background(), false)
	if got := counter.lists.Load(); got != 0 {
		t.Fatalf("clean 2-second watch tick performed %d remote list calls", got)
	}
}

func TestWatchDebouncesMetadataChangeBeforePreview(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := syncJob(t, s, id, dir, "upload", Job{Watch: true})
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	counter := &listCountingStore{Store: base}
	replaceStore(t, s, id, counter)
	oldFingerprint, err := localMetadataFingerprint(context.Background(), dir, j.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	key := schedulerKey{s, j.ID}
	schedulerObservations.Store(key, scheduleObservation{localFingerprint: oldFingerprint, lastFullScan: time.Now()})
	if err = os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), false)
	if got := counter.lists.Load(); got != 0 {
		t.Fatalf("first changed metadata tick skipped debounce and performed %d remote list calls", got)
	}
	stateAny, ok := schedulerObservations.Load(key)
	if !ok || !stateAny.(scheduleObservation).localDirty {
		t.Fatal("changed local metadata was not retained for debounced scan")
	}
	state := stateAny.(scheduleObservation)
	state.changedAt = time.Now().Add(-schedulerDebounce - time.Millisecond)
	schedulerObservations.Store(key, state)
	s.schedulerCycle(context.Background(), false)
	if got := counter.lists.Load(); got == 0 {
		t.Fatal("debounced local change did not trigger a full remote preview")
	}
}

func TestWatchIgnoresChangesInsideExcludedSubtree(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	ignored := filepath.Join(dir, "cache")
	if err := os.Mkdir(ignored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignored, "item.txt"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{Watch: true, Exclude: []string{"cache/**"}})
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	counter := &listCountingStore{Store: base}
	replaceStore(t, s, id, counter)
	fingerprint, err := localMetadataFingerprint(context.Background(), dir, j.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	key := schedulerKey{s, j.ID}
	schedulerObservations.Store(key, scheduleObservation{localFingerprint: fingerprint, lastFullScan: time.Now()})
	if err = os.WriteFile(filepath.Join(ignored, "item.txt"), []byte("after with different length"), 0600); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), false)
	stateAny, ok := schedulerObservations.Load(key)
	if !ok {
		t.Fatal("watcher dropped its observation")
	}
	state := stateAny.(scheduleObservation)
	if state.localDirty || state.localFingerprint != fingerprint {
		t.Fatalf("excluded subtree marked the watch dirty: %+v", state)
	}
	if got := counter.lists.Load(); got != 0 {
		t.Fatalf("excluded subtree change triggered %d remote list calls", got)
	}
}

func TestWatchIgnoresDSStoreChanges(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	metadataDir := filepath.Join(dir, "docs")
	if err := os.Mkdir(metadataDir, 0700); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(metadataDir, ".DS_Store")
	if err := os.WriteFile(metadataPath, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, id, dir, "upload", Job{Watch: true})
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	counter := &listCountingStore{Store: base}
	replaceStore(t, s, id, counter)
	fingerprint, err := localMetadataFingerprint(context.Background(), dir, job.Exclude)
	if err != nil {
		t.Fatal(err)
	}
	key := schedulerKey{s, job.ID}
	schedulerObservations.Store(key, scheduleObservation{localFingerprint: fingerprint, lastFullScan: time.Now()})
	if err = os.WriteFile(metadataPath, []byte("changed metadata size"), 0600); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), false)
	stateAny, ok := schedulerObservations.Load(key)
	if !ok {
		t.Fatal("watcher dropped its observation")
	}
	state := stateAny.(scheduleObservation)
	if state.localDirty || state.localFingerprint != fingerprint {
		t.Fatalf(".DS_Store change marked the watch dirty: %+v", state)
	}
	if got := counter.lists.Load(); got != 0 {
		t.Fatalf(".DS_Store change triggered %d remote list calls", got)
	}
}

func TestSavedWatchPauseAndResumeDriveScheduler(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	j.Watch = true
	if _, err := s.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), true)
	b := Backend{s, id}
	if got := readBody(t, b, "first.txt"); got != "first" {
		t.Fatalf("saved watch setting did not sync the local file: %q", got)
	}
	if err := s.CancelJob(j.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "during-pause.txt"), []byte("paused"), 0600); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), true)
	if _, err := b.Stat(context.Background(), "during-pause.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("paused job wrote a new remote file: %v", err)
	}
	if err := s.ResumeJob(j.ID); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), true)
	if got := readBody(t, b, "during-pause.txt"); got != "paused" {
		t.Fatalf("resumed watch did not sync the pending local change: %q", got)
	}
}

func TestSavedExcludeRuleProtectsExistingMirrorRemotePath(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "mirror-upload", "cache/item.txt")
	b := Backend{s, id}
	remote, err := b.Stat(context.Background(), "cache/item.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(context.Background(), "cache/item.txt", strings.NewReader("remote changed"), 14, storage.Condition{IfMatch: remote.ETag}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "cache", "item.txt"), []byte("local changed"), 0600); err != nil {
		t.Fatal(err)
	}
	j.Exclude = []string{"cache/**"}
	if _, err = s.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.DeleteCount != 0 || len(p.DeletePaths) != 0 {
		t.Fatalf("saved exclusion produced mirror deletions: %+v", p.DeletePaths)
	}
	if a, ok := actionOf(p, "cache/item.txt"); ok && a.Kind != "skip" {
		t.Fatalf("saved exclusion produced an action for its path: %+v", a)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, b, "cache/item.txt"); got != "remote changed" {
		t.Fatalf("saved exclusion changed the protected remote content: %q", got)
	}
	if got := localText(t, dir, "cache/item.txt"); got != "local changed" {
		t.Fatalf("saved exclusion changed the protected local content: %q", got)
	}
}

func TestExcludedOversizedDirectoryIsPrunedBeforeFileHash(t *testing.T) {
	s, id := testService(t)
	preferences := s.Preferences()
	preferences.MaxFileBytes = 1 << 20
	if err := s.SetPreferences(preferences); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ignored := filepath.Join(dir, "node_modules")
	if err := os.Mkdir(ignored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignored, "large.bin"), make([]byte, 2<<20), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	if err := b.Mkdir(context.Background(), "node_modules"); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, "node_modules/old.bin", "keep")
	j := syncJob(t, s, id, dir, "both", Job{Exclude: []string{"node_modules/**"}})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("excluded oversized subtree was still hashed: %v", err)
	}
	for _, action := range p.Actions {
		if action.Path == "node_modules/old.bin" {
			t.Fatalf("excluded remote item was planned for mutation: %+v", action)
		}
	}
}

func TestStopSchedulerReleasesPerServiceObservations(t *testing.T) {
	s, _ := testService(t)
	key := schedulerKey{s, "closed-service-test"}
	schedulerObservations.Store(key, scheduleObservation{lastFullScan: time.Now()})
	s.StartScheduler()
	s.StopScheduler()
	if _, ok := schedulerObservations.Load(key); ok {
		t.Fatal("stopped service remains retained in scheduler observation map")
	}
}

func TestSchedulerWaitsForAutomationRulesWithoutRepeatingWrites(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scheduled.txt"), []byte("manual still works"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{Watch: true})
	prefs := s.Preferences()
	prefs.Rules = ExecutionRules{Enabled: true, Networks: []string{"allowed-network"}}
	if err := s.SetPreferences(prefs); err != nil {
		t.Fatal(err)
	}
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	counter := &listCountingStore{Store: base}
	replaceStore(t, s, id, counter)
	counter.lists.Store(0)
	ctx := systemstate.WithSnapshot(context.Background(), systemstate.Snapshot{NetworkInterfaces: []string{"other-network"}})
	var activityBefore int
	if err = s.db.QueryRow("SELECT count(*) FROM activity").Scan(&activityBefore); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(ctx, true)
	waiting, err := s.job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != "waiting" || waiting.Detail != "当前没有连接允许的活动网络" {
		t.Fatalf("scheduler did not explain its automation wait: %+v", waiting)
	}
	if got := counter.lists.Load(); got != 0 {
		t.Fatalf("scheduler scanned the remote before its automation rule allowed work: %d", got)
	}
	var firstSettings string
	if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&firstSettings); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(ctx, true)
	var secondSettings string
	if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&secondSettings); err != nil {
		t.Fatal(err)
	}
	var activityAfter int
	if err = s.db.QueryRow("SELECT count(*) FROM activity").Scan(&activityAfter); err != nil {
		t.Fatal(err)
	}
	if firstSettings != secondSettings || activityAfter != activityBefore {
		t.Fatalf("repeated blocked scheduler cycles wrote state or activity: settingsEqual=%v activity=%d/%d", firstSettings == secondSettings, activityBefore, activityAfter)
	}
	plan, err := s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(ctx, j.ID, plan.Token); err != nil {
		t.Fatalf("manual execution was blocked by automatic rules: %v", err)
	}
	if got := readBody(t, Backend{s, id}, "scheduled.txt"); got != "manual still works" {
		t.Fatalf("manual execution produced %q", got)
	}
}

func TestSchedulerSuppressesRetriesWhileOperationNeedsReconciliation(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blocked.txt"), []byte("retry after reconciliation"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, id, dir, "upload", Job{ScheduleMinutes: 1})
	c, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	opID := ID()
	_, err = s.beginWithEvidence(c, "blocked.txt", "upload", "", opID, operationReceipt{
		Kind: "upload", DestinationConnection: id, DestinationPath: "blocked.txt", Step: "prepared",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE operations SET state='uncertain' WHERE id=?", opID); err != nil {
		t.Fatal(err)
	}
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	counter := &statCountingStore{Store: base}
	replaceStore(t, s, id, counter)

	s.schedulerCycle(context.Background(), false)
	attention, err := s.job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attention.Status != "needs_attention" || !strings.Contains(attention.Detail, "待核对") {
		t.Fatalf("scheduler did not surface the unresolved operation: %+v", attention)
	}
	firstChecks := counter.stats.Load()
	if firstChecks == 0 {
		t.Fatal("scheduler did not attempt a single operation reconciliation")
	}
	var firstScans, firstActivity int
	if err = s.db.QueryRow("SELECT count(*) FROM sync_scan_history WHERE job=?", job.ID).Scan(&firstScans); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM activity").Scan(&firstActivity); err != nil {
		t.Fatal(err)
	}
	var firstSettings string
	if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&firstSettings); err != nil {
		t.Fatal(err)
	}

	// A forced wake must still respect an unresolved write blocker. It should
	// perform only the lightweight database check, without another remote retry.
	s.schedulerCycle(context.Background(), true)
	if got := counter.stats.Load(); got != firstChecks {
		t.Fatalf("blocked scheduler retried remote reconciliation: stats=%d/%d", firstChecks, got)
	}
	var secondScans, secondActivity int
	if err = s.db.QueryRow("SELECT count(*) FROM sync_scan_history WHERE job=?", job.ID).Scan(&secondScans); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM activity").Scan(&secondActivity); err != nil {
		t.Fatal(err)
	}
	var secondSettings string
	if err = s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&secondSettings); err != nil {
		t.Fatal(err)
	}
	if firstScans != secondScans || firstActivity != secondActivity || firstSettings != secondSettings {
		t.Fatalf("blocked scheduler cycle changed state: scans=%d/%d activity=%d/%d settingsEqual=%v", firstScans, secondScans, firstActivity, secondActivity, firstSettings == secondSettings)
	}

	// Once the operation is resolved, the next cycle clears suppression and
	// performs a fresh plan against the same local change.
	if _, err = s.db.Exec("UPDATE operations SET state='failed' WHERE id=?", opID); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(context.Background(), false)
	if got := readBody(t, Backend{s, id}, "blocked.txt"); got != "retry after reconciliation" {
		t.Fatalf("scheduler did not resume after the blocker cleared: %q", got)
	}
}
