package core

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type responseWriteGate struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newResponseWriteGate() *responseWriteGate {
	return &responseWriteGate{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *responseWriteGate) Header() http.Header { return w.header }
func (w *responseWriteGate) WriteHeader(int)     {}
func (w *responseWriteGate) Write(body []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(body), nil
}

func TestPostHandlerFinishesTrackedTaskBeforeResponseWrite(t *testing.T) {
	s, _ := testService(t)
	var picked bool
	s.PickSave = func(string) (string, error) {
		picked = true
		return filepath.Join(t.TempDir(), "config.json"), nil
	}
	r := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader("ignored"))
	r.Header.Set("X-Tami-Client", "desktop")
	w := newResponseWriteGate()
	handlerDone := make(chan struct{})
	go func() {
		s.Handler(nil).ServeHTTP(w, r)
		close(handlerDone)
	}()

	select {
	case <-w.entered:
	case <-time.After(time.Second):
		close(w.release)
		t.Fatal("handler did not reach its response write")
	}
	if !picked {
		close(w.release)
		t.Fatal("request did not run the save command")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	var closeErr error
	closeWasBlocked := false
	select {
	case closeErr = <-closeDone:
	case <-time.After(time.Second):
		closeWasBlocked = true
	}
	close(w.release)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler did not finish after releasing the response writer")
	}
	if closeWasBlocked {
		select {
		case closeErr = <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("service close remained blocked after response write completed")
		}
		t.Fatal("service close waited for an HTTP response write after Core work had finished")
	}
	if closeErr != nil {
		t.Fatalf("service close failed: %v", closeErr)
	}
}
