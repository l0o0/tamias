package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"tamiops/internal/gateway"
	"tamiops/internal/storage"
	"testing"
)

func TestStagedContentMustMatchPreview(t *testing.T) {
	s, id := testService(t)
	h := sha256.Sum256([]byte("planned"))
	b := Backend{s, id}
	_, err := b.put(context.Background(), "changed.txt", strings.NewReader("changed"), 7, storage.Condition{IfNoneMatch: true}, hex.EncodeToString(h[:]))
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if _, err = b.Stat(context.Background(), "changed.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("unplanned bytes committed")
	}
}
func TestDataDirectorySymlinkOverlapRejected(t *testing.T) {
	parent := t.TempDir()
	actual := filepath.Join(parent, "app-data")
	os.Mkdir(actual, 0700)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Skip(err)
	}
	s, err := New(alias, &memoryVault{m: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _ := s.Demo()
	if _, err = s.AddJob(Job{Name: "bad", ConnectionID: id, LocalPath: parent, Direction: "upload"}); err == nil {
		t.Fatal("application state exposed to sync")
	}
}
func TestStartupRemovesOnlySafeStaging(t *testing.T) {
	dir := t.TempDir()
	v := &memoryVault{m: map[string]string{}}
	s, err := New(dir, v)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(s.dir, "staging", "upload-orphan")
	unknown := filepath.Join(s.dir, "staging", "upload-unknown")
	os.WriteFile(orphan, []byte("orphan"), 0600)
	os.WriteFile(unknown, []byte("retain"), 0600)
	_, err = s.db.Exec("INSERT INTO operations(id,connection,path,kind,state,staging,receipt,created) VALUES('u','c','k','upload','committing',?,'','')", unknown)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = New(dir, v)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan not removed")
	}
	if _, err = os.Stat(unknown); err != nil {
		t.Fatal("unknown commit content removed")
	}
}
func TestDownloadNoReplaceAndUserDotFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "note.txt")
	os.WriteFile(target, []byte("keep"), 0600)
	if err := publishNewFile(target, strings.NewReader("new")); err == nil {
		t.Fatal("replaced destination")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "keep" {
		t.Fatal("destination changed")
	}
	os.WriteFile(filepath.Join(dir, ".tami-notes"), []byte("notes"), 0600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := scanLocal(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[".tami-notes"]; !ok {
		t.Fatal("user dotfile silently skipped")
	}
}
func TestMkdirCannotShadowFile(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	_, err := b.Put(ctx, "collision", strings.NewReader("data"), 4, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Mkdir(ctx, "collision"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if err = b.Mkdir(ctx, "collision/child"); err == nil {
		t.Fatal("file parent treated as directory")
	}
	if got := readBody(t, b, "collision"); got != "data" {
		t.Fatal(got)
	}
}
func TestGatewayUnsupportedConditionsNeverMutate(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	b.Put(ctx, "keep.txt", strings.NewReader("original"), 8, storage.Condition{IfNoneMatch: true})
	h := gateway.NewHandler(gateway.Config{Username: "u", Password: "p"}, b)
	for _, tc := range []struct {
		method, key, header, value string
		status                     int
	}{{"DELETE", "keep.txt", "If-None-Match", "*", 412}, {"PUT", "keep.txt", "If", "([\"wrong\"])", 501}, {"MKCOL", "new", "If-Match", "\"missing\"", 501}} {
		r := httptest.NewRequest(tc.method, "http://localhost/"+tc.key, strings.NewReader("data"))
		r.SetBasicAuth("u", "p")
		r.Header.Set(tc.header, tc.value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %d body=%s", tc.method, w.Code, w.Body.String())
		}
	}
	if got := readBody(t, b, "keep.txt"); got != "original" {
		t.Fatal("conditional request mutated file")
	}
}

type conflictStore struct{ storage.Store }
type noReceiptStore struct{ storage.Store }

func (n noReceiptStore) Put(ctx context.Context, k string, r io.ReadSeeker, size int64, c storage.Condition) (storage.Entry, error) {
	e, err := n.Store.Put(ctx, k, r, size, c)
	e.ETag = ""
	return e, err
}
func TestMissingCommitRevisionIsUncertain(t *testing.T) {
	s, id := testService(t)
	s.mu.Lock()
	s.stores[id] = noReceiptStore{s.stores[id]}
	s.mu.Unlock()
	b := Backend{s, id}
	if _, err := b.Put(context.Background(), "no-receipt", strings.NewReader("data"), 4, storage.Condition{IfNoneMatch: true}); err == nil {
		t.Fatal("missing receipt marked success")
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM operations WHERE kind='upload' LIMIT 1").Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestPauseDoesNotReleaseRunningJob(t *testing.T) {
	s, id := testService(t)
	j, err := s.AddJob(Job{Name: "running", ConnectionID: id, LocalPath: t.TempDir(), Direction: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.Jobs[0].Status = "running"
	s.mu.Unlock()
	if err = s.pauseJob(j.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.deleteJob(j.ID); err == nil {
		t.Fatal("running job deleted before its commit finished")
	}
	if _, err = s.Preview(context.Background(), j.ID); err == nil {
		t.Fatal("running paused job accepted a concurrent plan")
	}
}

func (c conflictStore) Put(context.Context, string, io.ReadSeeker, int64, storage.Condition) (storage.Entry, error) {
	return storage.Entry{}, storage.ErrConflict
}
func TestDefiniteFailureReleasesBackup(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	ctx := context.Background()
	e, err := b.Put(ctx, "item", strings.NewReader("old"), 3, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[id] = conflictStore{s.stores[id]}
	s.mu.Unlock()
	_, err = b.Put(ctx, "item", strings.NewReader("new"), 3, storage.Condition{IfMatch: e.ETag})
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if n := directorySize(filepath.Join(s.dir, "recovery")); n != 0 {
		t.Fatalf("definite conflict leaked %d backup bytes", n)
	}
}
