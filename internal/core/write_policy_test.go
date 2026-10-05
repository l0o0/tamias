package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"tamiops/internal/storage"
)

// Models the relevant Data Capsule behavior: ordinary CRUD, ignored PUT
// conditions, no PUT ETag, but readable strong ETags from Stat and Open.
type compatibilityTestStore struct {
	storage.Store
	puts          int
	afterPut      func(context.Context, string)
	afterOpen     func(context.Context, string)
	unknownLength bool
}

func (st *compatibilityTestStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, _ storage.Condition) (storage.Entry, error) {
	st.puts++
	e, err := st.Store.Put(ctx, key, r, size, storage.Condition{})
	if err == nil && st.afterPut != nil {
		st.afterPut(ctx, key)
	}
	e.ETag = ""
	return e, err
}

func (st *compatibilityTestStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	r, e, err := st.Store.Open(ctx, key, etag)
	if st.unknownLength {
		e.Size = -1
	}
	if err == nil && st.afterOpen != nil {
		st.afterOpen(ctx, key)
	}
	return r, e, err
}

func setPolicyForTest(t *testing.T, s *Service, id, mode string, caps storage.Capabilities) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == id {
			s.cfg.Connections[i].WriteMode = mode
			s.cfg.Connections[i].Tested = true
			s.cfg.Connections[i].Capabilities = caps
			return
		}
	}
	t.Fatal("missing connection")
}

