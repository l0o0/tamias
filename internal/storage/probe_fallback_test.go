package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestProbeRecoversMissingPutETag(t *testing.T) {
	for _, stage := range []string{"create", "update"} {
		for _, metadata := range []string{"empty", "weak"} {
			t.Run(stage+"/"+metadata, func(t *testing.T) {
				base := NewMemory()
				store := &missingPutETagStore{Store: base, stage: stage, metadata: metadata}

				caps, err := Probe(context.Background(), store)
				if err != nil {
					t.Fatalf("Probe failed: %v", err)
				}
				if !caps.ConditionalWrite || !caps.ConditionalDelete {
					t.Fatalf("capabilities = %#v; conditional write/delete should pass", caps)
				}
				if !store.hidden {
					t.Fatalf("test did not hide a successful %s Put ETag", stage)
				}
				if store.conditionalOpenCalls == 0 {
					t.Fatal("Probe did not conditionally open the Stat candidate ETag")
				}
				entries, err := base.List(context.Background(), "")
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("probe object was not cleaned after successful fallback: %#v", entries)
				}
			})
		}
	}
}

type missingPutETagStore struct {
	Store
	stage                string
	metadata             string
	hidden               bool
	conditionalOpenCalls int
}

func (s *missingPutETagStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	hide := !s.hidden && s.stage == "create" && condition.IfNoneMatch
	if !hide && !s.hidden && s.stage == "update" && condition.IfMatch != "" {
		current, err := s.Store.Stat(ctx, key)
		hide = err == nil && current.ETag == condition.IfMatch
	}
	e, err := s.Store.Put(ctx, key, body, size, condition)
	if err == nil && hide {
		s.hidden = true
		e.ETag = hiddenProbeETag(e.ETag, s.metadata)
	}
	return e, err
}

func (s *missingPutETagStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	if etag != "" {
		s.conditionalOpenCalls++
	}
	return s.Store.Open(ctx, key, etag)
}

func hiddenProbeETag(etag, metadata string) string {
	if metadata == "weak" {
		return "W/" + etag
	}
	return ""
}

func TestProbeDoesNotDeleteUnverifiedFallbackRevision(t *testing.T) {
	for _, hazard := range []string{"equal-size foreign payload at Stat", "equal-size revision changes after Stat"} {
		t.Run(hazard, func(t *testing.T) {
			base := NewMemory()
			store := &fallbackHazardStore{Store: base, hazard: hazard}

			_, err := Probe(context.Background(), store)
			if err == nil {
				t.Fatal("Probe succeeded with an unverified Stat candidate")
			}
			if store.deleteCalls != 0 {
				t.Fatalf("Probe attempted %d cleanup deletes using an unverified revision", store.deleteCalls)
			}
			entries, listErr := base.List(context.Background(), "")
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(entries) != 1 {
				t.Fatalf("unverified newer object should remain untouched, got %#v", entries)
			}
			body, _, openErr := base.Open(context.Background(), entries[0].Path, "")
			if openErr != nil {
				t.Fatal(openErr)
			}
			got, readErr := io.ReadAll(body)
			closeErr := body.Close()
			if readErr != nil || closeErr != nil || !bytes.Equal(got, store.foreignContent) {
				t.Fatalf("surviving object content differs from concurrent foreign payload (read=%v close=%v)", readErr, closeErr)
			}
		})
	}
}

type fallbackHazardStore struct {
	Store
	hazard         string
	armed          bool
	changed        bool
	deleteCalls    int
	foreignContent []byte
}

func (s *fallbackHazardStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	creating := !s.armed && condition.IfNoneMatch
	if creating {
		position, seekErr := body.Seek(0, io.SeekCurrent)
		if seekErr != nil {
			return Entry{}, seekErr
		}
		uploaded, readErr := io.ReadAll(body)
		if readErr != nil {
			return Entry{}, readErr
		}
		if _, seekErr = body.Seek(position, io.SeekStart); seekErr != nil {
			return Entry{}, seekErr
		}
		s.foreignContent = append([]byte(nil), uploaded...)
		if len(s.foreignContent) != 0 {
			s.foreignContent[0] ^= 0xff
		}
	}
	e, err := s.Store.Put(ctx, key, body, size, condition)
	if err != nil || s.armed || !condition.IfNoneMatch {
		return e, err
	}
	s.armed = true
	if s.hazard == "equal-size foreign payload at Stat" {
		if _, err := s.Store.Put(ctx, key, bytes.NewReader(s.foreignContent), int64(len(s.foreignContent)), Condition{}); err != nil {
			return Entry{}, err
		}
		s.changed = true
	}
	e.ETag = ""
	return e, nil
}

