package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeWriteModeUsesStandardDefaultAndKeepsLegacyModes(t *testing.T) {
	mode, err := NormalizeWriteMode("")
	if err != nil || mode != WriteModeStandard {
		t.Fatalf("empty mode normalized to %q, err=%v; want %q", mode, err, WriteModeStandard)
	}
	for _, legacy := range []string{WriteModeStrict, WriteModeCopy, WriteModeCompatible} {
		got, err := NormalizeWriteMode(legacy)
		if err != nil || got != legacy {
			t.Fatalf("explicit mode %q changed to %q, err=%v", legacy, got, err)
		}
	}
	if got, err := NormalizeWriteMode("unknown"); err == nil || got != "" {
		t.Fatalf("unknown mode was accepted as %q", got)
	}
}

func TestConfigRecoveryFlagDefaultsOffAndRoundTripsWhenEnabled(t *testing.T) {
	cfg := Config{Kind: "webdav", Endpoint: "https://example.test/dav"}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	mode, err := NormalizeWriteMode(decoded.WriteMode)
	if err != nil || mode != WriteModeStandard || decoded.KeepRecovery {
		t.Fatalf("zero-value config did not use normal-sync defaults: mode=%q keepRecovery=%v err=%v", mode, decoded.KeepRecovery, err)
	}
	cfg.KeepRecovery = true
	encoded, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &decoded); err != nil || !decoded.KeepRecovery {
		t.Fatalf("enabled recovery flag did not persist: keepRecovery=%v err=%v", decoded.KeepRecovery, err)
	}
}

func TestWebDAVListScopesAndDecodesDirectChildren(t *testing.T) {
	const document = `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
<d:response><d:href>/dav/root/dir/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dav/root/dir/%E7%9B%AE%E5%BD%95%20One.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"opaque-tag"</d:getetag><d:getcontentlength>5</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dav/root/dir/sub/deeper.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"nested"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dav/root/dir2/leak.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"outside-dir"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dav/elsewhere/leak.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"outside-prefix"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`
	var authorized bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		authorized = ok && u == "alice" && p == "password"
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" || r.URL.Path != "/dav/root/dir/" {
			t.Errorf("unexpected WebDAV request: %s %s Depth=%q", r.Method, r.URL.Path, r.Header.Get("Depth"))
		}
		w.WriteHeader(207)
		_, _ = io.WriteString(w, document)
	}))
	defer server.Close()

	store, err := New(Config{Kind: "webdav", Endpoint: server.URL + "/dav", Prefix: "root"}, Credentials{Username: "alice", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "dir")
	if err != nil {
		t.Fatal(err)
	}
	if !authorized {
		t.Fatal("WebDAV request did not use the configured Basic credentials")
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %#v", len(entries), entries)
	}
	if entries[0].Path != "dir/sub" || !entries[0].IsDir {
		t.Fatalf("nested response was not reduced to a direct directory: %#v", entries[0])
	}
	if entries[1].Path != "dir/目录 One.txt" || entries[1].IsDir || entries[1].ETag != `"opaque-tag"` {
		t.Fatalf("Unicode file entry was not preserved: %#v", entries[1])
	}
}

func TestWebDAVRejectsPathEscapeBeforeRequest(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../secret", "a/../../b", "/absolute", "a\\b", "bad\x00key", "a//b", "a/./b"} {
		if _, err := store.List(context.Background(), key); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("List(%q) error = %v, want ErrInvalidPath", key, err)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid paths caused %d network requests", requests)
	}
}

