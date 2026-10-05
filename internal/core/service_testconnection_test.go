package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type connectionRestrictionStore struct {
	storage.Store
	putErr  error
	listErr error
}

func (s *connectionRestrictionStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition storage.Condition) (storage.Entry, error) {
	if s.putErr != nil {
		return storage.Entry{}, s.putErr
	}
	return s.Store.Put(ctx, key, body, size, condition)
}

func (s *connectionRestrictionStore) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(ctx, prefix)
}

func TestTestConnectionTracksReadOnlyRestrictionSafely(t *testing.T) {
	s, id := testService(t)
	setPolicyForTest(t, s, id, storage.WriteModeStrict, storage.Capabilities{})
	const detail = "仅新建条件被忽略；临时验证文件 .tamiops-probe-fixture 清理失败"
	remote := &connectionRestrictionStore{Store: storage.NewMemory(), putErr: fmt.Errorf("%w：%s", storage.ErrConditionalUnsupported, detail)}
	s.mu.Lock()
	s.stores[id] = remote
	s.mu.Unlock()

	ctx := context.Background()
	caps, err := s.TestConnection(ctx, id, true)
	if !errors.Is(err, storage.ErrConditionalUnsupported) || !strings.Contains(err.Error(), "连接可读取") {
		t.Fatalf("conditional write failure = %v, want clear read-only restriction", err)
	}
	if !strings.Contains(err.Error(), detail) {
		t.Fatalf("probe cause and cleanup detail were lost: %v", err)
	}
	if caps.ConditionalWrite || caps.ConditionalDelete || caps.MultipartConditional {
		t.Fatalf("unsafe write capabilities reported: %+v", caps)
	}
	c, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Tested || c.Error != "" || c.WriteRestriction == "" || c.Capabilities.ConditionalWrite || c.Capabilities.ConditionalDelete {
		t.Fatalf("readable but restricted state was not recorded safely: %+v", c)
	}
	if c.CompatibilityProfile != connectionProfileCompatible || !connectionDetectionCurrent(c) {
		t.Fatalf("manual probe result did not persist a current compatible profile: %+v", c)
	}
	restriction := c.WriteRestriction
	if !strings.Contains(restriction, detail) {
		t.Fatalf("saved restriction omitted probe detail: %q", restriction)
	}

	if _, err = s.TestConnection(ctx, id, false); err != nil {
		t.Fatalf("read-only recheck failed: %v", err)
	}
	c, err = s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Tested || c.WriteRestriction != restriction || c.Capabilities.ConditionalWrite || c.Capabilities.ConditionalDelete {
		t.Fatalf("read-only check erased restriction or enabled writes: %+v", c)
	}
	if c.CompatibilityProfile != connectionProfileReadChecked || !connectionDetectionCurrent(c) {
		t.Fatalf("manual read check did not persist its measured profile: %+v", c)
	}

	remote.listErr = errors.New("private transport details")
	caps, err = s.TestConnection(ctx, id, true)
	if err == nil || !strings.Contains(err.Error(), "连接异常") || strings.Contains(err.Error(), "private transport details") {
		t.Fatalf("failed read-only fallback should report a safe connection error, got %v", err)
	}
	if caps.ConditionalWrite || caps.ConditionalDelete || caps.MultipartConditional {
		t.Fatalf("failed fallback left unsafe capabilities: %+v", caps)
	}
	c, err = s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tested || !strings.Contains(c.Error, "连接异常") || c.WriteRestriction != restriction || c.Capabilities != (storage.Capabilities{}) {
		t.Fatalf("failed list fallback was treated as readable or lost the known restriction: %+v", c)
	}
	if c.CompatibilityProfile != connectionProfileFailed || !connectionDetectionCurrent(c) {
		t.Fatalf("failed read check did not persist a versioned failure result: %+v", c)
	}

	remote.listErr = nil
	remote.putErr = nil
	caps, err = s.TestConnection(ctx, id, true)
	if err != nil {
		t.Fatalf("successful write verification failed: %v", err)
	}
	if !caps.ConditionalWrite || !caps.ConditionalDelete {
		t.Fatalf("successful probe did not restore verified capabilities: %+v", caps)
	}
	c, err = s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if c.WriteRestriction != "" || c.Error != "" || !c.Tested {
		t.Fatalf("successful write verification did not clear stale restriction: %+v", c)
	}
	if c.CompatibilityProfile != connectionProfileConditional || !connectionDetectionCurrent(c) {
		t.Fatalf("successful manual probe did not persist current conditional capabilities: %+v", c)
	}
}

