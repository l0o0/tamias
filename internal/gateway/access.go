package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

type accessRequestBody struct {
	io.ReadCloser
	bytes int64
}

func (b *accessRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	return n, err
}

type accessResponseWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	partial bool
}

func (w *accessResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func markAccessPartial(w http.ResponseWriter) {
	if response, ok := w.(*accessResponseWriter); ok {
		response.partial = true
	}
}

func (w *accessResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *accessResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *accessResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}