func TestWebDAVConditionalOperationsAndOpenIfMatch(t *testing.T) {
	var mu sync.Mutex
	exists := false
	etag := ""
	next := 0
	var openIfMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/root/file.txt" {
			t.Errorf("unexpected key path %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read PUT body: %v", err)
			}
			if r.ContentLength != int64(len(data)) {
				t.Errorf("Content-Length=%d, body length=%d", r.ContentLength, len(data))
			}
			if r.Header.Get("If-None-Match") == "*" && exists || r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != `"v1"`) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			exists = true
			next++
			etag = fmt.Sprintf(`"v%d"`, next)
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			openIfMatch = r.Header.Get("If-Match")
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "payload")
		case http.MethodDelete:
			if r.Header.Get("If-Match") != etag {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			exists = false
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL, Prefix: "root"}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := store.Put(ctx, "file.txt", strings.NewReader("one"), 3, Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ETag != `"v1"` {
		t.Fatalf("initial ETag = %q", first.ETag)
	}
	if _, err := store.Put(ctx, "file.txt", strings.NewReader("two"), 3, Condition{IfNoneMatch: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate conditional create error = %v, want ErrConflict", err)
	}
	if _, err := store.Put(ctx, "file.txt", strings.NewReader("two"), 3, Condition{IfMatch: `"wrong"`}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong ETag write error = %v, want ErrConflict", err)
	}
	second, err := store.Put(ctx, "file.txt", strings.NewReader("two"), 3, Condition{IfMatch: first.ETag})
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.Open(ctx, "file.txt", second.ETag)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if openIfMatch != second.ETag {
		t.Fatalf("GET If-Match=%q, want %q", openIfMatch, second.ETag)
	}
	if err := store.Delete(ctx, "file.txt", Condition{IfMatch: `"wrong"`}); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong ETag delete error = %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, "file.txt", Condition{IfMatch: second.ETag}); err != nil {
		t.Fatal(err)
	}
}

func TestWebDAVPutWithoutResponseETagDoesNotStat(t *testing.T) {
	var propfinds int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			propfinds++
			w.WriteHeader(207)
			_, _ = io.WriteString(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/file</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"someone-elses-version"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Put(context.Background(), "file", strings.NewReader("payload"), int64(len("payload")), Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if entry.ETag != "" {
		t.Fatalf("Put returned a fabricated ETag: %q", entry.ETag)
	}
	if propfinds != 0 {
		t.Fatalf("Put made %d PROPFIND calls to infer a commit ETag", propfinds)
	}
}

func TestWebDAVOpenChecksResponseETagAndUnknownLength(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != `"expected"` {
			t.Errorf("If-Match = %q", r.Header.Get("If-Match"))
		}
		w.Header().Set("ETag", `"expected"`)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "payload")
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	body, entry, err := store.Open(context.Background(), "file", `"expected"`)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if entry.Size != -1 {
		t.Fatalf("unknown response size = %d, want -1", entry.Size)
	}

	serverMismatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"different"`)
		_, _ = io.WriteString(w, "wrong payload")
	}))
	defer serverMismatch.Close()
	storeMismatch, err := New(Config{Kind: "webdav", Endpoint: serverMismatch.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if body, _, err := storeMismatch.Open(context.Background(), "file", `"expected"`); !errors.Is(err, ErrConflict) || body != nil {
		t.Fatalf("Open returned body=%v error=%v; want conflict with no body", body, err)
	}

	serverMissing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	defer serverMissing.Close()
	storeMissing, err := New(Config{Kind: "webdav", Endpoint: serverMissing.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if body, _, err := storeMissing.Open(context.Background(), "file", `"expected"`); !errors.Is(err, ErrConflict) || body != nil {
		t.Fatalf("missing response ETag returned body=%v error=%v; want conflict with no body", body, err)
	}
}

func TestWebDAVPartialAndIncompleteMultistatusFailClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "malformed XML", body: `<d:multistatus xmlns:d="DAV:"><d:response>`},
		{name: "partial propstat failure", body: `<d:multistatus xmlns:d="DAV:">
<d:response><d:href>/dir/ok</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"ok"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dir/failed</d:href><d:propstat><d:prop><d:getetag/></d:prop><d:status>HTTP/1.1 500 Internal Server Error</d:status></d:propstat></d:response></d:multistatus>`},
		{name: "missing href", body: `<d:multistatus xmlns:d="DAV:"><d:response><d:propstat><d:prop/><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(207)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := store.List(context.Background(), "dir")
			if err == nil {
				t.Fatalf("List succeeded with partial response: %#v", entries)
			}
			if entries != nil {
				t.Fatalf("partial entries escaped after error: %#v", entries)
			}
		})
	}
}

