package core

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/gateway"
	"tamiops/internal/storage"
)

// This test exercises the real AWS SDK S3 adapter through Core and the WebDAV
// gateway. The local server implements only the S3 requests used by that path.
func TestS3WebDAVProtocolIntegration(t *testing.T) {
	fake := newS3ProtocolFake(t)
	fake.seed("scope/inside.txt", "existing object")
	fake.seed("scope/folder/nested.txt", "nested object")
	fake.seed("outside.txt", "must stay outside the configured prefix")
	s3Server := httptest.NewServer(fake)
	defer s3Server.Close()

	s, _ := testService(t)
	connectionID := ID()
	store, err := storage.New(storage.Config{
		ID: connectionID, Kind: "s3", Endpoint: s3Server.URL, Region: "us-east-1",
		Bucket: "test", Prefix: "scope", PathStyle: true,
	}, storage.Credentials{AccessKey: "test-access", SecretKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, Connection{
		Config:       storage.Config{ID: connectionID, Kind: "s3", Endpoint: s3Server.URL, Region: "us-east-1", Bucket: "test", Prefix: "scope", PathStyle: true},
		Tested:       true,
		Capabilities: storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true},
	})
	s.stores[connectionID] = store
	s.mu.Unlock()

	backend := Backend{Service: s, ConnectionID: connectionID}
	client := s3DAVClient(t, gateway.NewHandler(gateway.Config{Username: "alice", Password: "test-password", ReadOnly: true}, backend))

	// Read-only is enforced at the gateway edge; no S3 write reaches the fake.
	beforeWrites := fake.count(http.MethodPut)
	resp := client.request(t, http.MethodPut, "/readonly.txt", "blocked", map[string]string{"If-None-Match": "*"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only PUT status = %d, want 403", resp.StatusCode)
	}
	if got := fake.count(http.MethodPut); got != beforeWrites {
		t.Fatalf("read-only PUT reached S3: PUT count went from %d to %d", beforeWrites, got)
	}
	resp = client.request(t, http.MethodGet, "/inside.txt", "", nil)
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "existing object" {
		t.Fatalf("read-only GET = status %d body %q, want existing object", resp.StatusCode, body)
	}
	beforeDeletes := fake.count(http.MethodDelete)
	resp = client.request(t, http.MethodDelete, "/inside.txt", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only DELETE status = %d, want 403", resp.StatusCode)
	}
	if got := fake.count(http.MethodDelete); got != beforeDeletes {
		t.Fatalf("read-only DELETE reached S3: DELETE count went from %d to %d", beforeDeletes, got)
	}

	client = s3DAVClient(t, gateway.NewHandler(gateway.Config{Username: "alice", Password: "test-password"}, backend))

	// Depth-1 listing must come from S3's XML listing and stay within scope.
	resp = client.request(t, "PROPFIND", "/", "", map[string]string{"Depth": "1"})
	listing, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != 207 {
		t.Fatalf("PROPFIND status = %d, want 207; body: %s", resp.StatusCode, listing)
	}
	for _, want := range []string{"/inside.txt", "/folder/"} {
		if !strings.Contains(string(listing), want) {
			t.Errorf("PROPFIND listing omitted %q: %s", want, listing)
		}
	}
	if strings.Contains(string(listing), "outside.txt") || strings.Contains(string(listing), "nested.txt") {
		t.Errorf("PROPFIND leaked an outside-scope or non-direct object: %s", listing)
	}

	// The create condition reaches S3, yields an ETag, and the next GET reads
	// through the SDK's conditional GetObject path.
	resp = client.request(t, http.MethodPut, "/created.txt", "first payload", map[string]string{"If-None-Match": "*"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("conditional create status = %d, want 201", resp.StatusCode)
	}
	if got := fake.lastHeader(http.MethodPut, "/test/scope/created.txt", "If-None-Match"); got != "*" {
		t.Fatalf("S3 conditional create header = %q, want *", got)
	}

	resp = client.request(t, http.MethodGet, "/created.txt", "", nil)
	body, readErr = io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "first payload" {
		t.Fatalf("GET after create = status %d body %q, want 200 and original payload", resp.StatusCode, body)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("GET did not expose the S3 ETag")
	}

	// A second conditional create and a stale If-Match must both preserve data.
	resp = client.request(t, http.MethodPut, "/created.txt", "overwrite attempt", map[string]string{"If-None-Match": "*"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("duplicate conditional create status = %d, want 412", resp.StatusCode)
	}
	resp = client.request(t, http.MethodPut, "/created.txt", "stale overwrite", map[string]string{"If-Match": `"stale-tag"`})
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("stale conditional PUT status = %d, want 412", resp.StatusCode)
	}
	resp = client.request(t, http.MethodGet, "/created.txt", "", nil)
	body, readErr = io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "first payload" {
		t.Fatalf("failed condition changed the object: GET status %d body %q", resp.StatusCode, body)
	}

	// Gateway/core failures must become an HTTP error, never a success status.
	fake.failPutFor("scope/backend-fails.txt")
	resp = client.request(t, http.MethodPut, "/backend-fails.txt", "payload", map[string]string{"If-None-Match": "*"})
	failureBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Fatalf("S3 backend failure returned success status %d: %s", resp.StatusCode, failureBody)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("S3 backend failure status = %d, want 502: %s", resp.StatusCode, failureBody)
	}

	// If-None-Match on DELETE is a failed condition and must not reach S3.
	beforeDeletes = fake.count(http.MethodDelete)
	resp = client.request(t, http.MethodDelete, "/inside.txt", "", map[string]string{"If-None-Match": "*"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("DELETE If-None-Match status = %d, want 412", resp.StatusCode)
	}
	if got := fake.count(http.MethodDelete); got != beforeDeletes {
		t.Fatalf("failed DELETE condition reached S3: DELETE count went from %d to %d", beforeDeletes, got)
	}
	resp = client.request(t, http.MethodGet, "/inside.txt", "", nil)
	body, readErr = io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "existing object" {
		t.Fatalf("DELETE If-None-Match removed the object: GET status %d body %q", resp.StatusCode, body)
	}

	fake.assertAllSignedScopedRequests(t)
}

type s3DAVTestClient struct {
	http *http.Client
	base string
}

func s3DAVClient(t *testing.T, handler http.Handler) s3DAVTestClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return s3DAVTestClient{http: server.Client(), base: server.URL}
}

func (c s3DAVTestClient) request(t *testing.T, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("alice", "test-password")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type s3FakeObject struct {
	body    []byte
	etag    string
	changed time.Time
}

type s3FakeRequest struct {
	method  string
	path    string
	headers http.Header
}

type s3ProtocolFake struct {
	t        *testing.T
	mu       sync.Mutex
	objects  map[string]s3FakeObject
	requests []s3FakeRequest
	nextETag int
	failKey  string
}

func newS3ProtocolFake(t *testing.T) *s3ProtocolFake {
	return &s3ProtocolFake{t: t, objects: make(map[string]s3FakeObject)}
}

func (f *s3ProtocolFake) seed(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextETag++
	f.objects[key] = s3FakeObject{body: []byte(body), etag: fmt.Sprintf(`"seed-%d"`, f.nextETag), changed: time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)}
}

func (f *s3ProtocolFake) failPutFor(key string) {
	f.mu.Lock()
	f.failKey = key
	f.mu.Unlock()
}

func (f *s3ProtocolFake) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.method == method {
			n++
		}
	}
	return n
}