func (s *fallbackHazardStore) Stat(ctx context.Context, key string) (Entry, error) {
	e, err := s.Store.Stat(ctx, key)
	if err == nil && s.hazard == "equal-size revision changes after Stat" && s.armed && !s.changed {
		s.changed = true
		if _, putErr := s.Store.Put(ctx, key, bytes.NewReader(s.foreignContent), int64(len(s.foreignContent)), Condition{}); putErr != nil {
			return Entry{}, putErr
		}
	}
	return e, err
}

func (s *fallbackHazardStore) Delete(ctx context.Context, key string, condition Condition) error {
	s.deleteCalls++
	return s.Store.Delete(ctx, key, condition)
}

func TestProbeStillRejectsIgnoredConditionsAfterETagFallback(t *testing.T) {
	for _, bypass := range []string{"If-None-Match", "wrong If-Match PUT", "wrong If-Match DELETE"} {
		t.Run(bypass, func(t *testing.T) {
			store := &conditionBypassAfterFallbackStore{Store: NewMemory(), bypass: bypass}
			caps, err := Probe(context.Background(), store)
			if err == nil {
				t.Fatalf("Probe capabilities=%#v; want conditional capability rejection", caps)
			}
			if caps.ConditionalWrite || caps.ConditionalDelete {
				t.Fatalf("Probe reported rejected conditional operations as supported: %#v", caps)
			}
			if !store.hiddenInitialETag {
				t.Fatal("test did not exercise missing-ETag fallback")
			}
		})
	}
}

type conditionBypassAfterFallbackStore struct {
	Store
	bypass            string
	hiddenInitialETag bool
}

func (s *conditionBypassAfterFallbackStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	current, statErr := s.Store.Stat(ctx, key)
	exists := statErr == nil
	initialCreate := condition.IfNoneMatch && !exists && errors.Is(statErr, ErrNotFound)

	if s.bypass == "If-None-Match" && condition.IfNoneMatch && exists {
		condition.IfNoneMatch = false
	}
	if s.bypass == "wrong If-Match PUT" && condition.IfMatch != "" && exists && condition.IfMatch != current.ETag {
		condition.IfMatch = ""
	}
	e, err := s.Store.Put(ctx, key, body, size, condition)
	if err == nil && initialCreate && !s.hiddenInitialETag {
		s.hiddenInitialETag = true
		e.ETag = ""
	}
	return e, err
}

func (s *conditionBypassAfterFallbackStore) Delete(ctx context.Context, key string, condition Condition) error {
	if s.bypass == "wrong If-Match DELETE" && condition.IfMatch != "" {
		current, err := s.Store.Stat(ctx, key)
		if err == nil && condition.IfMatch != current.ETag {
			condition.IfMatch = ""
		}
	}
	return s.Store.Delete(ctx, key, condition)
}

func TestProbeRejectsConstantETagWhenResolvedAfterMissingPutETag(t *testing.T) {
	base := NewMemory()
	constant := constantETagStore{Store: base}
	store := &hideAllSuccessfulPutETagsStore{Store: constant}

	caps, err := Probe(context.Background(), store)
	if err == nil || caps.ConditionalWrite {
		t.Fatalf("Probe capabilities=%#v err=%v; want unchanged resolved ETag rejected", caps, err)
	}
	entries, listErr := base.List(context.Background(), "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("probe object was not cleaned after unchanged ETag rejection: %#v", entries)
	}
}

type hideAllSuccessfulPutETagsStore struct{ Store }

func (s *hideAllSuccessfulPutETagsStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	e, err := s.Store.Put(ctx, key, body, size, condition)
	if err == nil {
		e.ETag = ""
	}
	return e, err
}

