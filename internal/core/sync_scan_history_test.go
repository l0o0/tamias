package core

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type scanHistoryFailListStore struct{ storage.Store }

func (s scanHistoryFailListStore) List(context.Context, string) ([]storage.Entry, error) {
	return nil, errors.New("remote listing failed")
}

type scanHistoryBlockingListStore struct {
	storage.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *scanHistoryBlockingListStore) List(ctx context.Context, key string) ([]storage.Entry, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Store.List(ctx, key)
}

func scanHistoryRows(t *testing.T, s *Service, jobID string) []SyncScanHistory {
	t.Helper()
	request := httptest.NewRequest("GET", "/api/jobs/scan-history?id="+jobID, nil)
	result, err, handled := s.scanHistoryGet(request)
	if !handled || err != nil {
		t.Fatalf("scan history read failed: handled=%v err=%v", handled, err)
	}
	history, ok := result.([]SyncScanHistory)
	if !ok {
		t.Fatalf("unexpected scan history response type %T", result)
	}
	return history
}

func TestSyncPreviewTransferEstimateUsesActionSizesOnly(t *testing.T) {
	s, connectionID := testService(t)
	local := t.TempDir()
	localUpload := "local upload bytes"
	if err := os.WriteFile(filepath.Join(local, "upload.txt"), []byte(localUpload), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(local, "local-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "same.txt"), []byte("same content"), 0600); err != nil {
		t.Fatal(err)
	}
	backend := Backend{s, connectionID}
	remotePrefix := "scan-estimate-upload"
	if err := ensureRemoteDirectory(context.Background(), backend, remotePrefix); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, backend, remotePrefix+"/same.txt", "same content")
	seedRemote(t, backend, remotePrefix+"/remote-only.txt", "delete should not count")
	job := syncJob(t, s, connectionID, local, "mirror-upload", Job{RemotePath: remotePrefix, DeleteThreshold: 1})
	preview, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.UploadBytes != int64(len(localUpload)) || preview.DownloadBytes != 0 {
		t.Fatalf("unexpected upload preview estimate: upload=%d download=%d", preview.UploadBytes, preview.DownloadBytes)
	}
	for _, key := range []string{"local-dir", "same.txt", "remote-only.txt"} {
		action, ok := actionOf(preview, key)
		if !ok {
			t.Fatalf("missing expected non-transfer action for %q: %+v", key, preview.Actions)
		}
		if action.Kind == "upload" || action.Kind == "download" {
			t.Fatalf("path %q should not contribute a transfer estimate: %+v", key, action)
		}
	}
	stored, err := s.storedPlan(job.ID, preview.Token)
	if err != nil || stored.UploadBytes != preview.UploadBytes || stored.DownloadBytes != preview.DownloadBytes {
		t.Fatalf("persisted plan lost preview byte totals: stored=%+v err=%v", stored, err)
	}

	downloadData := "remote download payload"
	downloadPrefix := "scan-estimate-download"
	if err := ensureRemoteDirectory(context.Background(), backend, downloadPrefix); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, backend, downloadPrefix+"/download.txt", downloadData)
	downloadJob := syncJob(t, s, connectionID, t.TempDir(), "download", Job{RemotePath: downloadPrefix})
	downloadPreview, err := s.Preview(context.Background(), downloadJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if downloadPreview.DownloadBytes != int64(len(downloadData)) || downloadPreview.UploadBytes != 0 {
		t.Fatalf("unexpected download preview estimate: upload=%d download=%d", downloadPreview.UploadBytes, downloadPreview.DownloadBytes)
	}
	for _, action := range downloadPreview.Actions {
		if action.Kind == "skip" || strings.HasPrefix(action.Kind, "mkdir-") || strings.HasPrefix(action.Kind, "delete-") {
			continue
		}
		if action.Kind != "download" {
			t.Fatalf("unexpected action in download plan: %+v", action)
		}
	}

	history := scanHistoryRows(t, s, job.ID)
	if len(history) != 1 {
		t.Fatalf("got %d history records, want one", len(history))
	}
	if history[0].Status != "needs_attention" || history[0].Uploads != 1 || history[0].Downloads != 0 || history[0].Deletes != 1 || history[0].UploadBytes != int64(len(localUpload)) || history[0].DownloadBytes != 0 {
		t.Fatalf("history did not preserve the scan counts and byte totals: %+v", history[0])
	}
	if history[0].Scope.LocalPath != job.LocalPath || history[0].Scope.RemotePath != remotePrefix || history[0].Scope.Direction != "mirror-upload" || history[0].Completed == "" {
		t.Fatalf("history has an incomplete scope or timestamp: %+v", history[0])
	}
}

func TestSyncPreviewEstimateStaysBoundToStoredPreview(t *testing.T) {
	s, connectionID := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "version.txt"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, connectionID, local, "upload", Job{})
	first, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.UploadBytes != int64(len("first")) {
		t.Fatalf("first preview estimate=%d", first.UploadBytes)
	}
	if err = os.WriteFile(filepath.Join(local, "version.txt"), []byte("larger second version"), 0600); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.plans = map[string]Plan{} // Verify the durable plan, not only the memory cache.
	s.mu.Unlock()
	stored, err := s.storedPlan(job.ID, first.Token)
	if err != nil || stored.UploadBytes != first.UploadBytes {
		t.Fatalf("stored preview estimate changed with the current file: bytes=%d err=%v", stored.UploadBytes, err)
	}
	storedAction, found := actionOf(stored, "version.txt")
	if !found || storedAction.Size != int64(len("first")) {
		t.Fatalf("durable action size lost: %+v", storedAction)
	}
	second, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token == first.Token || second.UploadBytes != int64(len("larger second version")) {
		t.Fatalf("updated file did not produce a fresh preview: first=%+v second=%+v", first, second)
	}
}

