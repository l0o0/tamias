package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type gatedDownloadStore struct {
	storage.Store
	started   chan struct{}
	release   chan struct{}
	closed    chan struct{}
	openCount atomic.Int32
}

func (s *gatedDownloadStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	r, e, err := s.Store.Open(ctx, key, etag)
	if err != nil {
		return nil, e, err
	}
	s.openCount.Add(1)
	return &gatedDownloadReader{ReadCloser: r, ctx: ctx, started: s.started, release: s.release, closed: s.closed}, e, nil
}

type gatedDownloadReader struct {
	io.ReadCloser
	ctx     context.Context
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
	blocked atomic.Bool
	once    atomic.Bool
}

func (r *gatedDownloadReader) Read(p []byte) (int, error) {
	if r.blocked.CompareAndSwap(false, true) {
		close(r.started)
		select {
		case <-r.release:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	return r.ReadCloser.Read(p)
}

func (r *gatedDownloadReader) Close() error {
	err := r.ReadCloser.Close()
	if r.once.CompareAndSwap(false, true) {
		close(r.closed)
	}
	return err
}

func installGatedDownloadStore(t *testing.T, s *Service, connection string) *gatedDownloadStore {
	t.Helper()
	s.mu.Lock()
	base := s.stores[connection]
	for {
		previous, ok := base.(*gatedDownloadStore)
		if !ok {
			break
		}
		base = previous.Store
	}
	gated := &gatedDownloadStore{Store: base, started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	s.stores[connection] = gated
	s.mu.Unlock()
	return gated
}

func seedPausedDownload(t *testing.T, s *Service, connection, key, destination string) DownloadTransfer {
	t.Helper()
	b := Backend{s, connection}
	entry, err := b.Put(context.Background(), key, strings.NewReader(strings.Repeat("payload-", 64)), int64(len(strings.Repeat("payload-", 64))), storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "download-test-*")
	if err != nil {
		t.Fatal(err)
	}
	staging := f.Name()
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	emptyHash := sha256.Sum256(nil)
	d := DownloadTransfer{ID: ID(), ConnectionID: connection, Path: key, ETag: entry.ETag, Destination: destination, Staging: staging, Size: entry.Size, Hash: hex.EncodeToString(emptyHash[:]), State: "paused"}
	if err = s.saveDownload(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func waitDownloadSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not reach the gated read")
	}
}

func TestSlowDownloadDoesNotBlockConcurrentMkdir(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	content := strings.Repeat("large-enough-payload-", 1<<15)
	if _, err := b.Put(ctx, "slow.bin", strings.NewReader(content), int64(len(content)), storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	remote := installGatedDownloadStore(t, s, id)
	destination := filepath.Join(t.TempDir(), "slow.bin")
	completed := make(chan error, 1)
	go func() {
		_, err := s.StartDownload(ctx, id, "slow.bin", destination)
		completed <- err
	}()
	waitDownloadSignal(t, remote.started)

	mkdirDone := make(chan error, 1)
	go func() { mkdirDone <- b.Mkdir(ctx, "during-download") }()
	select {
	case err := <-mkdirDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(remote.release)
		<-completed
		t.Fatal("Mkdir waited for a slow download; download still holds the global writer lock")
	}
	putDone := make(chan error, 1)
	go func() {
		_, err := b.Put(ctx, "during-download.txt", strings.NewReader("commit"), 6, storage.Condition{IfNoneMatch: true})
		putDone <- err
	}()
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(remote.release)
		<-putDone
		<-completed
		t.Fatal("Put commit waited for a slow download; download still holds the global writer lock")
	}
	close(remote.release)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != content {
		t.Fatalf("download result mismatch: %v", err)
	}
}

func TestBackendDownloadDoesNotBlockConcurrentUploadCommit(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	content := strings.Repeat("backend-download-", 1<<14)
	if _, err := b.Put(ctx, "slow-direct.bin", strings.NewReader(content), int64(len(content)), storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	remote := installGatedDownloadStore(t, s, id)
	destination := filepath.Join(t.TempDir(), "direct.bin")
	downloadDone := make(chan error, 1)
	go func() { downloadDone <- b.Download(ctx, "slow-direct.bin", destination) }()
	waitDownloadSignal(t, remote.started)

	putDone := make(chan error, 1)
	go func() {
		_, err := b.Put(ctx, "during-direct-download.txt", strings.NewReader("commit"), 6, storage.Condition{IfNoneMatch: true})
		putDone <- err
	}()
	select {
	case err := <-putDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(remote.release)
		<-putDone
		<-downloadDone
		t.Fatal("upload commit waited for Backend.Download's slow reader")
	}
	close(remote.release)
	if err := <-downloadDone; err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != content {
		t.Fatalf("download result mismatch: %v", err)
	}
}

func TestDownloadResumeIsSerializedPerTransferAndCancelWaitsForClose(t *testing.T) {
	s, id := testService(t)
	d := seedPausedDownload(t, s, id, "resume.bin", filepath.Join(t.TempDir(), "resume.bin"))
	remote := installGatedDownloadStore(t, s, id)
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.ResumeDownload(context.Background(), d.ID)
		firstDone <- err
	}()
	waitDownloadSignal(t, remote.started)

	secondDone := make(chan error, 1)
	go func() {
		_, err := s.ResumeDownload(context.Background(), d.ID)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "进行中") {
			t.Fatalf("concurrent Resume should fail as in-progress, got %v", err)
		}
	case <-time.After(2 * time.Second):
		close(remote.release)
		<-firstDone
		t.Fatal("second Resume blocked instead of refusing re-entry")
	}
	if got := remote.openCount.Load(); got != 1 {
		t.Fatalf("second Resume opened remote body, count=%d", got)
	}
	close(remote.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	d = seedPausedDownload(t, s, id, "cancel.bin", filepath.Join(t.TempDir(), "cancel.bin"))
	remote = installGatedDownloadStore(t, s, id)
	resumeDone := make(chan error, 1)
	go func() {
		_, err := s.ResumeDownload(context.Background(), d.ID)
		resumeDone <- err
	}()
	waitDownloadSignal(t, remote.started)
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- s.CancelDownload(d.ID) }()
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelDownload did not cancel and wait for the active read")
	}
	select {
	case <-remote.closed:
	default:
		t.Fatal("CancelDownload returned before closing the active remote reader")
	}
	if err := <-resumeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resume returned %v", err)
	}
	if _, err := os.Stat(d.Staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel did not remove staging after reader close: %v", err)
	}
	if directorySize(filepath.Join(s.dir, "staging")) != 0 {
		t.Fatal("cancel leaked staging bytes or reservations")
	}
}
