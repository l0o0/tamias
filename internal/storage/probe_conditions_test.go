package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Match servers which accept a negative conditional PUT but omit the response
// ETag, and may also ignore the old validator on GET and DELETE.
func TestWebDAVProbeDiagnosesIgnoredConditions(t *testing.T) {
	for _, condition := range []string{"If-None-Match", "If-Match PUT", "If-Match DELETE", "all"} {
		t.Run(condition, func(t *testing.T) {
			state := &probeDAVState{objects: make(map[string]probeDAVObject), ignoreCondition: condition}
			server := httptest.NewServer(http.HandlerFunc(state.serveHTTP))
			defer server.Close()
			store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
			if err != nil {
				t.Fatal(err)
			}
			caps, err := Probe(context.Background(), store)
			if !errors.Is(err, ErrConditionalUnsupported) || errors.Is(err, ErrConflict) {
				t.Fatalf("Probe error = %v; want unsupported conditions rather than stale-version conflict", err)
			}
			if caps != (Capabilities{}) {
				t.Fatalf("unsafe capabilities enabled: %+v", caps)
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if len(state.objects) != 0 {
				t.Fatal("verified, nonce-bearing probe was not cleaned up")
			}
			for _, condition := range state.deleteConditions {
				if condition == "" {
					t.Fatal("probe issued an unconditional delete")
				}
			}
		})
	}
}

func TestProbeIgnoredCreatePreservesUnverifiedReplacement(t *testing.T) {
	for _, stage := range []string{"before readback", "before cleanup"} {
		t.Run(stage, func(t *testing.T) {
			base := NewMemory()
			store := &ignoredCreateReplacementStore{Store: base, stage: stage}
			caps, err := Probe(context.Background(), store)
			if !errors.Is(err, ErrConditionalUnsupported) || caps != (Capabilities{}) {
				t.Fatalf("Probe = %+v, %v; want unsupported conditions", caps, err)
			}
			if store.deletes != 0 {
				t.Fatalf("cleanup attempted %d deletes of an unverified replacement", store.deletes)
			}
			entries, err := base.List(context.Background(), "")
			if err != nil || len(entries) != 1 {
				t.Fatalf("foreign object disappeared: %+v, %v", entries, err)
			}
			body, _, err := base.Open(context.Background(), entries[0].Path, "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(body)
			_ = body.Close()
			if err != nil || !bytes.Equal(got, store.foreign) {
				t.Fatalf("foreign contents changed: %q, %v", got, err)
			}
		})
	}
}

type ignoredCreateReplacementStore struct {
	Store
	stage              string
	replaced, accepted bool
	foreign            []byte
	deletes            int
}

func (s *ignoredCreateReplacementStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond Condition) (Entry, error) {
	_, statErr := s.Store.Stat(ctx, key)
	if cond.IfNoneMatch && statErr == nil {
		s.accepted = true
		cond.IfNoneMatch = false
	}
	e, err := s.Store.Put(ctx, key, body, size, cond)
	if err == nil && s.accepted {
		if s.stage == "before readback" {
			s.replace(ctx, key, size)
		}
		e.ETag = ""
	}
	return e, err
}

func (s *ignoredCreateReplacementStore) replace(ctx context.Context, key string, size int64) {
	s.replaced = true
	s.foreign = []byte(strings.Repeat("x", int(size)))
	_, _ = s.Store.Put(ctx, key, bytes.NewReader(s.foreign), size, Condition{})
}

func (s *ignoredCreateReplacementStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	body, entry, err := s.Store.Open(ctx, key, etag)
	if err == nil && s.accepted && !s.replaced && s.stage == "before cleanup" {
		s.replace(ctx, key, entry.Size)
	}
	return body, entry, err
}

func (s *ignoredCreateReplacementStore) Delete(ctx context.Context, key string, cond Condition) error {
	s.deletes++
	return s.Store.Delete(ctx, key, cond)
}

func TestProbeMultipartCreateCleanupRechecksContent(t *testing.T) {
	base := NewMemory().(*memoryStore)
	multipart := &probeMultipartStore{Store: base, uploads: make(map[string]*probeMultipartUpload)}
	store := &multipartSameTagReplacementStore{probeMultipartStore: multipart, base: base}
	caps, err := Probe(context.Background(), store)
	if err == nil || caps != (Capabilities{}) {
		t.Fatalf("Probe = %+v, %v; want unverified cleanup failure", caps, err)
	}
	if !store.replaced || store.deletedReplacement {
		t.Fatal("multipart create cleanup deleted unverified content")
	}
	entries, err := base.List(context.Background(), "")
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Path, "-multipart-create") {
		t.Fatalf("replacement did not survive cleanup: %+v, %v", entries, err)
	}
}

type multipartSameTagReplacementStore struct {
	*probeMultipartStore
	base                         *memoryStore
	replaced, deletedReplacement bool
}

func (s *multipartSameTagReplacementStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	body, entry, err := s.Store.Open(ctx, key, etag)
	if err == nil && !s.replaced && strings.HasSuffix(key, "-multipart-create") {
		s.base.mu.Lock()
		obj := s.base.objects[key]
		changed := append([]byte(nil), obj.data...)
		changed[0] ^= 0xff
		obj.data = changed
		s.base.objects[key] = obj
		s.base.mu.Unlock()
		s.replaced = true
	}
	return body, entry, err
}

func (s *multipartSameTagReplacementStore) Delete(ctx context.Context, key string, cond Condition) error {
	if s.replaced && strings.HasSuffix(key, "-multipart-create") {
		s.deletedReplacement = true
	}
	return s.Store.Delete(ctx, key, cond)
}
