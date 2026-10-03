package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tamiops/internal/gateway"
	"tamiops/internal/storage"
)

func TestGatewayMutationInvalidatesCacheAndRechecksManualJob(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	first := seedRemote(t, b, "gateway.txt", "first")
	cache, err := s.FetchCache(ctx, id, "gateway.txt", true, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	j := syncJob(t, s, id, dir, "download", Job{})
	handler := gateway.NewHandler(gateway.Config{Username: "test", Password: "secret"}, b)
	req := httptest.NewRequest(http.MethodPut, "http://localhost/gateway.txt", strings.NewReader("after"))
	req.SetBasicAuth("test", "secret")
	req.Header.Set("If-Match", first.ETag)
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, req)
	if result.Code < 200 || result.Code >= 300 {
		t.Fatalf("gateway PUT %d: %s", result.Code, result.Body.String())
	}
	committed, err := b.Stat(ctx, "gateway.txt")
	if err != nil || result.Header().Get("ETag") != committed.ETag || committed.ETag == first.ETag {
		t.Fatalf("PUT response lost committed version: %s %v", result.Header().Get("ETag"), err)
	}
	entries, err := s.CacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].RemoteChanged || entries[0].Checked != "" {
		t.Fatalf("stale cache not marked: %+v", entries)
	}
	bytes, err := os.ReadFile(cache.LocalPath)
	if err != nil || string(bytes) != "first" {
		t.Fatal("remote mutation replaced pinned local file", err)
	}
	if s.remoteChangeToken(j.ID) == "" {
		t.Fatal("missing durable recheck")
	}
	s.schedulerCycle(ctx, false)
	updated, err := s.job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "needs_attention" {
		t.Fatalf("manual job status=%s detail=%s", updated.Status, updated.Detail)
	}
	if _, err = os.Stat(filepath.Join(dir, "gateway.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("manual job auto-wrote local file", err)
	}
	if s.remoteChangeToken(j.ID) != "" {
		t.Fatal("completed recheck left dirty token")
	}
}

