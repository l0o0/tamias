package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tamiops/internal/storage"
)

func TestSyncDownloadAcceptsUnknownContentLength(t *testing.T) {
	s, id := testService(t)
	raw := storage.NewMemory()
	const contents = "chunked response with a stable strong version"
	entry, err := raw.Put(context.Background(), "notes.txt", strings.NewReader(contents), int64(len(contents)), storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	s.stores[id] = unknownOpenSizeStore{Store: raw}
	dir := t.TempDir()
	j := syncJob(t, s, id, dir, "download", Job{})
	plan, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, plan.Token); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || string(got) != contents {
		t.Fatalf("downloaded %q: %v", got, err)
	}
	var etag, hash string
	if err = s.db.QueryRow("SELECT remote_etag,local_hash FROM baseline WHERE job=? AND path='notes.txt'", j.ID).Scan(&etag, &hash); err != nil || etag != entry.ETag || hash == "" {
		t.Fatalf("unverified baseline: %q %q %v", etag, hash, err)
	}
}

func TestSyncDownloadUnknownLengthRejectsChangedVersion(t *testing.T) {
	s, id := testService(t)
	raw := storage.NewMemory()
	_, err := raw.Put(context.Background(), "notes.txt", strings.NewReader("before"), 6, storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	st := &compatibilityTestStore{Store: raw, unknownLength: true}
	st.afterOpen = func(ctx context.Context, key string) {
		_, _ = raw.Put(ctx, key, strings.NewReader("after"), 5, storage.Condition{})
	}
	s.stores[id] = st
	dir := t.TempDir()
	j := syncJob(t, s, id, dir, "download", Job{})
	plan, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, plan.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("changed version accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "notes.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("download installed stale data: %v", err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=?", j.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected baseline: %d %v", count, err)
	}
}
