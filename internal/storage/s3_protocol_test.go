package storage

import (
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
)

func newProtocolS3(t *testing.T, endpoint string) Store {
	t.Helper()
	store, err := New(Config{Kind: "s3", Endpoint: endpoint, Region: "us-east-1", Bucket: "bucket", Prefix: "root", PathStyle: true}, Credentials{AccessKey: "access", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3RangeAndObjectVersionRequests(t *testing.T) {
	var mu sync.Mutex
	var rangeSeen, versionSeen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.Method == http.MethodGet && query.Has("versions") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>root/file.txt</Prefix><IsTruncated>false</IsTruncated><Version><Key>root/file.txt</Key><VersionId>v2</VersionId><IsLatest>true</IsLatest><LastModified>2026-10-02T00:00:00.000Z</LastModified><ETag>"etag-2"</ETag><Size>7</Size></Version><DeleteMarker><Key>root/file.txt</Key><VersionId>gone</VersionId><IsLatest>false</IsLatest><LastModified>2026-10-01T00:00:00.000Z</LastModified></DeleteMarker><Version><Key>root/file.txt/child</Key><VersionId>child</VersionId><IsLatest>true</IsLatest><LastModified>2026-10-02T00:00:00.000Z</LastModified><ETag>"child"</ETag><Size>1</Size></Version></ListVersionsResult>`)
			return
		}
		if r.Method == http.MethodGet && query.Get("versionId") != "" {
			mu.Lock()
			versionSeen = query.Get("versionId")
			mu.Unlock()
			w.Header().Set("ETag", `"etag-1"`)
			w.Header().Set("Last-Modified", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
			w.Header().Set("x-amz-version-id", query.Get("versionId"))
			_, _ = io.WriteString(w, "version")
			return
		}
		if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
			mu.Lock()
			rangeSeen = r.Header.Get("Range") + "|" + r.Header.Get("If-Match")
			mu.Unlock()
			if r.Header.Get("If-Match") != `"etag-2"` {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			w.Header().Set("ETag", `"etag-2"`)
			w.Header().Set("Content-Range", "bytes 1-3/7")
			w.Header().Set("Content-Length", "3")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "abc")
			return
		}
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	store := newProtocolS3(t, server.URL)
	rangeStore, ok := store.(RangeStore)
	if !ok {
		t.Fatal("S3 store does not expose RangeStore")
	}
	body, entry, err := rangeStore.OpenRange(context.Background(), "file.txt", `"etag-2"`, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "abc" || entry.Size != 3 || entry.ETag != `"etag-2"` {
		t.Fatalf("range result entry=%+v data=%q err=%v", entry, data, err)
	}
	mu.Lock()
	gotRange := rangeSeen
	mu.Unlock()
	if gotRange != `bytes=1-3|"etag-2"` {
		t.Fatalf("S3 range request headers = %q", gotRange)
	}

	versioned, ok := store.(VersionedStore)
	if !ok {
		t.Fatal("S3 store does not expose VersionedStore")
	}
	versions, err := versioned.ListVersions(context.Background(), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].VersionID != "v2" || !versions[0].IsLatest || !versions[1].DeleteMarker {
		t.Fatalf("version list was not exact-key filtered: %+v", versions)
	}
	body, entry, err = versioned.OpenVersion(context.Background(), "file.txt", "v1")
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "version" || entry.VersionID != "v1" {
		t.Fatalf("version read entry=%+v data=%q err=%v", entry, data, err)
	}
	mu.Lock()
	gotVersion := versionSeen
	mu.Unlock()
	if gotVersion != "v1" {
		t.Fatalf("versionId query = %q", gotVersion)
	}
}

func TestS3MultipartIsResumableAndCompletesConditionally(t *testing.T) {
	var mu sync.Mutex
	var uploaded []byte
	var completeHeaders http.Header
	var abortCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		uploadID := query.Get("uploadId")
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>bucket</Bucket><Key>root/large.bin</Key><UploadId>upload-123</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && uploadID == "upload-123" && query.Get("partNumber") == "1":
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			uploaded = data
			mu.Unlock()
			w.Header().Set("ETag", `"part-etag"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && uploadID == "upload-123":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><ListPartsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>bucket</Bucket><Key>root/large.bin</Key><UploadId>upload-123</UploadId><IsTruncated>false</IsTruncated><Part><PartNumber>1</PartNumber><ETag>"part-etag"</ETag><Size>4</Size></Part></ListPartsResult>`)
		case r.Method == http.MethodPost && uploadID == "upload-123":
			mu.Lock()
			completeHeaders = r.Header.Clone()
			mu.Unlock()
			if r.Header.Get("If-Match") == `"wrong"` {
				http.Error(w, `<Error><Code>PreconditionFailed</Code></Error>`, http.StatusPreconditionFailed)
				return
			}
			if r.Header.Get("If-Match") != `"current"` || r.Header.Get("If-None-Match") != "" {
				http.Error(w, "completion was not conditional", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Location>http://example/bucket/large.bin</Location><Bucket>bucket</Bucket><Key>root/large.bin</Key><ETag>"object-etag"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodDelete && uploadID == "upload-123":
			mu.Lock()
			abortCalled = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, fmt.Sprintf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery), http.StatusBadRequest)
		}
	}))
	defer server.Close()
	store := newProtocolS3(t, server.URL)
	multipart, ok := store.(MultipartStore)
	if !ok {
		t.Fatal("S3 store does not expose MultipartStore")
	}
	ctx := context.Background()
	uploadID, err := multipart.CreateMultipart(ctx, "large.bin")
	if err != nil || uploadID != "upload-123" {
		t.Fatalf("CreateMultipart id=%q err=%v", uploadID, err)
	}
	part, err := multipart.UploadPart(ctx, "large.bin", uploadID, 1, strings.NewReader("data"), 4)
	if err != nil || part.Number != 1 || part.ETag != `"part-etag"` || part.Size != 4 {
		t.Fatalf("UploadPart receipt=%+v err=%v", part, err)
	}
	mu.Lock()
	gotUpload := string(uploaded)
	mu.Unlock()
	if gotUpload != "data" {
		t.Fatalf("uploaded body = %q", gotUpload)
	}
	parts, err := multipart.ListParts(ctx, "large.bin", uploadID)
	if err != nil || len(parts) != 1 || parts[0] != part {
		t.Fatalf("ListParts=%+v err=%v", parts, err)
	}
	entry, err := multipart.CompleteMultipart(ctx, "large.bin", uploadID, parts, Condition{IfMatch: `"current"`})
	if err != nil || entry.Size != 4 || entry.ETag != `"object-etag"` {
		t.Fatalf("CompleteMultipart entry=%+v err=%v", entry, err)
	}
	mu.Lock()
	if completeHeaders.Get("If-Match") != `"current"` {
		t.Errorf("complete If-Match header = %q", completeHeaders.Get("If-Match"))
	}
	mu.Unlock()
	if err := multipart.AbortMultipart(ctx, "large.bin", uploadID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !abortCalled {
		t.Fatal("AbortMultipart did not reach S3")
	}
}

func TestS3MultipartConditionalCompletionFailureIsNotRetriedUnconditionally(t *testing.T) {
	var completeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>bucket</Bucket><Key>root/large.bin</Key><UploadId>upload-123</UploadId></InitiateMultipartUploadResult>`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Query().Get("uploadId") == "upload-123" {
			completeCalls++
			if r.Header.Get("If-Match") == `"wrong"` {
				http.Error(w, `<Error><Code>PreconditionFailed</Code></Error>`, http.StatusPreconditionFailed)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()
	store := newProtocolS3(t, server.URL)
	multipart := store.(MultipartStore)
	uploadID, err := multipart.CreateMultipart(context.Background(), "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = multipart.CompleteMultipart(context.Background(), "large.bin", uploadID, []MultipartPart{{Number: 1, ETag: `"part"`, Size: 1}}, Condition{IfMatch: `"wrong"`})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong completion condition error=%v, want ErrConflict", err)
	}
	if completeCalls != 1 {
		t.Fatalf("conditional completion attempted %d times, want exactly once", completeCalls)
	}
}

func TestS3VersionAndMultipartUnsupportedResponsesStayUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, `<Error><Code>NotImplemented</Code></Error>`)
	}))
	defer server.Close()
	store := newProtocolS3(t, server.URL)
	versioned := store.(VersionedStore)
	if _, err := versioned.ListVersions(context.Background(), "file.txt"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ListVersions error=%v, want ErrUnsupported", err)
	}
	multipart := store.(MultipartStore)
	if _, err := multipart.CreateMultipart(context.Background(), "large.bin"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("CreateMultipart error=%v, want ErrUnsupported", err)
	}
}

func TestS3EmptyDirectoryRemovalOnlyDeletesConditionalMarker(t *testing.T) {
	var mu sync.Mutex
	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bucket/root/dir/" {
			t.Errorf("unsafe directory object path: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "HEAD":
			w.Header().Set("ETag", `"marker"`)
			w.Header().Set("Content-Length", "0")
		case "DELETE":
			if r.Header.Get("If-Match") != `"marker"` {
				t.Error("missing marker precondition")
			}
			mu.Lock()
			seen = true
			mu.Unlock()
			w.WriteHeader(204)
		default:
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	st := newProtocolS3(t, server.URL)
	if err := st.(EmptyDirectoryStore).RemoveEmptyDirectory(context.Background(), "dir"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen {
		t.Fatal("marker was not deleted")
	}
}