func TestSyncScanHistoryRecordsCompleteFailureAndScannedCounts(t *testing.T) {
	s, connectionID := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "local.txt"), []byte("known local item"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, connectionID, local, "both", Job{})
	base, err := s.store(connectionID)
	if err != nil {
		t.Fatal(err)
	}
	replaceStore(t, s, connectionID, scanHistoryFailListStore{Store: base})
	if _, err = s.Preview(context.Background(), job.ID); err == nil || !strings.Contains(err.Error(), "remote listing failed") {
		t.Fatalf("Preview error=%v, want remote listing failure", err)
	}
	history := scanHistoryRows(t, s, job.ID)
	if len(history) != 1 {
		t.Fatalf("failed full scan was not recorded: %+v", history)
	}
	entry := history[0]
	if entry.Status != "error" || entry.Error == "" || entry.LocalFiles != 1 || entry.RemoteFiles != 0 || entry.Completed == "" {
		t.Fatalf("failure record lost status, error, or partial scan counts: %+v", entry)
	}

	jobState, err := s.job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if jobState.Status != "error" || jobState.LastScanSummary == "" || jobState.LastScanAt == "" {
		t.Fatalf("task card state was not updated after a failed scan: %+v", jobState)
	}
}

func TestSyncScanStatusDuringScanAndPreservesPause(t *testing.T) {
	s, connectionID := testService(t)
	job := syncJob(t, s, connectionID, t.TempDir(), "upload", Job{})
	base, err := s.store(connectionID)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &scanHistoryBlockingListStore{Store: base, started: make(chan struct{}), release: make(chan struct{})}
	replaceStore(t, s, connectionID, blocking)
	finished := make(chan error, 1)
	go func() {
		_, previewErr := s.Preview(context.Background(), job.ID)
		finished <- previewErr
	}()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Preview did not reach remote enumeration")
	}
	current, err := s.job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "scanning" {
		t.Fatalf("job status while listing remote was %q, want scanning", current.Status)
	}
	if err = s.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	current, err = s.job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "paused" || current.Enabled {
		t.Fatalf("scan completion overwrote the user's pause: %+v", current)
	}
	history := scanHistoryRows(t, s, job.ID)
	if len(history) != 1 || history[0].Completed == "" {
		t.Fatalf("completed scan history is incomplete after task pause: %+v", history)
	}
}

func TestSyncScanHistoryBoundedToOneHundredPerJob(t *testing.T) {
	s, connectionID := testService(t)
	job := syncJob(t, s, connectionID, t.TempDir(), "upload", Job{})
	for i := 0; i < syncScanHistoryPerJob+3; i++ {
		if _, err := s.Preview(context.Background(), job.ID); err != nil {
			t.Fatalf("Preview #%d: %v", i+1, err)
		}
	}
	history := scanHistoryRows(t, s, job.ID)
	if len(history) != syncScanHistoryPerJob {
		t.Fatalf("history retained %d records, want the per-task limit %d", len(history), syncScanHistoryPerJob)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM sync_scan_history WHERE job=?", job.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != syncScanHistoryPerJob {
		t.Fatalf("database has %d history records for task, want %d", count, syncScanHistoryPerJob)
	}
}

func TestSyncScanHistoryRecoversInterruptedRunOnRestart(t *testing.T) {
	dir := t.TempDir()
	vault := &memoryVault{m: map[string]string{}}
	s, err := New(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	connectionID, jobID := "persisted-connection", "interrupted-scan-job"
	s.mu.Lock()
	s.cfg.Connections = []Connection{{Config: storage.Config{ID: connectionID, Name: "persisted", Kind: "webdav"}}}
	s.cfg.Jobs = []Job{{ID: jobID, Name: "interrupted", ConnectionID: connectionID, LocalPath: t.TempDir(), Direction: "both", Enabled: true, Status: "scanning"}}
	if err = s.saveLocked(); err != nil {
		s.mu.Unlock()
		s.Close()
		t.Fatal(err)
	}
	s.mu.Unlock()
	started := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err = s.db.Exec(`INSERT INTO sync_scan_history(job,started,scope,status) VALUES(?,?,?,'scanning')`, jobID, started, fmt.Sprintf(`{"localPath":"%s","remotePath":"archive","direction":"both","exclude":[]}`, t.TempDir())); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	job, err := restarted.job(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "error" || !strings.Contains(job.LastScanSummary, "中断") {
		t.Fatalf("interrupted scanning task was not recovered: %+v", job)
	}
	history := scanHistoryRows(t, restarted, jobID)
	if len(history) != 1 || history[0].Status != "interrupted" || history[0].Completed == "" || history[0].Error == "" {
		t.Fatalf("interrupted history record was not closed on restart: %+v", history)
	}
}