func TestWebDAVProbeRecoversPUTWithoutETagAndCleansTemporaryObject(t *testing.T) {
	serverState := &probeDAVState{objects: make(map[string]probeDAVObject)}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local TCP listener unavailable: %v", err)
	}
	server := &httptest.Server{Config: &http.Server{Handler: http.HandlerFunc(serverState.serveHTTP)}, Listener: listener}
	server.Start()
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}

	caps, err := Probe(context.Background(), store)
	if err != nil {
		t.Fatalf("Probe against WebDAV server without PUT ETags failed: %v", err)
	}
	if !caps.ConditionalWrite || !caps.ConditionalDelete {
		t.Fatalf("capabilities = %#v; conditional write/delete should pass", caps)
	}
	serverState.mu.Lock()
	defer serverState.mu.Unlock()
	if len(serverState.objects) != 0 {
		t.Fatalf("WebDAV probe object remained after success: %#v", serverState.objects)
	}
	if serverState.conditionalGets == 0 {
		t.Fatal("Probe did not verify the Stat candidate with a conditional WebDAV GET")
	}
}

func TestProbeMultipartCleanupPreservesConcurrentRevision(t *testing.T) {
	for _, stage := range []string{"after rejected completion", "after successful completion"} {
		t.Run(stage, func(t *testing.T) {
			base := NewMemory()
			multipart := &probeMultipartStore{Store: base, uploads: make(map[string]*probeMultipartUpload)}
			store := &multipartRevisionRaceStore{probeMultipartStore: multipart, stage: stage}

			_, err := Probe(context.Background(), store)
			if err == nil {
				t.Fatal("Probe succeeded after a concurrent revision replaced the multipart result")
			}
			entries, listErr := base.List(context.Background(), "")
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(entries) != 1 {
				t.Fatalf("concurrent object should survive cleanup, got %#v", entries)
			}
			body, _, openErr := base.Open(context.Background(), entries[0].Path, "")
			if openErr != nil {
				t.Fatal(openErr)
			}
			got, readErr := io.ReadAll(body)
			closeErr := body.Close()
			if readErr != nil || closeErr != nil || string(got) != "foreign multipart writer" {
				t.Fatalf("surviving object content = %q (read=%v close=%v), want foreign multipart writer", got, readErr, closeErr)
			}
		})
	}
}

type multipartRevisionRaceStore struct {
	*probeMultipartStore
	stage   string
	mutated bool
}

func (s *multipartRevisionRaceStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart, condition Condition) (Entry, error) {
	before, beforeErr := s.Store.Stat(ctx, key)
	correctMatch := condition.IfMatch != "" && beforeErr == nil && condition.IfMatch == before.ETag
	entry, err := s.probeMultipartStore.CompleteMultipart(ctx, key, uploadID, parts, condition)
	shouldMutate := !s.mutated && (s.stage == "after rejected completion" && !correctMatch && err != nil ||
		s.stage == "after successful completion" && correctMatch && err == nil)
	if shouldMutate {
		s.mutated = true
		foreign := []byte("foreign multipart writer")
		if _, putErr := s.Store.Put(ctx, key, bytes.NewReader(foreign), int64(len(foreign)), Condition{}); putErr != nil {
			return Entry{}, putErr
		}
	}
	return entry, err
}

func TestProbeMultipartCompletionWithoutETagUsesReadbackForCleanup(t *testing.T) {
	base := NewMemory()
	multipart := &probeMultipartStore{Store: base, uploads: make(map[string]*probeMultipartUpload)}
	store := &multipartMissingETagStore{probeMultipartStore: multipart}

	caps, err := Probe(context.Background(), store)
	if err != nil {
		t.Fatalf("Probe failed with omitted multipart completion ETags: %v", err)
	}
	if !caps.MultipartConditional {
		t.Fatalf("MultipartConditional = false; capabilities %#v", caps)
	}
	if store.missingETagSuccesses != 2 || store.fallbackStats != 2 || store.fallbackOpens != 2 {
		t.Fatalf("multipart readbacks: omitted=%d stat=%d conditional open=%d; want two verified fallbacks", store.missingETagSuccesses, store.fallbackStats, store.fallbackOpens)
	}
	entries, listErr := base.List(context.Background(), "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("multipart probe objects were not cleaned: %#v", entries)
	}
}