func TestRemoteChangesRespectAliasesScopeAndLatestToken(t *testing.T) {
	s, id := testService(t)
	s.mu.Lock()
	original := s.cfg.Connections[0]
	original.Kind = "s3"
	original.Endpoint = "https://storage.example.test"
	original.Bucket = "bucket"
	original.Prefix = "notes"
	original.ID = id
	alias := original
	alias.ID = "alias"
	alias.Prefix = ""
	s.cfg.Connections = []Connection{original, alias}
	s.cfg.Jobs = []Job{{ID: "same", ConnectionID: alias.ID, RemotePath: "notes"}, {ID: "unrelated", ConnectionID: alias.ID, RemotePath: "notes2"}}
	s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO cache_entries(id,connection,path) VALUES('hit','alias','notes/a.txt'),('miss','alias','notes2/a.txt')")
	if err != nil {
		t.Fatal(err)
	}
	commit := func() {
		s.writes.Lock()
		defer s.writes.Unlock()
		op, err := s.beginWithEvidence(original, "a.txt", "upload", "", ID(), operationReceipt{Kind: "upload", DestinationConnection: id, DestinationPath: "a.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.finish(op, "upload", "a.txt", storage.Entry{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	commit()
	first := s.remoteChangeToken("same")
	if first == "" || s.remoteChangeToken("unrelated") != "" {
		t.Fatal("alias/sibling scope mismatch")
	}
	var hit, miss bool
	if err = s.db.QueryRow("SELECT remote_changed FROM cache_entries WHERE id='hit'").Scan(&hit); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT remote_changed FROM cache_entries WHERE id='miss'").Scan(&miss); err != nil {
		t.Fatal(err)
	}
	if !hit || miss {
		t.Fatal("cache invalidation crossed a sibling prefix")
	}
	commit()
	s.acknowledgeRemoteChange("same", first)
	if s.remoteChangeToken("same") == "" || s.remoteChangeToken("same") == first {
		t.Fatal("new mutation was lost by stale acknowledgement")
	}
}

func TestPausedJobRetainsRemoteRecheckAndDefiniteFailureDoesNotNotify(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	first := seedRemote(t, b, "paused.txt", "first")
	j := syncJob(t, s, id, t.TempDir(), "download", Job{})
	if err := s.CancelJob(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "paused.txt", strings.NewReader("after"), 5, storage.Condition{IfMatch: first.ETag}); err != nil {
		t.Fatal(err)
	}
	token := s.remoteChangeToken(j.ID)
	if token == "" {
		t.Fatal("paused job lost recheck")
	}
	s.schedulerCycle(ctx, false)
	after, _ := s.job(j.ID)
	if after.Enabled || after.Status != "paused" || s.remoteChangeToken(j.ID) != token {
		t.Fatalf("paused job changed: %+v", after)
	}
	if _, err := b.Put(ctx, "paused.txt", strings.NewReader("bad"), 3, storage.Condition{IfMatch: first.ETag}); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if s.remoteChangeToken(j.ID) != token {
		t.Fatal("rejected write emitted mutation")
	}
}

type cacheBlockedOpen struct {
	storage.Store
	ready, release chan struct{}
}

func (b *cacheBlockedOpen) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	r, e, err := b.Store.Open(ctx, key, etag)
	if err != nil {
		return nil, e, err
	}
	close(b.ready)
	select {
	case <-b.release:
		return r, e, nil
	case <-ctx.Done():
		r.Close()
		return nil, e, ctx.Err()
	}
}

func TestCacheDownloadCannotPublishBeforeConcurrentMutation(t *testing.T) {
	s, id := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := Backend{s, id}
	first := seedRemote(t, b, "race.txt", "first")
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	blocker := &cacheBlockedOpen{Store: base, ready: make(chan struct{}), release: make(chan struct{})}
	replaceStore(t, s, id, blocker)
	done := make(chan error, 1)
	go func() { _, err := s.FetchCache(ctx, id, "race.txt", false, false); done <- err }()
	select {
	case <-blocker.ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Write directly through the underlying store to avoid blocking the backup
	// read inside Backend.Put; external provider writes must also be caught.
	if _, err = base.Put(ctx, "race.txt", strings.NewReader("after"), 5, storage.Condition{IfMatch: first.ETag}); err != nil {
		t.Fatal(err)
	}
	close(blocker.release)
	select {
	case err = <-done:
		if !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("published stale cache: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	entries, err := s.CacheEntries()
	if err != nil || len(entries) != 0 {
		t.Fatal("stale revision registered", entries, err)
	}
}

func TestManualPendingQueueRechecksWithoutAutomaticWrites(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "manual.txt"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	if _, err := s.db.Exec("INSERT INTO sync_queue(job,path,kind,state) VALUES(?,?,?,'pending')", j.ID, "manual.txt", "upload"); err != nil {
		t.Fatal(err)
	}
	s.schedulerCycle(ctx, true)
	if _, err := (Backend{s, id}).Stat(ctx, "manual.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("manual queue resumed writing", err)
	}
	after, _ := s.job(j.ID)
	if after.Status != "needs_attention" {
		t.Fatalf("manual pending status: %+v", after)
	}
}

func TestInMemorySyncPreviewStillExpires(t *testing.T) {
	s, id := testService(t)
	j := syncJob(t, s, id, t.TempDir(), "download", Job{})
	plan, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan.Created = time.Now().Add(-16 * time.Minute)
	s.mu.Lock()
	s.plans[plan.Token] = plan
	s.mu.Unlock()
	if _, err = s.storedPlan(j.ID, plan.Token); err == nil {
		t.Fatal("expired in-memory token remained valid")
	}
}

func TestSchedulerPauseDuringScanKeepsSignalsAndStopsWrites(t *testing.T) {
	s, id := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "planned.txt"), []byte("keep local"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{Watch: true})
	seedRemote(t, Backend{s, id}, "remote.txt", "remote")
	token := s.remoteChangeToken(j.ID)
	if _, err := s.db.Exec("INSERT INTO sync_queue(job,path,kind,state) VALUES(?,?,?,'pending')", j.ID, "planned.txt", "upload"); err != nil {
		t.Fatal(err)
	}
	base, _ := s.store(id)
	blocked := &scanHistoryBlockingListStore{Store: base, started: make(chan struct{}), release: make(chan struct{})}
	replaceStore(t, s, id, blocked)
	done := make(chan struct{})
	go func() { s.schedulerCycle(ctx, false); close(done) }()
	select {
	case <-blocked.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := s.CancelJob(j.ID); err != nil {
		t.Fatal(err)
	}
	close(blocked.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if s.remoteChangeToken(j.ID) != token || !s.hasPendingQueue(j.ID) {
		t.Fatal("paused scan consumed pending signals")
	}
	if _, err := base.Stat(ctx, "planned.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("paused scan wrote remote", err)
	}
	p, err := s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(ctx, j.ID, p.Token); err == nil {
		t.Fatal("RunPlan silently resumed paused job")
	}
}
