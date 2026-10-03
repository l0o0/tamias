package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type protocolBackend struct {
	*fakeBackend
	mu             sync.Mutex
	rangeStart     int64
	rangeLength    int64
	rangeETag      string
	rangeCalls     int
	copyCalls      int
	moveCalls      int
	partialMove    bool
	deleteTreeCall string
	locks          map[string]storage.DAVLock
	nextLock       int
	seenLockTokens [][]string
}

func newProtocolBackend() *protocolBackend {
	return &protocolBackend{fakeBackend: newFakeBackend(), locks: make(map[string]storage.DAVLock)}
}

func (f *protocolBackend) OpenRange(_ context.Context, key, etag string, start, length int64) (io.ReadCloser, storage.Entry, error) {
	f.mu.Lock()
	f.rangeCalls++
	f.rangeStart, f.rangeLength, f.rangeETag = start, length, etag
	f.mu.Unlock()
	f.fakeBackend.mu.Lock()
	defer f.fakeBackend.mu.Unlock()
	obj, ok := f.fakeBackend.objects[key]
	if !ok {
		return nil, storage.Entry{}, storage.ErrNotFound
	}
	if obj.entry.ETag != etag {
		return nil, storage.Entry{}, storage.ErrConflict
	}
	if start < 0 || length <= 0 || start+length > int64(len(obj.data)) {
		return nil, storage.Entry{}, storage.ErrInvalidRange
	}
	return io.NopCloser(bytes.NewReader(obj.data[start : start+length])), storage.Entry{Path: key, Name: pathBase(key), Size: length, ETag: obj.entry.ETag, Modified: obj.entry.Modified}, nil
}

func (f *protocolBackend) Copy(_ context.Context, src, dst string, overwrite bool) (storage.Entry, error) {
	f.mu.Lock()
	f.copyCalls++
	f.mu.Unlock()
	f.fakeBackend.mu.Lock()
	defer f.fakeBackend.mu.Unlock()
	object, exists := f.fakeBackend.objects[src]
	if !exists {
		return storage.Entry{}, storage.ErrNotFound
	}
	if _, exists := f.fakeBackend.objects[dst]; exists && !overwrite {
		return storage.Entry{}, storage.ErrConflict
	}
	object.entry.Path, object.entry.Name = dst, pathBase(dst)
	object.entry.ETag = "copied-" + object.entry.ETag
	object.data = append([]byte(nil), object.data...)
	f.fakeBackend.objects[dst] = object
	return object.entry, nil
}

func (f *protocolBackend) Move(ctx context.Context, src, dst string, overwrite bool) (storage.Entry, error) {
	f.mu.Lock()
	f.moveCalls++
	partial := f.partialMove
	f.mu.Unlock()
	if partial {
		return storage.Entry{}, storage.PartialOperationError{Operation: "MOVE", Err: errors.New("source cleanup failed")}
	}
	entry, err := f.Copy(ctx, src, dst, overwrite)
	if err != nil {
		return storage.Entry{}, err
	}
	f.fakeBackend.mu.Lock()
	delete(f.fakeBackend.objects, src)
	f.fakeBackend.mu.Unlock()
	return entry, nil
}

func (f *protocolBackend) DeleteTree(_ context.Context, key string) error {
	f.mu.Lock()
	f.deleteTreeCall = key
	f.mu.Unlock()
	f.fakeBackend.mu.Lock()
	defer f.fakeBackend.mu.Unlock()
	for objectKey := range f.fakeBackend.objects {
		if objectKey == key || strings.HasPrefix(objectKey, key+"/") {
			delete(f.fakeBackend.objects, objectKey)
		}
	}
	for dirKey := range f.fakeBackend.dirs {
		if dirKey == key || strings.HasPrefix(dirKey, key+"/") {
			delete(f.fakeBackend.dirs, dirKey)
		}
	}
	return nil
}

func (f *protocolBackend) AcquireDAVLock(_ context.Context, key, owner string, depth bool, timeout time.Duration) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.locks[key]; exists {
		return "", time.Time{}, storage.ErrLocked
	}
	f.nextLock++
	token := fmt.Sprintf("opaquelocktoken:test-%d", f.nextLock)
	expires := time.Now().Add(timeout)
	f.locks[key] = storage.DAVLock{Token: token, Owner: owner, RootKey: key, DepthInfinity: depth, Expires: expires}
	return token, expires, nil
}

func (f *protocolBackend) RefreshDAVLock(_ context.Context, key, token string, timeout time.Duration) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lock, exists := f.locks[key]
	if !exists || lock.Token != token {
		return time.Time{}, storage.ErrConflict
	}
	lock.Expires = time.Now().Add(timeout)
	f.locks[key] = lock
	return lock.Expires, nil
}

func (f *protocolBackend) UnlockDAVLock(_ context.Context, key, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lock, exists := f.locks[key]
	if !exists || lock.Token != token {
		return storage.ErrConflict
	}
	delete(f.locks, key)
	return nil
}