func TestWebDAVDoesNotFollowCredentialedRedirect(t *testing.T) {
	var reached int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("Basic credentials reached redirect destination")
		}
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/target", http.StatusFound)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(context.Background(), "file", ""); err == nil {
		t.Fatal("Open followed a redirect")
	}
	if reached != 0 {
		t.Fatalf("redirect destination received %d requests", reached)
	}
}

func TestWebDAVAllowsMissingOptionalProperties(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		_, _ = io.WriteString(w, `<d:multistatus xmlns:d="DAV:">
<d:response><d:href>/dir/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dir/file</d:href>
<d:propstat><d:prop><d:resourcetype/><d:getetag>"tag"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
<d:propstat><d:prop><d:getcontentlength/><d:getlastmodified/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>
</d:response></d:multistatus>`)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ETag != `"tag"` {
		t.Fatalf("unexpected entry from optional-property response: %#v", entries)
	}
}

func TestWebDAVCollectionMayOmitETag(t *testing.T) {
	// This is the 207 shape returned by golang.org/x/net/webdav for a local
	// directory: resourcetype/last-modified succeed, while ETag and length are
	// reported in a separate 404 propstat. The order of propstats is immaterial.
	const successfulFirst = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:resourcetype><D:collection xmlns:D="DAV:"/></D:resourcetype><D:getlastmodified>Fri, 02 Oct 2026 23:24:06 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat><D:propstat><D:prop><D:getetag></D:getetag><D:getcontentlength></D:getcontentlength></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response></D:multistatus>`
	const missingFirst = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:getetag></D:getetag><D:getcontentlength></D:getcontentlength></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat><D:propstat><D:prop><D:resourcetype><D:collection xmlns:D="DAV:"/></D:resourcetype><D:getlastmodified>Fri, 02 Oct 2026 23:24:06 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	for _, body := range []string{successfulFirst, missingFirst} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PROPFIND" || r.Header.Get("Depth") != "0" {
				t.Errorf("unexpected WebDAV request: %s %s Depth=%q", r.Method, r.URL.Path, r.Header.Get("Depth"))
			}
			w.WriteHeader(207)
			_, _ = io.WriteString(w, body)
		}))
		store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		entry, err := store.Stat(context.Background(), "")
		server.Close()
		if err != nil {
			t.Fatalf("Stat rejected a collection without optional ETag: %v", err)
		}
		if !entry.IsDir || entry.ETag != "" {
			t.Fatalf("collection metadata = %#v, want directory with no ETag", entry)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Depth") != "1" {
			t.Errorf("unexpected WebDAV request: %s %s Depth=%q", r.Method, r.URL.Path, r.Header.Get("Depth"))
		}
		w.WriteHeader(207)
		_, _ = io.WriteString(w, `<D:multistatus xmlns:D="DAV:">
<D:response><D:href>/dir/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat><D:propstat><D:prop><D:getetag/></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>
<D:response><D:href>/dir/sub/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat><D:propstat><D:prop><D:getetag/></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>
</D:multistatus>`)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "dir")
	if err != nil {
		t.Fatalf("List rejected collections without ETags: %v", err)
	}
	if len(entries) != 1 || !entries[0].IsDir || entries[0].ETag != "" {
		t.Fatalf("listing entries = %#v, want one directory with no ETag", entries)
	}
}

func TestWebDAVListRequiresCompleteCollectionAndChildren(t *testing.T) {
	root := `<d:response><d:href>/dir/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	file := `<d:response><d:href>/dir/file</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"tag"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	cases := []struct {
		name string
		body string
	}{
		{name: "empty response omits collection", body: `<d:multistatus xmlns:d="DAV:"></d:multistatus>`},
		{name: "root collection omitted", body: `<d:multistatus xmlns:d="DAV:">` + file + `</d:multistatus>`},
		{name: "child forbidden", body: `<d:multistatus xmlns:d="DAV:">` + root + `<d:response><d:href>/dir/hidden</d:href><d:status>HTTP/1.1 403 Forbidden</d:status></d:response></d:multistatus>`},
		{name: "child missing required property", body: `<d:multistatus xmlns:d="DAV:">` + root + `<d:response><d:href>/dir/hidden</d:href><d:propstat><d:prop><d:getetag>"tag"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`},
		{name: "file missing required etag", body: `<d:multistatus xmlns:d="DAV:">` + root + `<d:response><d:href>/dir/file</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat><d:propstat><d:prop><d:getetag/><d:getcontentlength/><d:getlastmodified/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response></d:multistatus>`},
		{name: "duplicate child response", body: `<d:multistatus xmlns:d="DAV:">` + root + file + file + `</d:multistatus>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(207)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := store.List(context.Background(), "dir")
			if err == nil || entries != nil {
				t.Fatalf("List returned entries=%#v err=%v; want a fail-closed error", entries, err)
			}
		})
	}
}