func TestTestConnectionProbesThroughLocalGatewayWithoutHoldingWriter(t *testing.T) {
	s, demoID := testService(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	g, secret, err := s.AddGateway(Gateway{Name: "test-connection-loopback", ConnectionID: demoID, Port: port, Username: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartGateway(g.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.StopGateway(g.ID) })
	connection, err := s.AddConnection(context.Background(), ConnectionInput{
		Config:   storage.Config{Name: "loopback-webdav", Kind: "webdav", Endpoint: g.URL, Username: g.Username},
		Password: secret,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	type result struct {
		capabilities storage.Capabilities
		err          error
	}
	done := make(chan result, 1)
	go func() {
		capabilities, probeErr := s.TestConnection(ctx, connection.ID, true)
		done <- result{capabilities: capabilities, err: probeErr}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("connection probe through local gateway failed: %v", got.err)
		}
		if !got.capabilities.ConditionalWrite || !got.capabilities.ConditionalDelete {
			t.Fatalf("connection probe returned incomplete capabilities: %+v", got.capabilities)
		}
	case <-ctx.Done():
		t.Fatal("connection probe through local gateway waited on the shared writer")
	}
}

type blockedConnectionProbe struct {
	service  *Service
	vault    *memoryVault
	conn     Connection
	started  <-chan struct{}
	release  func()
	finished chan error
}

func startBlockedConnectionProbe(t *testing.T) blockedConnectionProbe {
	t.Helper()
	started := make(chan struct{}, 1)
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	var requestOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype><d:getcontentlength>0</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
			return
		}
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requestOnce.Do(func() { started <- struct{}{} })
		select {
		case <-releaseCh:
			http.Error(w, "injected probe failure", http.StatusInternalServerError)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	vault := &memoryVault{m: map[string]string{}}
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := storage.Config{ID: ID(), Name: "blocked probe", Kind: "webdav", Endpoint: server.URL + "/dav", Username: "alice"}
	credentials := storage.Credentials{Username: "alice", Password: "old-secret"}
	raw, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Set("connection:"+cfg.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	previousCapabilities := storage.Capabilities{RangeRead: true}
	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, Connection{Config: cfg, Tested: true, Capabilities: previousCapabilities, Error: "previous probe result"})
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	conn := Connection{Config: cfg, Tested: true, Capabilities: previousCapabilities, Error: "previous probe result"}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	finished := make(chan error, 1)
	go func() {
		_, probeErr := s.TestConnection(ctx, cfg.ID, true)
		finished <- probeErr
		cancel()
	}()
	t.Cleanup(cancel)
	return blockedConnectionProbe{
		service:  s,
		vault:    vault,
		conn:     conn,
		started:  started,
		release:  func() { releaseOnce.Do(func() { close(releaseCh) }) },
		finished: finished,
	}
}

func (p blockedConnectionProbe) waitForProbe(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(3 * time.Second):
		p.release()
		t.Fatal("connection probe did not reach the remote store")
	}
}

func (p blockedConnectionProbe) lockWriterDuringProbe(t *testing.T) {
	t.Helper()
	acquired := make(chan struct{})
	go func() {
		p.service.writes.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return // The test now owns the writer lock.
	case <-time.After(time.Second):
		p.release()
		select {
		case <-p.finished:
		case <-time.After(5 * time.Second):
			t.Fatal("connection probe did not finish after the blocked request was released")
		}
		select {
		case <-acquired:
			p.service.writes.Unlock()
		case <-time.After(2 * time.Second):
			t.Fatal("writer lock remained held after the probe returned")
		}
		t.Fatal("connection probe held the shared writer during network I/O")
	}
}

func (p blockedConnectionProbe) finishAndCheckStaleResult(t *testing.T) {
	t.Helper()
	p.service.writes.Unlock()
	p.release()
	select {
	case err := <-p.finished:
		if !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("stale connection probe error = %v, want conflict", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale connection probe did not return")
	}
	p.service.mu.Lock()
	defer p.service.mu.Unlock()
	for _, current := range p.service.cfg.Connections {
		if current.ID == p.conn.ID {
			if !current.Tested || current.Capabilities != p.conn.Capabilities || current.Error != p.conn.Error {
				t.Fatalf("stale probe overwrote the previous result: %+v", current)
			}
			return
		}
	}
	t.Fatal("connection disappeared while checking the stale probe result")
}

func TestTestConnectionRejectsConfigChangedDuringProbe(t *testing.T) {
	p := startBlockedConnectionProbe(t)
	p.waitForProbe(t)
	p.lockWriterDuringProbe(t)
	p.service.mu.Lock()
	for i := range p.service.cfg.Connections {
		if p.service.cfg.Connections[i].ID == p.conn.ID {
			p.service.cfg.Connections[i].Prefix = "changed-scope"
		}
	}
	err := p.service.saveLocked()
	p.service.mu.Unlock()
	if err != nil {
		p.service.writes.Unlock()
		t.Fatal(err)
	}
	p.finishAndCheckStaleResult(t)
}

func TestTestConnectionRejectsCredentialsChangedDuringProbe(t *testing.T) {
	p := startBlockedConnectionProbe(t)
	p.waitForProbe(t)
	p.lockWriterDuringProbe(t)
	raw, err := json.Marshal(storage.Credentials{Username: "alice", Password: "new-secret"})
	if err != nil {
		p.service.writes.Unlock()
		t.Fatal(err)
	}
	if err = p.vault.Set("connection:"+p.conn.ID, string(raw)); err != nil {
		p.service.writes.Unlock()
		t.Fatal(err)
	}
	p.finishAndCheckStaleResult(t)
}
