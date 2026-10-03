package core

import (
	"context"
	"os"
	"path/filepath"
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