func TestMemoryProbeAndConditions(t *testing.T) {
	store := NewMemory()
	caps, err := Probe(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ConditionalWrite || !caps.ConditionalDelete || !caps.RangeRead || caps.MultipartConditional {
		t.Fatalf("capabilities = %#v", caps)
	}
	entries, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe object was not cleaned: %#v", entries)
	}
}

type constantETagStore struct{ Store }

const constantProbeETag = `"constant-etag"`

func (s constantETagStore) Stat(ctx context.Context, key string) (Entry, error) {
	e, err := s.Store.Stat(ctx, key)
	if err == nil {
		e.ETag = constantProbeETag
	}
	return e, err
}
func (s constantETagStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, Entry, error) {
	body, e, err := s.Store.Open(ctx, key, "")
	if err == nil {
		e.ETag = constantProbeETag
	}
	return body, e, err
}
func (s constantETagStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond Condition) (Entry, error) {
	if cond.IfMatch == constantProbeETag {
		current, err := s.Store.Stat(ctx, key)
		if err != nil {
			return Entry{}, err
		}
		cond.IfMatch = current.ETag
	}
	e, err := s.Store.Put(ctx, key, body, size, cond)
	if err == nil {
		e.ETag = constantProbeETag
	}
	return e, err
}
func (s constantETagStore) Delete(ctx context.Context, key string, cond Condition) error {
	if cond.IfMatch == constantProbeETag {
		current, err := s.Store.Stat(ctx, key)
		if err != nil {
			return err
		}
		cond.IfMatch = current.ETag
	}
	return s.Store.Delete(ctx, key, cond)
}

func TestProbeRejectsETagThatDoesNotChangeAfterWrite(t *testing.T) {
	base := NewMemory()
	caps, err := Probe(context.Background(), constantETagStore{Store: base})
	if err == nil || caps.ConditionalWrite {
		t.Fatalf("Probe capabilities=%#v err=%v; want unchanged ETag rejected", caps, err)
	}
	entries, listErr := base.List(context.Background(), "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left objects behind after rejection: %#v", entries)
	}
}

func TestProbeMultipartConditionalRequiresProtectedCompletion(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ignoreIfMatch bool
		ignoreIfNone  bool
		constantETag  bool
		want          bool
	}{
		{name: "both conditions enforced", want: true},
		{name: "If-Match ignored", ignoreIfMatch: true},
		{name: "If-None-Match ignored", ignoreIfNone: true},
		{name: "multipart overwrite does not advance ETag", constantETag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &probeMultipartStore{
				Store:         NewMemory(),
				uploads:       make(map[string]*probeMultipartUpload),
				ignoreIfMatch: tc.ignoreIfMatch,
				ignoreIfNone:  tc.ignoreIfNone,
				constantETag:  tc.constantETag,
			}
			caps, err := Probe(context.Background(), store)
			if err != nil {
				t.Fatal(err)
			}
			if caps.MultipartConditional != tc.want {
				t.Fatalf("MultipartConditional = %v, want %v (capabilities %#v)", caps.MultipartConditional, tc.want, caps)
			}
			entries, err := store.List(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("probe objects were not cleaned: %#v", entries)
			}
		})
	}
}

type probeMultipartUpload struct {
	key   string
	parts map[int32]MultipartPart
	data  map[int32][]byte
}

type probeMultipartStore struct {
	Store
	uploads       map[string]*probeMultipartUpload
	next          int
	ignoreIfMatch bool
	ignoreIfNone  bool
	constantETag  bool
}

func (s *probeMultipartStore) CreateMultipart(_ context.Context, key string) (string, error) {
	s.next++
	id := fmt.Sprintf("upload-%d", s.next)
	s.uploads[id] = &probeMultipartUpload{key: key, parts: make(map[int32]MultipartPart), data: make(map[int32][]byte)}
	return id, nil
}

