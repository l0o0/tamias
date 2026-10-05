package core

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tamiops/internal/storage"
)

func waitForAutomaticDetection(t *testing.T, s *Service, id string) Connection {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		pending := s.connectionDetectionPending[id]
		s.mu.Unlock()
		c, err := s.connection(id)
		if err == nil && !pending && connectionDetectionCurrent(c) {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("automatic connection detection did not complete")
	return Connection{}
}

func TestAutomaticConnectionDetectionUpgradesAndPersistsLegacyConnection(t *testing.T) {
	s, vault, old, dav := setupEditableConnection(t)
	s.StartScheduler()
	c := waitForAutomaticDetection(t, s, old.ID)
	if !c.Tested || !c.Capabilities.ConditionalWrite || c.CompatibilityProfile != "conditional" {
		t.Fatalf("legacy connection not detected: %+v", c)
	}
	s.StopScheduler()
	dav.mu.Lock()
	writes := dav.next
	remaining := len(dav.objects)
	dav.mu.Unlock()
	if writes == 0 || remaining != 0 {
		t.Fatalf("probe writes=%d residual=%d", writes, remaining)
	}
	for i := 0; i < 3; i++ {
		_ = s.Snapshot()
	}
	s.StartScheduler()
	s.StopScheduler()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(s.dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.connection(old.ID)
	if err != nil || persisted.Capabilities != c.Capabilities || !connectionDetectionCurrent(persisted) || !persisted.Tested {
		t.Fatalf("detection did not survive restart: %+v, %v", persisted, err)
	}
	reopened.StartScheduler()
	reopened.StopScheduler()
	dav.mu.Lock()
	defer dav.mu.Unlock()
	if dav.next != writes {
		t.Fatalf("saved detection was repeated: %d -> %d writes", writes, dav.next)
	}
}

func TestAutomaticConnectionDetectionKeepsCopyModeReadOnly(t *testing.T) {
	s, _, old, dav := setupEditableConnection(t)
	s.mu.Lock()
	s.cfg.Connections[0].WriteMode = storage.WriteModeCopy
	s.mu.Unlock()
	s.StartScheduler()
	c := waitForAutomaticDetection(t, s, old.ID)
	if !c.Tested || c.CompatibilityProfile != "read-checked" || c.Capabilities.ConditionalWrite || c.Capabilities.ConditionalDelete {
		t.Fatalf("copy mode was not read-only detected: %+v", c)
	}
	dav.mu.Lock()
	defer dav.mu.Unlock()
	if dav.next != 0 || len(dav.objects) != 0 {
		t.Fatal("copy-mode detection wrote a remote probe")
	}
}

func TestAutomaticConnectionDetectionCancelsOnClose(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	vault := &memoryVault{m: map[string]string{}}
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cfg := storage.Config{ID: ID(), Name: "slow legacy", Kind: "webdav", Endpoint: server.URL}
	raw, _ := json.Marshal(storage.Credentials{})
	if err := vault.Set("connection:"+cfg.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	s.cfg.Connections = append(s.cfg.Connections, Connection{Config: cfg})
	s.StartScheduler()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("detection did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel detection")
	}
}