func TestProbeCleansNewMultipartObjectWhenCompletionReplyIsLost(t *testing.T) {
	base := NewMemory()
	multipart := &probeMultipartStore{Store: base, uploads: make(map[string]*probeMultipartUpload)}
	store := &multipartLostCreateReplyStore{probeMultipartStore: multipart}

	_, err := Probe(context.Background(), store)
	if err != nil {
		t.Fatalf("Probe failed after recovering a committed multipart object: %v", err)
	}
	if !store.lostReply {
		t.Fatal("test did not simulate a committed create with a lost completion reply")
	}
	if len(store.createDeleteConditions) != 1 || store.createDeleteConditions[0] == "" {
		t.Fatalf("new multipart object cleanup conditions = %#v; want one conditional delete with a verified ETag", store.createDeleteConditions)
	}
	entries, listErr := base.List(context.Background(), "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("multipart probe objects remained after recovered completion: %#v", entries)
	}
}

type multipartLostCreateReplyStore struct {
	*probeMultipartStore
	lostReply              bool
	createDeleteConditions []string
}

func (s *multipartLostCreateReplyStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart, condition Condition) (Entry, error) {
	entry, err := s.probeMultipartStore.CompleteMultipart(ctx, key, uploadID, parts, condition)
	if err == nil && !s.lostReply && strings.HasSuffix(key, "-multipart-create") && condition.IfNoneMatch {
		s.lostReply = true
		return Entry{}, errors.New("simulated lost multipart completion reply")
	}
	return entry, err
}

func (s *multipartLostCreateReplyStore) Delete(ctx context.Context, key string, condition Condition) error {
	if strings.HasSuffix(key, "-multipart-create") {
		s.createDeleteConditions = append(s.createDeleteConditions, condition.IfMatch)
	}
	return s.probeMultipartStore.Store.Delete(ctx, key, condition)
}

type multipartMissingETagStore struct {
	*probeMultipartStore
	missingETagSuccesses int
	fallbackStats        int
	fallbackOpens        int
	pendingReadback      bool
	candidateETag        string
}

func (s *multipartMissingETagStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart, condition Condition) (Entry, error) {
	entry, err := s.probeMultipartStore.CompleteMultipart(ctx, key, uploadID, parts, condition)
	if err == nil {
		s.missingETagSuccesses++
		s.pendingReadback = true
		entry.ETag = ""
	}
	return entry, err
}

func (s *multipartMissingETagStore) Stat(ctx context.Context, key string) (Entry, error) {
	entry, err := s.probeMultipartStore.Store.Stat(ctx, key)
	if err == nil && s.pendingReadback {
		s.fallbackStats++
		s.candidateETag = entry.ETag
	}
	return entry, err
}

func (s *multipartMissingETagStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	if s.pendingReadback && s.candidateETag != "" {
		if etag == s.candidateETag {
			s.fallbackOpens++
		}
		s.pendingReadback = false
		s.candidateETag = ""
	}
	return s.probeMultipartStore.Store.Open(ctx, key, etag)
}

type probeDAVObject struct {
	data []byte
	etag string
}

type probeDAVState struct {
	mu               sync.Mutex
	objects          map[string]probeDAVObject
	nextETag         int
	conditionalGets  int
	ignoreCondition  string
	deleteConditions []string
}

func (s *probeDAVState) serveHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.Trim(r.URL.Path, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	object, exists := s.objects[key]

	switch r.Method {
	case http.MethodPut:
		if (r.Header.Get("If-None-Match") == "*" && exists && s.ignoreCondition != "If-None-Match" && s.ignoreCondition != "all") ||
			(r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != object.etag) && s.ignoreCondition != "If-Match PUT" && s.ignoreCondition != "all") {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		s.nextETag++
		etag := fmt.Sprintf(`"dav-%d"`, s.nextETag)
		s.objects[key] = probeDAVObject{data: data, etag: etag}
		// Intentionally leave the successful PUT response without an ETag.
		w.WriteHeader(http.StatusCreated)
	case "PROPFIND":
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/%s</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>%s</d:getetag><d:getcontentlength>%d</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, key, object.etag, len(object.data))
	case http.MethodGet:
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("If-Match") != "" {
			s.conditionalGets++
			if r.Header.Get("If-Match") != object.etag && s.ignoreCondition != "all" {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		w.Header().Set("ETag", object.etag)
		if r.Header.Get("Range") == "bytes=0-0" {
			if len(object.data) == 0 {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", "bytes 0-0/"+strconv.Itoa(len(object.data)))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(object.data[:1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(object.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(object.data)
	case http.MethodDelete:
		s.deleteConditions = append(s.deleteConditions, r.Header.Get("If-Match"))
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != object.etag && s.ignoreCondition != "If-Match DELETE" && s.ignoreCondition != "all" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
