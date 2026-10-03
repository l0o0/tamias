package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type fakeObject struct {
	entry storage.Entry
	data  []byte
}

type fakeBackend struct {
	mu sync.Mutex

	objects        map[string]fakeObject
	dirs           map[string]bool
	putErr         error
	putCalls       int
	lastKey        string
	lastSize       int64
	lastCond       storage.Condition
	lastDeleteCond storage.Condition
	openN          int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{objects: make(map[string]fakeObject), dirs: make(map[string]bool)}
}

func (f *fakeBackend) addDir(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[key] = true
}

func (f *fakeBackend) addFile(key string, data []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{entry: storage.Entry{
		Path: key, Name: pathBase(key), Size: int64(len(data)), Modified: time.Date(2026, 10, 2, 12, 34, 56, 0, time.UTC), ETag: etag,
	}, data: append([]byte(nil), data...)}
}

func (f *fakeBackend) List(_ context.Context, key string) ([]storage.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[key] {
		return nil, storage.ErrNotFound
	}
	var entries []storage.Entry
	for childKey, obj := range f.objects {
		if directChild(key, childKey) {
			entries = append(entries, obj.entry)
		}
	}
	for dirKey := range f.dirs {
		if dirKey != key && directChild(key, dirKey) {
			entries = append(entries, storage.Entry{Path: dirKey, Name: pathBase(dirKey), IsDir: true, Modified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (f *fakeBackend) Stat(_ context.Context, key string) (storage.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj, ok := f.objects[key]; ok {
		return obj.entry, nil
	}
	if f.dirs[key] {
		return storage.Entry{Path: key, Name: pathBase(key), IsDir: true, Modified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}, nil
	}
	return storage.Entry{}, storage.ErrNotFound
}

func (f *fakeBackend) Open(_ context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	if !ok {
		return nil, storage.Entry{}, storage.ErrNotFound
	}
	if etag != "" && etag != obj.entry.ETag {
		return nil, storage.Entry{}, storage.ErrConflict
	}
	f.openN++
	return io.NopCloser(bytes.NewReader(obj.data)), obj.entry, nil
}

func (f *fakeBackend) Put(_ context.Context, key string, body io.Reader, size int64, cond storage.Condition) (storage.Entry, error) {
	data, readErr := io.ReadAll(body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCalls++
	f.lastKey, f.lastSize, f.lastCond = key, size, cond
	if f.putErr != nil {
		return storage.Entry{}, f.putErr
	}
	if readErr != nil {
		return storage.Entry{}, readErr
	}
	if size >= 0 && int64(len(data)) != size {
		return storage.Entry{}, io.ErrUnexpectedEOF
	}
	old, exists := f.objects[key]
	if cond.IfNoneMatch && exists {
		return storage.Entry{}, storage.ErrConflict
	}
	if cond.IfMatch == "*" && !exists || cond.IfMatch != "" && cond.IfMatch != "*" && (!exists || old.entry.ETag != cond.IfMatch) {
		return storage.Entry{}, storage.ErrConflict
	}
	entry := storage.Entry{Path: key, Name: pathBase(key), Size: int64(len(data)), ETag: fmt.Sprintf("v%d", f.putCalls), Modified: time.Now().UTC()}
	f.objects[key] = fakeObject{entry: entry, data: data}
	return entry, nil
}

func (f *fakeBackend) Delete(_ context.Context, key string, cond storage.Condition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDeleteCond = cond
	obj, ok := f.objects[key]
	if !ok {
		return storage.ErrNotFound
	}
	if cond.IfNoneMatch || (cond.IfMatch != "" && cond.IfMatch != "*" && obj.entry.ETag != cond.IfMatch) {
		return storage.ErrConflict
	}
	delete(f.objects, key)
	return nil
}

func (f *fakeBackend) Mkdir(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[key] || f.objects[key].entry.Path != "" {
		return storage.ErrConflict
	}
	f.dirs[key] = true
	return nil
}

func (f *fakeBackend) exists(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func directChild(parent, child string) bool {
	prefix := parent
	if prefix != "" {
		prefix += "/"
	}
	if !strings.HasPrefix(child, prefix) {
		return false
	}
	rest := strings.TrimPrefix(child, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

func pathBase(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

func authorizedRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.SetBasicAuth("web", "secret")
	return r
}

func TestBasicAuthAndReadOnlyMutations(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault", ReadOnly: true}, backend)

	unauthorized := httptest.NewRequest(http.MethodGet, "/doc.txt", nil)
	unauthorizedResult := httptest.NewRecorder()
	h.ServeHTTP(unauthorizedResult, unauthorized)
	if unauthorizedResult.Code != http.StatusUnauthorized || unauthorizedResult.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated request: status=%d headers=%v", unauthorizedResult.Code, unauthorizedResult.Header())
	}

	for _, method := range []string{http.MethodPut, "MKCOL", http.MethodDelete} {
		request := authorizedRequest(method, "/doc.txt", strings.NewReader("x"))
		result := httptest.NewRecorder()
		h.ServeHTTP(result, request)
		if result.Code != http.StatusForbidden {
			t.Errorf("read-only %s status = %d, want 403", method, result.Code)
		}
	}
	if backend.putCalls != 0 {
		t.Fatalf("read-only request reached backend Put %d times", backend.putCalls)
	}
}

func TestPathTraversalAndEncodedSeparatorsAreRejected(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	for _, target := range []string{"/../secret", "/%2e%2e/secret", "/a%2fb", "/a%5Cb", "/%00"} {
		t.Run(target, func(t *testing.T) {
			r := authorizedRequest(http.MethodGet, target, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", w.Code, w.Body.String())
			}
			if backend.openN != 0 {
				t.Fatalf("invalid path reached Open")
			}
		})
	}
}

func TestPropfindDepthOneUsesGatewayRelativeDAVXML(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("private")
	backend.addDir("private/sub")
	backend.addFile("private/a&b.txt", []byte("hello"), "etag-1")
	backend.addFile("private/sub/hidden.txt", []byte("hidden"), "etag-2")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "private"}, backend)

	r := authorizedRequest("PROPFIND", "/", nil)
	r.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 207 {
		t.Fatalf("PROPFIND status = %d, want 207: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("unexpected content type %q", w.Header().Get("Content-Type"))
	}
	if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "hidden.txt") {
		t.Fatalf("response leaked storage prefix or descendant: %s", w.Body.String())
	}
	var parsed struct {
		XMLName   xml.Name
		Responses []struct {
			Href     string `xml:"href"`
			Propstat struct {
				Prop struct {
					Type struct {
						Collection *struct{} `xml:"collection"`
					} `xml:"resourcetype"`
					Length int64  `xml:"getcontentlength"`
					ETag   string `xml:"getetag"`
				} `xml:"prop"`
				Status string `xml:"status"`
			} `xml:"propstat"`
		} `xml:"response"`
	}
	if err := xmlUnmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid XML: %v\n%s", err, w.Body.String())
	}
	if parsed.XMLName.Space != "DAV:" || parsed.XMLName.Local != "multistatus" {
		t.Fatalf("unexpected XML root namespace/name: %+v", parsed.XMLName)
	}
	if len(parsed.Responses) != 3 {
		t.Fatalf("got %d responses, want root and two direct children", len(parsed.Responses))
	}
	hrefs := make(map[string]bool)
	for _, resp := range parsed.Responses {
		hrefs[resp.Href] = true
		if resp.Propstat.Status != "HTTP/1.1 200 OK" {
			t.Errorf("propstat status = %q", resp.Propstat.Status)
		}
		if resp.Href == "/a&b.txt" {
			if resp.Propstat.Prop.Length != 5 || resp.Propstat.Prop.ETag != `"etag-1"` {
				t.Errorf("file properties = length %d, etag %q", resp.Propstat.Prop.Length, resp.Propstat.Prop.ETag)
			}
		}
		if resp.Href == "/" && resp.Propstat.Prop.Type.Collection == nil {
			t.Errorf("root collection property missing")
		}
	}
	if !hrefs["/"] || !hrefs["/a&b.txt"] || !hrefs["/sub/"] {
		t.Fatalf("unexpected hrefs: %v", hrefs)
	}
	if !strings.Contains(w.Body.String(), "a&amp;b.txt") {
		t.Fatalf("href was not XML escaped: %s", w.Body.String())
	}

	r = authorizedRequest("PROPFIND", "/", nil)
	r.Header.Set("Depth", "0")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 207 || strings.Count(w.Body.String(), "<response>") != 1 {
		t.Fatalf("Depth 0 should return only the target: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDepthInfinityIsRejected(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	r := authorizedRequest("PROPFIND", "/", nil)
	r.Header.Set("Depth", "infinity")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestGetAndHeadETagConditions(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	backend.addFile("vault/doc.txt", []byte("payload"), "version-a")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)

	r := authorizedRequest(http.MethodGet, "/doc.txt", nil)
	r.Header.Set("If-None-Match", `W/"version-a"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified || backend.openN != 0 {
		t.Fatalf("If-None-Match result: status=%d open calls=%d", w.Code, backend.openN)
	}

	r = authorizedRequest(http.MethodHead, "/doc.txt", nil)
	r.Header.Set("If-Match", `"wrong"`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("If-Match status = %d, want 412", w.Code)
	}

	r = authorizedRequest(http.MethodHead, "/doc.txt", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != "7" || backend.openN != 0 {
		t.Fatalf("HEAD result: status=%d length=%q open calls=%d", w.Code, w.Header().Get("Content-Length"), backend.openN)
	}
}

type truncatedReadBackend struct{ *fakeBackend }

func (b truncatedReadBackend) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	entry, err := b.fakeBackend.Stat(ctx, key)
	if err != nil {
		return nil, storage.Entry{}, err
	}
	return io.NopCloser(strings.NewReader("cut")), entry, nil
}

func TestTruncatedReadIsRecordedAsPartialFailure(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	backend.addFile("vault/file.bin", []byte("0123456789"), "etag")
	var event AccessEvent
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault", OnAccess: func(e AccessEvent) { event = e }}, truncatedReadBackend{backend})
	r := authorizedRequest(http.MethodGet, "/file.bin", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "cut" {
		t.Fatalf("GET status=%d body=%q", w.Code, w.Body.String())
	}
	if !event.Partial || event.Status != http.StatusOK || event.BytesOut != 3 {
		t.Fatalf("access event did not capture short read: %+v", event)
	}
	stats := h.Stats()
	if stats.Errors != 1 || stats.BytesOut != 3 {
		t.Fatalf("gateway stats did not capture short read: %+v", stats)
	}
}

func TestPutForwardsContentLengthAndConditions(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	backend.addFile("vault/doc.txt", []byte("old"), "old-etag")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)

	r := authorizedRequest(http.MethodPut, "/doc.txt", strings.NewReader("new-data"))
	r.Header.Set("If-Match", `"old-etag"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204: %s", w.Code, w.Body.String())
	}
	if backend.lastKey != "vault/doc.txt" || backend.lastSize != 8 || backend.lastCond.IfMatch != "old-etag" {
		t.Fatalf("backend arguments: key=%q size=%d cond=%+v", backend.lastKey, backend.lastSize, backend.lastCond)
	}

	r = authorizedRequest(http.MethodPut, "/new.txt", strings.NewReader("new"))
	r.Header.Set("If-None-Match", "*")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated || !backend.lastCond.IfNoneMatch {
		t.Fatalf("If-None-Match PUT: status=%d condition=%+v", w.Code, backend.lastCond)
	}

	r = authorizedRequest(http.MethodPut, "/doc.txt", strings.NewReader("wildcard"))
	r.Header.Set("If-Match", "*")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || backend.lastCond.IfMatch != "v1" {
		t.Fatalf("If-Match wildcard PUT: status=%d condition=%+v", w.Code, backend.lastCond)
	}
}

func TestDeleteResolvesUnconditionalAndWildcardToCurrentETag(t *testing.T) {
	for _, header := range []string{"", "*"} {
		t.Run(map[string]string{"": "no condition", "*": "wildcard"}[header], func(t *testing.T) {
			backend := newFakeBackend()
			backend.addDir("vault")
			backend.addFile("vault/item.txt", []byte("contents"), `"current"`)
			h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
			r := authorizedRequest(http.MethodDelete, "/item.txt", nil)
			if header != "" {
				r.Header.Set("If-Match", header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNoContent || backend.lastDeleteCond.IfMatch != `"current"` || backend.exists("vault/item.txt") {
				t.Fatalf("DELETE status=%d condition=%+v objectExists=%v", w.Code, backend.lastDeleteCond, backend.exists("vault/item.txt"))
			}
		})
	}
}

func TestIncompletePutDoesNotReportSuccess(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	r := authorizedRequest(http.MethodPut, "/truncated.bin", strings.NewReader("short"))
	r.ContentLength = 10
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("incomplete PUT status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if backend.exists("vault/truncated.bin") {
		t.Fatal("incomplete body was committed")
	}
}

func TestOptionsAdvertisesOnlyImplementedMethods(t *testing.T) {
	backend := newFakeBackend()
	h := NewHandler(Config{Username: "web", Password: "secret"}, backend)
	r := authorizedRequest(http.MethodOptions, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("DAV") != "1" {
		t.Fatalf("OPTIONS status=%d DAV=%q", w.Code, w.Header().Get("DAV"))
	}
	allow := w.Header().Get("Allow")
	for _, method := range []string{"OPTIONS", "PROPFIND", "GET", "HEAD", "PUT", "MKCOL", "DELETE"} {
		if !strings.Contains(allow, method) {
			t.Errorf("Allow %q omits %s", allow, method)
		}
	}
	for _, method := range []string{"LOCK", "UNLOCK", "PROPPATCH", "COPY", "MOVE"} {
		if strings.Contains(allow, method) || w.Header().Get("DAV") == "1, 2" {
			t.Errorf("OPTIONS advertises unsupported method %s", method)
		}
	}
	r = authorizedRequest("LOCK", "/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("LOCK status = %d, want 501", w.Code)
	}
}

func TestDeleteRejectsDirectories(t *testing.T) {
	backend := newFakeBackend()
	backend.addDir("vault")
	backend.addDir("vault/dir")
	h := NewHandler(Config{Username: "web", Password: "secret", Prefix: "vault"}, backend)
	r := authorizedRequest(http.MethodDelete, "/dir", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE directory status = %d, want 405", w.Code)
	}
}

func TestStorageErrorsDoNotLeakDetails(t *testing.T) {
	backend := newFakeBackend()
	backend.putErr = errors.New("secret endpoint token")
	h := NewHandler(Config{Username: "web", Password: "secret"}, backend)
	r := authorizedRequest(http.MethodPut, "/doc.txt", strings.NewReader("data"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "secret endpoint token") {
		t.Fatalf("status/body = %d/%q", w.Code, w.Body.String())
	}
}

// Keep XML parsing isolated so the test names remain focused on HTTP behavior.
func xmlUnmarshal(data []byte, value any) error {
	return xml.Unmarshal(data, value)
}
