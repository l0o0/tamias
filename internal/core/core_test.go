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
	"sync"
	"tamiops/internal/gateway"
	"tamiops/internal/storage"
	"testing"
)

type memoryVault struct {
	mu sync.Mutex
	m  map[string]string
}

func (v *memoryVault) Get(k string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.m[k]
	if !ok {
		return "", errors.New("missing")
	}
	return s, nil
}
func (v *memoryVault) Set(k, s string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[k] = s
	return nil
}
func (v *memoryVault) Delete(k string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, k)
	return nil
}
func testService(t *testing.T) (*Service, string) {
	t.Helper()
	s, err := New(t.TempDir(), &memoryVault{m: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id, err := s.Demo()
	if err != nil {
		t.Fatal(err)
	}
	return s, id
}
func readBody(t *testing.T, b Backend, k string) string {
	t.Helper()
	r, _, err := b.Open(context.Background(), k, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	v, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(v)
}
func TestIncompleteUploadNeverCommits(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	_, err := b.Put(context.Background(), "short.txt", strings.NewReader("short"), 100, storage.Condition{IfNoneMatch: true})
	if err == nil {
		t.Fatal("expected incomplete error")
	}
	if _, err = b.Stat(context.Background(), "short.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("partial object committed: %v", err)
	}
	if directorySize(filepath.Join(s.dir, "staging")) != 0 {
		t.Fatal("failed reception leaked staging")
	}
}
func TestConditionalWriteAndRecovery(t *testing.T) {
	s, id := testService(t)
	setNormalRecoveryForTest(t, s, id, true)
	b := Backend{s, id}
	ctx := context.Background()
	e, err := b.Put(ctx, "note.txt", strings.NewReader("before"), 6, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(ctx, "note.txt", strings.NewReader("oops"), 4, storage.Condition{IfMatch: "\"stale\""}); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if got := readBody(t, b, "note.txt"); got != "before" {
		t.Fatal(got)
	}
	_, err = b.Put(ctx, "note.txt", strings.NewReader("after"), 5, storage.Condition{IfMatch: e.ETag})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(s.dir, "recovery", "*.data"))
	if len(files) != 1 {
		t.Fatalf("recovery count %d", len(files))
	}
	old, _ := os.ReadFile(files[0])
	if string(old) != "before" {
		t.Fatal("old content not preserved")
	}
}

type uncertainStore struct{ storage.Store }

func (u uncertainStore) Put(ctx context.Context, k string, r io.ReadSeeker, n int64, c storage.Condition) (storage.Entry, error) {
	_, err := u.Store.Put(ctx, k, r, n, c)
	if err != nil {
		return storage.Entry{}, err
	}
	return storage.Entry{}, errors.New("response connection lost")
}
func TestUnknownCommitFreezesResource(t *testing.T) {
	s, id := testService(t)
	s.mu.Lock()
	s.stores[id] = uncertainStore{s.stores[id]}
	s.mu.Unlock()
	b := Backend{s, id}
	ctx := context.Background()
	_, err := b.Put(ctx, "unknown.txt", strings.NewReader("payload"), 7, storage.Condition{IfNoneMatch: true})
	if err == nil {
		t.Fatal("missing failure")
	}
	if directorySize(filepath.Join(s.dir, "staging")) != 7 {
		t.Fatal("unknown content must remain")
	}
	_, err = b.Put(ctx, "unknown.txt", strings.NewReader("replace"), 7, storage.Condition{})
	if err == nil || !strings.Contains(err.Error(), "待核对") {
		t.Fatalf("unknown result was retried: %v", err)
	}
	if got := readBody(t, b, "unknown.txt"); got != "payload" {
		t.Fatal(got)
	}
}
func TestDemoDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	v := &memoryVault{m: map[string]string{}}
	s, err := New(dir, v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Demo()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = New(dir, v)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.cfg.Connections) != 0 {
		t.Fatal("sandbox persisted")
	}
}
func TestSingleDataOwner(t *testing.T) {
	s, _ := testService(t)
	other, err := New(s.dir, &memoryVault{m: map[string]string{}})
	if err == nil {
		other.Close()
		t.Fatal("second owner allowed")
	}
}
func TestIncrementalSyncAndStalePreview(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "new.txt"), []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := s.AddJob(Job{Name: "test", ConnectionID: id, LocalPath: local, Direction: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(ctx, j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	p, err = s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Actions {
		if a.Kind == "upload" {
			t.Fatal("unchanged file scheduled")
		}
	}
	if err = os.WriteFile(filepath.Join(local, "new.txt"), []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	e, _ := b.Stat(ctx, "new.txt")
	if _, err = b.Put(ctx, "new.txt", strings.NewReader("external"), 8, storage.Condition{IfMatch: e.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(ctx, j.ID, p.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale plan overwrote external data: %v", err)
	}
	if got := readBody(t, b, "new.txt"); got != "external" {
		t.Fatal(got)
	}
}
func TestFirstSyncConflictAndNoDeletePropagation(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	os.WriteFile(filepath.Join(local, "欢迎使用小花鼠.md"), []byte("local"), 0600)
	j, err := s.AddJob(Job{Name: "test", ConnectionID: id, LocalPath: local, Direction: "both"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err == nil {
		t.Fatal("first sync conflict accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(local, "欢迎使用小花鼠.md")); string(got) != "local" {
		t.Fatal("local overwritten")
	}
}
func TestSymlinkScanSkippedWithoutFollowingOrInferringDeletion(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(target, []byte("secret"), 0600)
	link := filepath.Join(local, "link")
	if err := os.WriteFile(link, []byte("tracked"), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := s.AddJob(Job{Name: "test", ConnectionID: id, LocalPath: local, Direction: "both"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	p, err = s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Actions {
		if a.Path == "link" && (a.Kind == "upload" || a.Kind == "delete-remote" || a.Kind == "delete-local") {
			t.Fatalf("symlink was followed or treated as a deletion: %+v", a)
		}
	}
	if got := readBody(t, Backend{s, id}, "link"); got != "tracked" {
		t.Fatalf("symlink target was uploaded or remote baseline was deleted: %q", got)
	}
}
func TestGatewayRoundTripUsesCore(t *testing.T) {
	s, id := testService(t)
	h := gateway.NewHandler(gateway.Config{Username: "app", Password: "test-password"}, Backend{s, id})
	server := httptest.NewServer(h)
	defer server.Close()
	do := func(method, k, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+"/"+k, strings.NewReader(body))
		req.SetBasicAuth("app", "test-password")
		if method == "PUT" {
			req.Header.Set("If-None-Match", "*")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r := do("PUT", "bridge.txt", "gateway payload")
	r.Body.Close()
	if r.StatusCode != 201 && r.StatusCode != 204 {
		t.Fatalf("PUT %d", r.StatusCode)
	}
	r = do("GET", "bridge.txt", "")
	v, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(v) != "gateway payload" {
		t.Fatal(string(v))
	}
	var n int
	s.db.QueryRow("SELECT count(*) FROM operations WHERE kind='upload' AND state='committed'").Scan(&n)
	if n != 1 {
		t.Fatal("missing durable operation receipt")
	}
}
func TestAPIDeniesCrossOrigin(t *testing.T) {
	s, _ := testService(t)
	h := s.Handler(http.NotFoundHandler())
	for _, origin := range []string{"https://evil.example", "null"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9240/api/demo", nil)
		r.Header.Set("X-Tami-Client", "desktop")
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
}