func (f *protocolBackend) DAVLocks(_ context.Context, key string) ([]storage.DAVLock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []storage.DAVLock
	for lockKey, lock := range f.locks {
		if lockKey == key || lock.DepthInfinity && strings.HasPrefix(key, lockKey+"/") {
			result = append(result, lock)
		}
	}
	return result, nil
}

func (f *protocolBackend) Put(ctx context.Context, key string, body io.Reader, size int64, cond storage.Condition) (storage.Entry, error) {
	tokens := storage.LockTokens(ctx)
	f.mu.Lock()
	f.seenLockTokens = append(f.seenLockTokens, append([]string(nil), tokens...))
	for lockKey, lock := range f.locks {
		if lockKey == key || lock.DepthInfinity && strings.HasPrefix(key, lockKey+"/") {
			found := false
			for _, token := range tokens {
				found = found || token == lock.Token
			}
			if !found {
				f.mu.Unlock()
				return storage.Entry{}, storage.ErrLocked
			}
		}
	}
	f.mu.Unlock()
	return f.fakeBackend.Put(ctx, key, body, size, cond)
}

func TestGetRangeAndIfRangeUseStableRevision(t *testing.T) {
	backend := newProtocolBackend()
	backend.addDir("vault")
	backend.addFile("vault/doc.txt", []byte("payload"), "revision-a")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)

	r := authorizedRequest(http.MethodGet, "/doc.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	r.Header.Set("If-Range", `"revision-a"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "ayl" || w.Header().Get("Content-Range") != "bytes 1-3/7" || w.Header().Get("Content-Length") != "3" {
		t.Fatalf("range response: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if backend.rangeCalls != 1 || backend.rangeStart != 1 || backend.rangeLength != 3 || backend.rangeETag != "revision-a" {
		t.Fatalf("range backend call: calls=%d start=%d length=%d ETag=%q", backend.rangeCalls, backend.rangeStart, backend.rangeLength, backend.rangeETag)
	}

	r = authorizedRequest(http.MethodGet, "/doc.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	r.Header.Set("If-Range", `"old"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "payload" || backend.rangeCalls != 1 {
		t.Fatalf("mismatched If-Range should return full object: status=%d body=%q rangeCalls=%d", w.Code, w.Body.String(), backend.rangeCalls)
	}

	r = authorizedRequest(http.MethodGet, "/doc.txt", nil)
	r.Header.Set("Range", "bytes=9-")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestedRangeNotSatisfiable || w.Header().Get("Content-Range") != "bytes */7" {
		t.Fatalf("unsatisfiable range: status=%d Content-Range=%q", w.Code, w.Header().Get("Content-Range"))
	}

	r = authorizedRequest(http.MethodGet, "/doc.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	r.Header.Set("If-None-Match", `"revision-a"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified || backend.rangeCalls != 1 {
		t.Fatalf("If-None-Match should precede Range: status=%d rangeCalls=%d", w.Code, backend.rangeCalls)
	}

	r = authorizedRequest(http.MethodHead, "/doc.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != "7" || backend.rangeCalls != 1 {
		t.Fatalf("HEAD must report full metadata: status=%d length=%q rangeCalls=%d", w.Code, w.Header().Get("Content-Length"), backend.rangeCalls)
	}
}

func TestRangeWithoutStableETagIsNotAdvertised(t *testing.T) {
	backend := newProtocolBackend()
	backend.addDir("vault")
	backend.addFile("vault/doc.txt", []byte("payload"), "")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	r := authorizedRequest(http.MethodGet, "/doc.txt", nil)
	r.Header.Set("Range", "bytes=0-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "payload" || w.Header().Get("Accept-Ranges") != "none" || backend.rangeCalls != 0 {
		t.Fatalf("unstable object range response: status=%d accept-ranges=%q body=%q calls=%d", w.Code, w.Header().Get("Accept-Ranges"), w.Body.String(), backend.rangeCalls)
	}
}