func (s *probeMultipartStore) UploadPart(_ context.Context, key, uploadID string, number int32, body io.Reader, size int64) (MultipartPart, error) {
	upload := s.uploads[uploadID]
	if upload == nil || upload.key != key || number < 1 || size < 0 {
		return MultipartPart{}, ErrNotFound
	}
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil || int64(len(data)) != size {
		return MultipartPart{}, fmt.Errorf("invalid probe part: %w", err)
	}
	part := MultipartPart{Number: number, ETag: fmt.Sprintf("part-%d", number), Size: size}
	upload.parts[number] = part
	upload.data[number] = data
	return part, nil
}

func (s *probeMultipartStore) ListParts(_ context.Context, key, uploadID string) ([]MultipartPart, error) {
	upload := s.uploads[uploadID]
	if upload == nil || upload.key != key {
		return nil, ErrNotFound
	}
	parts := make([]MultipartPart, 0, len(upload.parts))
	for _, part := range upload.parts {
		parts = append(parts, part)
	}
	return parts, nil
}

func (s *probeMultipartStore) CompleteMultipart(ctx context.Context, key, uploadID string, parts []MultipartPart, condition Condition) (Entry, error) {
	upload := s.uploads[uploadID]
	if upload == nil || upload.key != key {
		return Entry{}, ErrNotFound
	}
	current, statErr := s.Store.Stat(ctx, key)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, ErrNotFound) {
		return Entry{}, statErr
	}
	if condition.IfMatch != "" && !s.ignoreIfMatch && (!exists || current.ETag != condition.IfMatch) {
		return Entry{}, ErrConflict
	}
	if condition.IfNoneMatch && !s.ignoreIfNone && exists {
		return Entry{}, ErrConflict
	}
	var body bytes.Buffer
	for _, part := range parts {
		stored, ok := upload.parts[part.Number]
		if !ok || stored != part {
			return Entry{}, ErrConflict
		}
		body.Write(upload.data[part.Number])
	}
	entry, err := s.Store.Put(ctx, key, bytes.NewReader(body.Bytes()), int64(body.Len()), Condition{})
	if err == nil {
		if s.constantETag && exists {
			if memory, ok := s.Store.(*memoryStore); ok {
				memory.mu.Lock()
				object := memory.objects[key]
				object.etag = current.ETag
				memory.objects[key] = object
				memory.mu.Unlock()
				entry.ETag = current.ETag
			}
		}
		delete(s.uploads, uploadID)
	}
	return entry, err
}

func (s *probeMultipartStore) AbortMultipart(_ context.Context, key, uploadID string) error {
	if upload := s.uploads[uploadID]; upload == nil || upload.key != key {
		return ErrNotFound
	}
	delete(s.uploads, uploadID)
	return nil
}

func TestProbeRejectsSameETagWithChangedContent(t *testing.T) {
	for _, stage := range []string{"if-none-match", "wrong-if-match-put", "wrong-if-match-delete"} {
		t.Run(stage, func(t *testing.T) {
			base := NewMemory().(*memoryStore)
			store := &sameETagCorruptStore{Store: base, base: base, stage: stage}
			_, err := Probe(context.Background(), store)
			if err == nil || !strings.Contains(err.Error(), "content") {
				t.Fatalf("Probe error = %v, want content verification failure", err)
			}
			entries, listErr := base.List(context.Background(), "")
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(entries) != 1 {
				t.Fatalf("modified content must survive probe cleanup: %#v", entries)
			}
			body, _, openErr := base.Open(context.Background(), entries[0].Path, "")
			if openErr != nil {
				t.Fatal(openErr)
			}
			content, readErr := io.ReadAll(body)
			_ = body.Close()
			if readErr != nil || string(content) != "tampered bytes" {
				t.Fatalf("cleanup changed unknown content: %q, %v", content, readErr)
			}
		})
	}
}

type sameETagCorruptStore struct {
	Store
	base  *memoryStore
	stage string
}