func (f *s3ProtocolFake) lastHeader(method, path, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		req := f.requests[i]
		if req.method == method && req.path == path {
			return req.headers.Get(name)
		}
	}
	return ""
}

func (f *s3ProtocolFake) assertAllSignedScopedRequests(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("AWS SDK made no requests to the S3 fake")
	}
	for _, req := range f.requests {
		auth := req.headers.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "Credential=test-access/") || !strings.Contains(auth, "/us-east-1/s3/aws4_request") {
			t.Errorf("%s %s did not carry expected SigV4 Authorization: %q", req.method, req.path, auth)
		}
		if req.path != "/test" && !strings.HasPrefix(req.path, "/test/") {
			t.Errorf("S3 request was not path-style addressed to bucket test: %s", req.path)
		}
		if strings.HasPrefix(req.path, "/test/") && !strings.HasPrefix(req.path, "/test/scope/") {
			t.Errorf("S3 request escaped configured scope: %s", req.path)
		}
	}
}

func (f *s3ProtocolFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	copyHeader := r.Header.Clone()
	f.requests = append(f.requests, s3FakeRequest{method: r.Method, path: r.URL.Path, headers: copyHeader})
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || !strings.Contains(r.Header.Get("Authorization"), "Credential=test-access/") || !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/s3/aws4_request") {
		f.mu.Unlock()
		f.t.Errorf("request missing expected SigV4 Authorization: %s %s: %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		writeS3FakeError(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	if r.URL.Path != "/test" && !strings.HasPrefix(r.URL.Path, "/test/") {
		f.mu.Unlock()
		f.t.Errorf("request did not use path-style bucket addressing: %s", r.URL.Path)
		writeS3FakeError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/test/") && !strings.HasPrefix(r.URL.Path, "/test/scope/") {
		f.mu.Unlock()
		f.t.Errorf("request escaped S3 prefix scope: %s", r.URL.Path)
		writeS3FakeError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	if r.URL.Path != "/test" && !strings.HasPrefix(r.URL.Path, "/test/") {
		f.mu.Unlock()
		writeS3FakeError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	if r.URL.Path == "/test" {
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			f.writeListLocked(w, r)
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
		writeS3FakeError(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/test/")
	switch r.Method {
	case http.MethodHead:
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			writeS3FakeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		setS3FakeObjectHeaders(w, obj)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			writeS3FakeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && match != "*" && match != obj.etag {
			writeS3FakeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		setS3FakeObjectHeaders(w, obj)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.body)
	case http.MethodPut:
		if key == f.failKey {
			f.mu.Unlock()
			writeS3FakeError(w, http.StatusServiceUnavailable, "InternalError")
			return
		}
		obj, exists := f.objects[key]
		if noneMatch := r.Header.Get("If-None-Match"); noneMatch == "*" && exists {
			f.mu.Unlock()
			writeS3FakeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && match != "*" && (!exists || match != obj.etag) {
			f.mu.Unlock()
			writeS3FakeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.mu.Unlock()
			writeS3FakeError(w, http.StatusInternalServerError, "InternalError")
			return
		}
		f.nextETag++
		obj = s3FakeObject{body: body, etag: fmt.Sprintf(`"object-%d"`, f.nextETag), changed: time.Now().UTC().Truncate(time.Second)}
		f.objects[key] = obj
		f.mu.Unlock()
		w.Header().Set("ETag", obj.etag)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		obj, exists := f.objects[key]
		if noneMatch := r.Header.Get("If-None-Match"); noneMatch == "*" && exists {
			f.mu.Unlock()
			writeS3FakeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && match != "*" && (!exists || match != obj.etag) {
			f.mu.Unlock()
			writeS3FakeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		f.mu.Unlock()
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		writeS3FakeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *s3ProtocolFake) writeListLocked(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	if prefix != "scope/" && !strings.HasPrefix(prefix, "scope/") {
		f.t.Errorf("ListObjectsV2 prefix = %q, want configured scope prefix", prefix)
	}
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	response := s3FakeListResult{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "test", Prefix: prefix, Delimiter: delimiter, MaxKeys: 1000, IsTruncated: false}
	common := make(map[string]bool)
	for _, key := range keys {
		relative := strings.TrimPrefix(key, prefix)
		if delimiter != "" {
			if i := strings.Index(relative, delimiter); i >= 0 {
				common[prefix+relative[:i+len(delimiter)]] = true
				continue
			}
		}
		obj := f.objects[key]
		response.Contents = append(response.Contents, s3FakeListObject{
			Key: key, LastModified: obj.changed.UTC().Format(time.RFC3339), ETag: obj.etag, Size: int64(len(obj.body)), StorageClass: "STANDARD",
		})
	}
	for value := range common {
		response.CommonPrefixes = append(response.CommonPrefixes, s3FakeCommonPrefix{Prefix: value})
	}
	sort.Slice(response.CommonPrefixes, func(i, j int) bool { return response.CommonPrefixes[i].Prefix < response.CommonPrefixes[j].Prefix })
	response.KeyCount = len(response.Contents) + len(response.CommonPrefixes)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if err := xml.NewEncoder(w).Encode(response); err != nil {
		f.t.Errorf("encode ListObjectsV2 response: %v", err)
	}
}

type s3FakeListResult struct {
	XMLName        xml.Name             `xml:"ListBucketResult"`
	XMLNS          string               `xml:"xmlns,attr"`
	Name           string               `xml:"Name"`
	Prefix         string               `xml:"Prefix"`
	Delimiter      string               `xml:"Delimiter,omitempty"`
	KeyCount       int                  `xml:"KeyCount"`
	MaxKeys        int                  `xml:"MaxKeys"`
	IsTruncated    bool                 `xml:"IsTruncated"`
	Contents       []s3FakeListObject   `xml:"Contents,omitempty"`
	CommonPrefixes []s3FakeCommonPrefix `xml:"CommonPrefixes,omitempty"`
}

type s3FakeListObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type s3FakeCommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

func setS3FakeObjectHeaders(w http.ResponseWriter, obj s3FakeObject) {
	w.Header().Set("ETag", obj.etag)
	w.Header().Set("Last-Modified", obj.changed.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(obj.body)))
	w.Header().Set("Content-Type", "application/octet-stream")
}

func writeS3FakeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: code})
}