func TestCompatibleWriteVerifiesReceiptAndPreservesOldContent(t *testing.T) {
	s, id := testService(t)
	setNormalRecoveryForTest(t, s, id, true)
	ctx := context.Background()
	raw, _ := s.store(id)
	old, _ := raw.Put(ctx, "note.txt", strings.NewReader("before"), 6, storage.Condition{})
	st := &compatibilityTestStore{Store: raw, unknownLength: true}
	s.stores[id] = st
	setPolicyForTest(t, s, id, storage.WriteModeCompatible, storage.Capabilities{})
	b := Backend{s, id}
	result, err := b.Put(ctx, "note.txt", strings.NewReader("after"), 5, storage.Condition{IfMatch: old.ETag})
	if err != nil || !strongTag(result.ETag) || result.Path != "note.txt" || result.Size != 5 {
		t.Fatalf("verified receipt: %+v %v", result, err)
	}
	if got := readBody(t, b, "note.txt"); got != "after" {
		t.Fatal(got)
	}
	backups, _ := filepath.Glob(filepath.Join(s.dir, "recovery", "*.data"))
	if len(backups) != 1 {
		t.Fatalf("backup count = %d", len(backups))
	}
	contents, _ := os.ReadFile(backups[0])
	if string(contents) != "before" {
		t.Fatalf("lost recovery: %q", contents)
	}
	c, _ := s.connection(id)
	if c.Capabilities.ConditionalWrite || c.Capabilities.ConditionalDelete {
		t.Fatal("mode forged measured capabilities")
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM operations WHERE kind='upload' ORDER BY created DESC LIMIT 1`).Scan(&state); err != nil || state != "committed" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if directorySize(filepath.Join(s.dir, "staging")) != 0 {
		t.Fatal("successful upload leaked staging")
	}
	if _, err = b.Put(ctx, "note.txt", strings.NewReader("bad"), 3, storage.Condition{IfMatch: old.ETag}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale client precondition: %v", err)
	}
	if st.puts != 1 {
		t.Fatal("stale write reached server")
	}
	if err = b.Delete(ctx, "note.txt", storage.Condition{IfMatch: result.ETag}); err == nil {
		t.Fatal("compat mode allowed unsafe delete")
	}
}

func TestCompatibleWritePostCommitMismatchRemainsUncertain(t *testing.T) {
	for _, mutation := range []string{"different-content", "missing", "changed-during-read"} {
		t.Run(mutation, func(t *testing.T) {
			s, id := testService(t)
			setNormalRecoveryForTest(t, s, id, true)
			ctx := context.Background()
			raw, _ := s.store(id)
			old, _ := raw.Put(ctx, "note.txt", strings.NewReader("before"), 6, storage.Condition{})
			st := &compatibilityTestStore{Store: raw}
			st.afterPut = func(ctx context.Context, key string) {
				switch mutation {
				case "missing":
					_ = raw.Delete(ctx, key, storage.Condition{})
				case "different-content":
					_, _ = raw.Put(ctx, key, strings.NewReader("alien"), 5, storage.Condition{})
				case "changed-during-read":
					st.afterOpen = func(ctx context.Context, key string) {
						_, _ = raw.Put(ctx, key, strings.NewReader("alien"), 5, storage.Condition{})
					}
				}
			}
			s.stores[id] = st
			setPolicyForTest(t, s, id, storage.WriteModeCompatible, storage.Capabilities{})
			_, err := (Backend{s, id}).Put(ctx, "note.txt", strings.NewReader("after"), 5, storage.Condition{IfMatch: old.ETag})
			if err == nil || errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("postcommit outcome incorrectly classified: %v", err)
			}
			var state, stage string
			if err := s.db.QueryRow(`SELECT state,staging FROM operations WHERE kind='upload'`).Scan(&state, &stage); err != nil || state != "uncertain" {
				t.Fatalf("state=%s err=%v", state, err)
			}
			if _, err := os.Stat(stage); err != nil {
				t.Fatal("lost uncertain staging", err)
			}
			backups, _ := filepath.Glob(filepath.Join(s.dir, "recovery", "*.data"))
			if len(backups) != 1 {
				t.Fatal("lost recovery after remote mutation")
			}
			if _, err := (Backend{s, id}).Put(ctx, "note.txt", strings.NewReader("retry"), 5, storage.Condition{}); err == nil || !strings.Contains(err.Error(), "待核对") {
				t.Fatalf("uncertain retry not blocked: %v", err)
			}
		})
	}
}

func TestCompatiblePrecommitRecheckPreservesExternalChange(t *testing.T) {
	s, id := testService(t)
	setNormalRecoveryForTest(t, s, id, true)
	ctx := context.Background()
	raw, _ := s.store(id)
	old, _ := raw.Put(ctx, "note.txt", strings.NewReader("before"), 6, storage.Condition{})
	st := &compatibilityTestStore{Store: raw}
	st.afterOpen = func(ctx context.Context, key string) {
		_, _ = raw.Put(ctx, key, strings.NewReader("external"), 8, storage.Condition{})
	}
	s.stores[id] = st
	setPolicyForTest(t, s, id, storage.WriteModeCompatible, storage.Capabilities{})
	_, err := (Backend{s, id}).Put(ctx, "note.txt", strings.NewReader("after"), 5, storage.Condition{IfMatch: old.ETag})
	if !errors.Is(err, storage.ErrConflict) || st.puts != 0 {
		t.Fatalf("precommit change overwritten: puts=%d err=%v", st.puts, err)
	}
	st.afterOpen = nil
	if got := readBody(t, Backend{s, id}, "note.txt"); got != "external" {
		t.Fatal(got)
	}
}

func TestCopyOnlyHTTPUploadUsesNewNamesAndBlocksStableWrites(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, _ := s.store(id)
	_, _ = raw.Put(ctx, "original.txt", strings.NewReader("original"), 8, storage.Condition{})
	s.stores[id] = &compatibilityTestStore{Store: raw}
	setPolicyForTest(t, s, id, storage.WriteModeCopy, storage.Capabilities{})
	var paths []string
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/api/files/upload?connectionId="+url.QueryEscape(id)+"&path=original.txt", strings.NewReader("copy"))
		r.Header.Set("X-Tami-Client", "desktop")
		w := httptest.NewRecorder()
		s.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("upload: %d %s", w.Code, w.Body.String())
		}
	}
	entries, _ := raw.List(ctx, "")
	for _, e := range entries {
		if strings.HasPrefix(e.Name, "original (副本-") {
			paths = append(paths, e.Path)
		}
	}
	if len(paths) != 2 || paths[0] == paths[1] {
		t.Fatalf("new destinations missing: %v", paths)
	}
	if got := readBody(t, Backend{s, id}, "original.txt"); got != "original" {
		t.Fatalf("original changed: %q", got)
	}
	for _, key := range paths {
		if got := readBody(t, Backend{s, id}, key); got != "copy" {
			t.Fatal(got)
		}
		var receipt string
		if err := s.db.QueryRow(`SELECT json_extract(receipt,'$.destinationPath') FROM operations WHERE kind='upload' AND json_extract(receipt,'$.destinationPath')=?`, key).Scan(&receipt); err != nil || receipt != key {
			t.Fatalf("receipt lost actual path: %v", err)
		}
	}
	// Even a server with real conditional support cannot override copy policy.
	setPolicyForTest(t, s, id, storage.WriteModeCopy, storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true})
	b := Backend{s, id}
	if _, err := b.Put(ctx, "original.txt", strings.NewReader("bad"), 3, storage.Condition{}); err == nil {
		t.Fatal("stable write bypassed copy policy")
	}
	if err := b.Delete(ctx, "original.txt", storage.Condition{IfMatch: "*"}); err == nil {
		t.Fatal("delete bypassed copy policy")
	}
	if _, err := b.Copy(ctx, "original.txt", "bad.txt", false); err == nil {
		t.Fatal("copy bypassed policy")
	}
	if _, err := b.Move(ctx, "original.txt", "bad.txt", false); err == nil {
		t.Fatal("move bypassed policy")
	}
	if err := b.Mkdir(ctx, "folder"); err == nil {
		t.Fatal("mkdir bypassed copy policy")
	}
}

func TestWritePolicyFailsClosedAndCopyNameKeepsExtension(t *testing.T) {
	for _, mode := range []string{"", storage.WriteModeStrict, "unknown", storage.WriteModeCompatible, storage.WriteModeCopy} {
		c := Connection{Config: storage.Config{WriteMode: mode}}
		if canUpload(c) || canDelete(c) || canWriteStrict(c) {
			t.Fatalf("unverified mode %q allowed mutation", mode)
		}
	}
	key := copyUploadKey("目录/" + strings.Repeat("长", 100) + ".pdf")
	if !strings.HasPrefix(key, "目录/") || !strings.HasSuffix(key, ".pdf") || len(path.Base(key)) > 240 {
		t.Fatalf("invalid copy key: %s", key)
	}
}

func TestCompatibleUnknownResponseCanReconcileChunkedRead(t *testing.T) {
	s, id := testService(t)
	raw, _ := s.store(id)
	st := &compatibilityTestStore{Store: raw, unknownLength: true}
	s.stores[id] = uncertainStore{st}
	setPolicyForTest(t, s, id, storage.WriteModeCompatible, storage.Capabilities{})
	_, err := (Backend{s, id}).Put(context.Background(), "lost-response.txt", strings.NewReader("data"), 4, storage.Condition{IfNoneMatch: true})
	if err == nil {
		t.Fatal("lost response incorrectly succeeded")
	}
	var op string
	if err = s.db.QueryRow(`SELECT id FROM operations WHERE state='uncertain'`).Scan(&op); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM operations WHERE id=?`, op).Scan(&state); err != nil || state != "committed" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}