func (s *sameETagCorruptStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition Condition) (Entry, error) {
	s.base.mu.RLock()
	obj, exists := s.base.objects[key]
	s.base.mu.RUnlock()
	corrupt := s.stage == "if-none-match" && condition.IfNoneMatch && exists ||
		s.stage == "wrong-if-match-put" && condition.IfMatch != "" && exists && condition.IfMatch != obj.etag
	if corrupt {
		s.changeContentWithoutChangingETag(key, []byte("tampered bytes"))
		return Entry{}, ErrConflict
	}
	return s.Store.Put(ctx, key, body, size, condition)
}

func (s *sameETagCorruptStore) Delete(ctx context.Context, key string, condition Condition) error {
	s.base.mu.RLock()
	obj, exists := s.base.objects[key]
	s.base.mu.RUnlock()
	if s.stage == "wrong-if-match-delete" && condition.IfMatch != "" && exists && condition.IfMatch != obj.etag {
		s.changeContentWithoutChangingETag(key, []byte("tampered bytes"))
		return ErrConflict
	}
	return s.Store.Delete(ctx, key, condition)
}

func (s *sameETagCorruptStore) changeContentWithoutChangingETag(key string, content []byte) {
	s.base.mu.Lock()
	obj := s.base.objects[key]
	obj.data = append([]byte(nil), content...)
	s.base.objects[key] = obj
	s.base.mu.Unlock()
}

func TestProbeReportsCleanupFailureWithoutUnconditionalDelete(t *testing.T) {
	base := NewMemory()
	store := cleanupFailStore{Store: base}
	_, err := Probe(context.Background(), store)
	if err == nil || !strings.Contains(err.Error(), "清理失败") {
		t.Fatalf("Probe error = %v, want cleanup failure", err)
	}
	entries, err := base.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("cleanup failure should leave the conditionally protected probe object, got %#v", entries)
	}
	if !strings.HasPrefix(entries[0].Name, ".tamiops-probe-") {
		t.Fatalf("unexpected cleanup fixture name: %#v", entries[0])
	}
}

type cleanupFailStore struct{ Store }

func (s cleanupFailStore) Delete(ctx context.Context, key string, condition Condition) error {
	if condition.IfMatch != "" {
		return ErrUnsupported
	}
	return s.Store.Delete(ctx, key, condition)
}

func TestS3ListPaginatesAndKeepsConfiguredPrefix(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSuffix(r.URL.Path, "/") != "/bucket" {
			t.Errorf("unexpected path-style URL %q", r.URL.Path)
		}
		mu.Lock()
		seen = append(seen, r.URL.Query().Get("continuation-token"))
		mu.Unlock()
		if r.URL.Query().Get("prefix") != "base/" || r.URL.Query().Get("delimiter") != "/" {
			t.Errorf("unexpected list query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "page-two" {
			_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>base/</Prefix><IsTruncated>false</IsTruncated><Contents><Key>base/Ωmega.txt</Key><Size>3</Size><ETag>&quot;etag-3&quot;</ETag></Contents></ListBucketResult>`)
			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>base/</Prefix><Delimiter>/</Delimiter><IsTruncated>true</IsTruncated><NextContinuationToken>page-two</NextContinuationToken><Contents><Key>base/Alpha.txt</Key><Size>1</Size><ETag>&quot;etag-1&quot;</ETag></Contents><CommonPrefixes><Prefix>base/Sub/</Prefix></CommonPrefixes></ListBucketResult>`)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", Prefix: "base", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Name != "Alpha.txt" || entries[1].Name != "Sub" || !entries[1].IsDir || entries[2].Name != "Ωmega.txt" {
		t.Fatalf("unexpected paginated listing: %#v", entries)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "" || seen[1] != "page-two" {
		t.Fatalf("continuation requests = %#v", seen)
	}
}

func TestS3ListRejectsIncompleteContinuation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>true</IsTruncated></ListBucketResult>`)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "no continuation token") {
		t.Fatalf("List error = %v, want incomplete pagination error", err)
	}
}

func TestS3ListRejectsMissingTruncationFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Contents><Key>file.txt</Key><Size>1</Size><ETag>&quot;etag&quot;</ETag></Contents></ListBucketResult>`)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "")
	if err == nil || entries != nil || !strings.Contains(err.Error(), "omitted IsTruncated") {
		t.Fatalf("List returned entries=%#v err=%v; want missing truncation flag error", entries, err)
	}
}

