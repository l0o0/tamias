package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type syncActiveActionProbeStore struct {
	storage.Store
	observe func(context.Context, string, string) error
}

func (s syncActiveActionProbeStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	if s.observe != nil {
		if err := s.observe(ctx, "open", key); err != nil {
			return nil, storage.Entry{}, err
		}
	}
	return s.Store.Open(ctx, key, etag)
}

func (s syncActiveActionProbeStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition storage.Condition) (storage.Entry, error) {
	if s.observe != nil {
		if err := s.observe(ctx, "put", key); err != nil {
			return storage.Entry{}, err
		}
	}
	return s.Store.Put(ctx, key, body, size, condition)
}

func waitForActiveAction(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case action := <-events:
		return action
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the sync operation")
		return ""
	}
}

func TestRunPlanTracksUploadAndDownloadForBothDirection(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "a-upload.txt"), []byte("local change"), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	seedRemote(t, b, "z-download.txt", "remote change")
	job := syncJob(t, s, id, local, "both", Job{})
	plan, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action, ok := actionOf(plan, "a-upload.txt"); !ok || action.Kind != "upload" {
		t.Fatalf("upload action = %+v, found=%v", action, ok)
	}
	if action, ok := actionOf(plan, "z-download.txt"); !ok || action.Kind != "download" {
		t.Fatalf("download action = %+v, found=%v", action, ok)
	}

	observed := make(chan string, 2)
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	replaceStore(t, s, id, syncActiveActionProbeStore{Store: base, observe: func(_ context.Context, method, key string) error {
		if method == "put" && key == "a-upload.txt" || method == "open" && key == "z-download.txt" {
			current, err := s.job(job.ID)
			if err != nil {
				return err
			}
			observed <- current.ActiveAction
		}
		return nil
	}})

	result, err := s.RunPlan(context.Background(), job.ID, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForActiveAction(t, observed); got != "upload" {
		t.Fatalf("active action during upload = %q", got)
	}
	if got := waitForActiveAction(t, observed); got != "download" {
		t.Fatalf("active action during download = %q", got)
	}
	if result.ActiveAction != "" || result.Status != "synced" {
		t.Fatalf("completed job retained runtime action: %+v", result)
	}
}

func TestRunPlanClearsActiveActionAfterFailure(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "upload.txt"), []byte("local change"), 0600); err != nil {
		t.Fatal(err)
	}
	job := syncJob(t, s, id, local, "upload", Job{})
	plan, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan string, 1)
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	replaceStore(t, s, id, syncActiveActionProbeStore{Store: base, observe: func(_ context.Context, method, key string) error {
		if method == "put" && key == "upload.txt" {
			current, err := s.job(job.ID)
			if err != nil {
				return err
			}
			observed <- current.ActiveAction
			return errors.New("injected upload failure")
		}
		return nil
	}})

	result, err := s.RunPlan(context.Background(), job.ID, plan.Token)
	if err == nil {
		t.Fatal("expected the injected upload failure")
	}
	if got := waitForActiveAction(t, observed); got != "upload" {
		t.Fatalf("active action during failed upload = %q", got)
	}
	current, getErr := s.job(job.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if result.ActiveAction != "" || current.ActiveAction != "" || current.Status != "error" {
		t.Fatalf("failed job retained runtime action: result=%+v current=%+v", result, current)
	}
}

func TestRunPlanClearsActiveActionAfterCancel(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	b := Backend{s, id}
	seedRemote(t, b, "remote.txt", "remote change")
	job := syncJob(t, s, id, local, "download", Job{})
	plan, err := s.Preview(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 1)
	base, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	replaceStore(t, s, id, syncActiveActionProbeStore{Store: base, observe: func(ctx context.Context, method, key string) error {
		if method == "open" && key == "remote.txt" {
			current, err := s.job(job.ID)
			if err != nil {
				return err
			}
			entered <- current.ActiveAction
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}})

	type runResult struct {
		job Job
		err error
	}
	finished := make(chan runResult, 1)
	go func() {
		result, runErr := s.RunPlan(context.Background(), job.ID, plan.Token)
		finished <- runResult{job: result, err: runErr}
	}()
	if got := waitForActiveAction(t, entered); got != "download" {
		t.Fatalf("active action during download = %q", got)
	}
	if err = s.CancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActiveAction != "" {
		t.Fatalf("cancelled job retained its active action: %+v", current)
	}
	select {
	case result := <-finished:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancelled run error = %v", result.err)
		}
		current, err = s.job(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.job.ActiveAction != "" || current.ActiveAction != "" || current.Status != "paused" {
			t.Fatalf("cancelled job retained runtime action: result=%+v current=%+v", result.job, current)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled sync run did not finish")
	}
}