func TestDestinationScopeCopyMoveAndPartialMove(t *testing.T) {
	backend := newProtocolBackend()
	backend.addDir("vault")
	backend.addFile("vault/src.txt", []byte("source"), "src")
	backend.addFile("vault/existing.txt", []byte("old"), "old")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)

	r := authorizedRequest("COPY", "/src.txt", nil)
	r.Header.Set("Destination", "http://example.com/copy.txt")
	r.Header.Set("Overwrite", "F")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated || !backend.exists("vault/copy.txt") {
		t.Fatalf("COPY status=%d body=%q", w.Code, w.Body.String())
	}

	for _, destination := range []string{"/../outside", "/%2e%2e/outside", "/a%2fb", "http://other.example/outside"} {
		r = authorizedRequest("COPY", "/src.txt", nil)
		r.Header.Set("Destination", destination)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("Destination %q status=%d, want 400", destination, w.Code)
		}
	}

	r = authorizedRequest("COPY", "/src.txt", nil)
	r.Header.Set("Destination", "/existing.txt")
	r.Header.Set("Overwrite", "F")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusPreconditionFailed || backend.copyCalls != 1 {
		t.Fatalf("Overwrite F response: status=%d copy calls=%d", w.Code, backend.copyCalls)
	}

	r = authorizedRequest("COPY", "/src.txt", nil)
	r.Header.Set("Destination", "/should-not-copy.txt")
	r.Header.Set("If-Match", `"src"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented || backend.copyCalls != 1 {
		t.Fatalf("unsupported entity-tag condition reached backend: status=%d calls=%d", w.Code, backend.copyCalls)
	}

	backend.mu.Lock()
	backend.partialMove = true
	backend.mu.Unlock()
	r = authorizedRequest("MOVE", "/src.txt", nil)
	r.Header.Set("Destination", "/moved.txt")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "partially completed") || !strings.Contains(w.Body.String(), "inspect source and destination") || strings.Contains(w.Body.String(), "201 Created") {
		t.Fatalf("partial MOVE response: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestDAVLockTokensAreSharedWithCoreWritesAndDiscoverable(t *testing.T) {
	backend := newProtocolBackend()
	backend.addDir("vault")
	backend.addFile("vault/doc.txt", []byte("old"), "v1")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	lockBody := strings.NewReader(`<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope><d:locktype><d:write/></d:locktype><d:owner>reader</d:owner></d:lockinfo>`)
	r := authorizedRequest("LOCK", "/doc.txt", lockBody)
	r.Header.Set("Depth", "0")
	r.Header.Set("Timeout", "Second-600")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	token := strings.Trim(w.Header().Get("Lock-Token"), "<>")
	if w.Code != http.StatusOK || token == "" || !strings.Contains(w.Body.String(), "lockdiscovery") {
		t.Fatalf("LOCK response: status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}

	r = authorizedRequest(http.MethodPut, "/doc.txt", strings.NewReader("blocked"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 423 {
		t.Fatalf("write without lock token status=%d, want 423", w.Code)
	}

	r = authorizedRequest("PROPFIND", "/doc.txt", strings.NewReader(`<d:propfind xmlns:d="DAV:"><d:prop><d:lockdiscovery/><d:supportedlock/></d:prop></d:propfind>`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 207 || !strings.Contains(w.Body.String(), token) || !strings.Contains(w.Body.String(), "/doc.txt") {
		t.Fatalf("lock discovery: status=%d body=%q", w.Code, w.Body.String())
	}

	r = authorizedRequest(http.MethodPut, "/doc.txt", strings.NewReader("updated"))
	r.Header.Set("If", "(<"+token+">)")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("write with lock token status=%d body=%q", w.Code, w.Body.String())
	}
	backend.mu.Lock()
	seen := append([]string(nil), backend.seenLockTokens[len(backend.seenLockTokens)-1]...)
	backend.mu.Unlock()
	if len(seen) != 1 || seen[0] != token {
		t.Fatalf("Core received lock tokens %v", seen)
	}

	r = authorizedRequest("LOCK", "/doc.txt", nil)
	r.Header.Set("If", "(<"+token+">)")
	r.Header.Set("Timeout", "Second-120")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Lock-Token") != "<"+token+">" {
		t.Fatalf("LOCK refresh status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}

	r = authorizedRequest("UNLOCK", "/doc.txt", nil)
	r.Header.Set("Lock-Token", "<"+token+">")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("UNLOCK status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestLockNullRequiresExistingParentAndIfListsFailClosed(t *testing.T) {
	backend := newProtocolBackend()
	backend.addDir("vault")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	lockBody := strings.NewReader(`<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope><d:locktype><d:write/></d:locktype></d:lockinfo>`)
	r := authorizedRequest("LOCK", "/missing.txt", lockBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("lock-null resource status=%d body=%q", w.Code, w.Body.String())
	}

	r = authorizedRequest(http.MethodPut, "/missing.txt", strings.NewReader("no"))
	r.Header.Set("If", "(<opaquelocktoken:a>) (<opaquelocktoken:b>)")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented || backend.putCalls != 0 {
		t.Fatalf("complex If condition status=%d PutCalls=%d", w.Code, backend.putCalls)
	}

	r = authorizedRequest("LOCK", "/no/parent/child", strings.NewReader(`<d:lockinfo xmlns:d="DAV:"><d:lockscope><d:exclusive/></d:lockscope><d:locktype><d:write/></d:locktype></d:lockinfo>`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("lock-null without parent status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestAccessObserverAndStatsAreBoundedToHandledRequests(t *testing.T) {
	backend := newProtocolBackend()
	backend.addDir("vault")
	backend.addFile("vault/doc.txt", []byte("payload"), "v1")
	var events []AccessEvent
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault", OnAccess: func(event AccessEvent) { events = append(events, event) }}, backend)
	r := authorizedRequest(http.MethodGet, "/doc.txt", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	stats := h.Stats()
	if stats.Requests != 1 || stats.Errors != 0 || stats.BytesOut != 7 || stats.Active != 0 || len(events) != 1 {
		t.Fatalf("stats=%+v events=%+v", stats, events)
	}
	if events[0].Method != http.MethodGet || events[0].Path != "/doc.txt" || events[0].Status != http.StatusOK || events[0].BytesOut != 7 {
		t.Fatalf("unexpected access event: %+v", events[0])
	}
}