func TestS3ListRejectsFileAndDirectoryNameCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated><Contents><Key>foo</Key><Size>1</Size><ETag>&quot;file&quot;</ETag></Contents><CommonPrefixes><Prefix>foo/</Prefix></CommonPrefixes></ListBucketResult>`)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.List(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "both a file and a directory") {
		t.Fatalf("List error = %v, entries=%#v; want explicit file/directory collision", err, entries)
	}
	if entries != nil {
		t.Fatalf("partial collision listing escaped: %#v", entries)
	}
}

func TestS3ProbeUsesConditionsAndCleansTemporaryObject(t *testing.T) {
	var mu sync.Mutex
	objects := make(map[string][]byte)
	etags := make(map[string]string)
	next := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read S3 request body: %v", err)
			}
			oldETag, exists := etags[key]
			if r.Header.Get("If-None-Match") == "*" && exists || r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != oldETag) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code><Message>conditional failure</Message></Error>`)
				return
			}
			objects[key] = body
			next++
			etag := fmt.Sprintf(`"s3-%d"`, next)
			etags[key] = etag
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			if _, exists := etags[key]; !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", etags[key])
			w.Header().Set("Content-Length", fmt.Sprint(len(objects[key])))
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if _, exists := etags[key]; !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Header.Get("If-Match") != etags[key] {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				return
			}
			w.Header().Set("ETag", etags[key])
			w.Header().Set("Content-Length", fmt.Sprint(len(objects[key])))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(objects[key])
		case http.MethodDelete:
			if len(r.Header.Get("If-Match")) > 0 && r.Header.Get("If-Match") != etags[key] {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				return
			}
			delete(objects, key)
			delete(etags, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", Prefix: "scope", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	caps, err := Probe(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ConditionalWrite || !caps.ConditionalDelete {
		t.Fatalf("capabilities = %#v", caps)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(objects) != 0 {
		t.Fatalf("probe left objects behind: %#v", objects)
	}
}

func TestS3PutWithoutResponseETagDoesNotHead(t *testing.T) {
	var heads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			heads++
			w.Header().Set("ETag", `"other-version"`)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Put(context.Background(), "file", strings.NewReader("payload"), int64(len("payload")), Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if entry.ETag != "" {
		t.Fatalf("Put returned a fabricated ETag: %q", entry.ETag)
	}
	if heads != 0 {
		t.Fatalf("Put made %d HEAD calls to infer a commit ETag", heads)
	}
}

func TestS3OpenChecksResponseETag(t *testing.T) {
	var heads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("ETag", `"different"`)
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data")
		case http.MethodHead:
			heads++
			w.Header().Set("ETag", `"expected"`)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := store.Open(context.Background(), "file", `"expected"`)
	if !errors.Is(err, ErrConflict) || body != nil {
		t.Fatalf("Open returned body=%v error=%v; want conflict with no body", body, err)
	}
	if heads != 0 {
		t.Fatalf("Open made %d HEAD calls to infer a response ETag", heads)
	}

	serverMissing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data")
	}))
	defer serverMissing.Close()
	storeMissing, err := New(Config{Kind: "s3", Endpoint: serverMissing.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if body, _, err := storeMissing.Open(context.Background(), "file", `"expected"`); !errors.Is(err, ErrConflict) || body != nil {
		t.Fatalf("missing response ETag returned body=%v error=%v; want conflict with no body", body, err)
	}
}

func TestS3ErrorsDoNotExposeRemoteResponseText(t *testing.T) {
	err := mapS3Error("put", errors.New("server echoed secret-access-key"))
	if strings.Contains(err.Error(), "secret-access-key") {
		t.Fatalf("remote error text leaked: %v", err)
	}
}

func TestS3DoesNotFollowCredentialedRedirect(t *testing.T) {
	var reached int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	store, err := New(Config{Kind: "s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "bucket", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Open(context.Background(), "file", ""); err == nil {
		t.Fatal("S3 Open followed a redirect")
	}
	if reached != 0 {
		t.Fatalf("redirect destination received %d requests", reached)
	}
}
