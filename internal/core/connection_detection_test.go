package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tamiops/internal/storage"
)

func TestAddConnectionAutomaticallyPersistsConditionalCapabilities(t *testing.T) {
	s, _ := testService(t)
	endpoint, dav := newEditDAV(t)

	connection, err := s.AddConnection(context.Background(), ConnectionInput{Config: storage.Config{
		Name: "auto-detected", Kind: "webdav", Endpoint: endpoint, Username: "alice",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !connection.Tested || connection.CompatibilityProfile != connectionProfileConditional || !connection.Capabilities.ConditionalWrite || !connection.Capabilities.ConditionalDelete {
		t.Fatalf("automatic capability detection result = %+v", connection)
	}
	if !connectionDetectionCurrent(connection) || connection.CapabilityVersion != connectionCapabilityVersion {
		t.Fatalf("automatic detection metadata is not current: %+v", connection)
	}
	if _, err = time.Parse(time.RFC3339Nano, connection.CapabilitiesCheckedAt); err != nil {
		t.Fatalf("invalid capability timestamp %q: %v", connection.CapabilitiesCheckedAt, err)
	}
	dav.mu.Lock()
	remaining := len(dav.objects)
	dav.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("successful automatic probe left %d temporary objects", remaining)
	}

	var raw string
	if err = s.db.QueryRow(`SELECT data FROM settings WHERE id=1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved config
	if err = json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	for _, stored := range saved.Connections {
		if stored.ID == connection.ID {
			if stored.CompatibilityProfile != connectionProfileConditional || stored.CapabilityVersion != connectionCapabilityVersion || stored.CapabilitiesCheckedAt == "" {
				t.Fatalf("automatic detection was not persisted: %+v", stored)
			}
			return
		}
	}
	t.Fatal("new WebDAV connection was not persisted")
}

func TestAddConnectionSavesReadableConditionalRestrictionInStandardMode(t *testing.T) {
	s, _ := testService(t)
	endpoint, dav := newEditDAV(t)
	dav.mu.Lock()
	dav.ignoreCreateCondition = true
	dav.mu.Unlock()

	connection, err := s.AddConnection(context.Background(), ConnectionInput{Config: storage.Config{
		Name: "compatible", Kind: "webdav", Endpoint: endpoint, Username: "alice", WriteMode: storage.WriteModeStandard,
	}})
	if err != nil {
		t.Fatalf("readable WebDAV server with unsupported conditions should be saved: %v", err)
	}
	if !connection.Tested || connection.CompatibilityProfile != connectionProfileCompatible || connection.Capabilities.ConditionalWrite || connection.Capabilities.ConditionalDelete {
		t.Fatalf("unsupported conditions were misclassified: %+v", connection)
	}
	if !connectionDetectionCurrent(connection) || connection.WriteRestriction == "" || !strings.Contains(connection.WriteRestriction, storage.ErrConditionalUnsupported.Error()) {
		t.Fatalf("conditional write restriction was not persisted: %+v", connection)
	}
}

func TestAddConnectionBlocksWhenReadCheckFails(t *testing.T) {
	s, _ := testService(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	_, err := s.AddConnection(context.Background(), ConnectionInput{Config: storage.Config{
		Name: "unreadable", Kind: "webdav", Endpoint: server.URL + "/dav", Username: "alice",
	}})
	if err == nil {
		t.Fatal("connection with failed read check was saved")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, connection := range s.cfg.Connections {
		if connection.Name == "unreadable" {
			t.Fatalf("unreadable connection was persisted: %+v", connection)
		}
	}
}

func TestAddS3ConnectionAutomaticallyRunsCapabilityDetection(t *testing.T) {
	fake := newS3ProtocolFake(t)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	s, _ := testService(t)

	connection, err := s.AddConnection(context.Background(), ConnectionInput{
		Config:    storage.Config{Kind: "s3", Name: "auto-detected-s3", Endpoint: server.URL, Region: "us-east-1", Bucket: "test", Prefix: "scope", PathStyle: true},
		AccessKey: "test-access", SecretKey: "test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !connection.Tested || connection.CompatibilityProfile != connectionProfileConditional || !connectionDetectionCurrent(connection) || !connection.Capabilities.ConditionalWrite || !connection.Capabilities.ConditionalDelete {
		t.Fatalf("S3 detection did not persist its verified conditional profile: %+v", connection)
	}
	fake.assertAllSignedScopedRequests(t)
	fake.mu.Lock()
	remaining := len(fake.objects)
	fake.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("automatic S3 probe left %d temporary objects", remaining)
	}
}

type failingProbeRecheckStore struct {
	*connectionRestrictionStore
	listCalls int
}

func (s *failingProbeRecheckStore) List(ctx context.Context, prefix string) ([]storage.Entry, error) {
	s.listCalls++
	if s.listCalls == 2 {
		return nil, errors.New("read access disappeared during probe")
	}
	return s.connectionRestrictionStore.Store.List(ctx, prefix)
}

func TestDetectionRejectsRestrictedProfileWhenPostProbeReadFails(t *testing.T) {
	store := &failingProbeRecheckStore{connectionRestrictionStore: &connectionRestrictionStore{
		Store:  storage.NewMemory(),
		putErr: storage.ErrConditionalUnsupported,
	}}
	detection, err := detectConnectionCapabilities(context.Background(), store, storage.WriteModeStandard, true)
	if err == nil || detection.CompatibilityProfile != connectionProfileFailed || detection.Capabilities != (storage.Capabilities{}) {
		t.Fatalf("failed post-probe read was treated as a compatible restriction: detection=%+v err=%v", detection, err)
	}
	if store.listCalls != 2 {
		t.Fatalf("detector performed %d read checks, want pre-probe and post-probe checks", store.listCalls)
	}
}

type cancellableProbeStore struct {
	storage.Store
	started chan struct{}
}

func (s cancellableProbeStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition storage.Condition) (storage.Entry, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return storage.Entry{}, ctx.Err()
}

func TestCanceledManualProbeDoesNotPersistFailedDetection(t *testing.T) {
	s, id := testService(t)
	before, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	remote := cancellableProbeStore{Store: storage.NewMemory(), started: make(chan struct{}, 1)}
	s.mu.Lock()
	s.stores[id] = remote
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, probeErr := s.TestConnection(ctx, id, true)
		done <- probeErr
	}()
	select {
	case <-remote.started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("probe did not reach its isolated test write")
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error = %v, want context canceled", err)
	}
	after, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("canceled probe changed persisted connection state: before=%+v after=%+v", before, after)
	}
}
