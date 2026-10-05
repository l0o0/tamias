package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebDAVRangeRequiresStableRevisionAndValidPartialResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Range") != "bytes=2-4" || r.Header.Get("If-Match") != `"stable"` || r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("unexpected range request: method=%s Range=%q If-Match=%q Accept-Encoding=%q", r.Method, r.Header.Get("Range"), r.Header.Get("If-Match"), r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("ETag", `"stable"`)
		w.Header().Set("Content-Range", "bytes 2-4/8")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "cde")
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	ranges := store.(RangeStore)
	body, entry, err := ranges.OpenRange(context.Background(), "file.txt", `"stable"`, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "cde" || entry.Size != 3 || entry.ETag != `"stable"` {
		t.Fatalf("entry=%+v data=%q err=%v", entry, data, err)
	}
	if _, _, err := ranges.OpenRange(context.Background(), "file.txt", `W/"weak"`, 0, 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("weak ETag range error=%v, want ErrUnsupported", err)
	}
}

func TestWebDAVRangeRejectsIgnoredAndMismatchedResponses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		etag         string
		contentRange string
		want         error
	}{
		{name: "range ignored", status: http.StatusOK, etag: `"e"`, want: ErrUnsupported},
		{name: "wrong etag", status: http.StatusPartialContent, etag: `"other"`, contentRange: "bytes 0-1/4", want: ErrConflict},
		{name: "wrong interval", status: http.StatusPartialContent, etag: `"e"`, contentRange: "bytes 1-2/4", want: ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", tc.etag)
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				if tc.status == http.StatusPartialContent {
					w.Header().Set("Content-Length", "2")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "xx")
			}))
			defer server.Close()
			store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
			if err != nil {
				t.Fatal(err)
			}
			body, _, err := store.(RangeStore).OpenRange(context.Background(), "file.txt", `"e"`, 0, 2)
			if !errors.Is(err, tc.want) || body != nil {
				t.Fatalf("body=%v err=%v; want %v", body, err, tc.want)
			}
		})
	}
}

func TestWebDAVRangeUnsatisfiableIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes */4")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		_, _ = io.Copy(io.Discard, strings.NewReader(""))
	}))
	defer server.Close()
	store, err := New(Config{Kind: "webdav", Endpoint: server.URL}, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if body, _, err := store.(RangeStore).OpenRange(context.Background(), "file.txt", `"e"`, 8, 1); !errors.Is(err, ErrInvalidRange) || body != nil {
		t.Fatalf("body=%v err=%v; want ErrInvalidRange", body, err)
	}
}
