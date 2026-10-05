package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestWebDAVOpenRequestsIdentityRepresentation(t *testing.T) {
	const etag = `"same-representation"`
	want := []byte("the uncompressed WebDAV representation")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("If-Match") != etag {
			t.Errorf("unexpected request: method=%s If-Match=%q", r.Method, r.Header.Get("If-Match"))
		}
		w.Header().Set("Vary", "Accept-Encoding")
		if r.Header.Get("Accept-Encoding") != "identity" {
			w.Header().Set("ETag", `W/"same-representation"`)
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			if _, err := zw.Write(want); err != nil {
				t.Errorf("write compressed response: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Errorf("close compressed response: %v", err)
			}
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(want)))
		_, _ = w.Write(want)
	}))
	defer server.Close()

	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	body, entry, err := store.Open(context.Background(), "file.txt", etag)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read err=%v close err=%v", readErr, closeErr)
	}
	if !bytes.Equal(data, want) || entry.ETag != etag || entry.Size != int64(len(want)) {
		t.Fatalf("entry=%+v data=%q, want ETag=%q size=%d data=%q", entry, data, etag, len(want), want)
	}
}
