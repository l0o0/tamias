package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"testing"
)

type atomicTestVault struct {
	mu      sync.Mutex
	values  map[string]string
	failSet func(string, string) bool
}

func newAtomicTestVault() *atomicTestVault {
	return &atomicTestVault{values: map[string]string{}}
}

func (v *atomicTestVault) Get(key string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[key]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}

func (v *atomicTestVault) Set(key, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.failSet != nil && v.failSet(key, value) {
		return errors.New("injected vault failure")
	}
	v.values[key] = value
	return nil
}

func (v *atomicTestVault) Delete(key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, key)
	return nil
}

func settingsDAV(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/dav"
}

func setupCredentialUpdateService(t *testing.T, vault *atomicTestVault) (*Service, string, storage.Config, storage.Store, string) {
	t.Helper()
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err = s.Demo(); err != nil {
		t.Fatal(err)
	}
	id := ID()
	cfg := storage.Config{ID: id, Name: "credential test", Kind: "webdav", Endpoint: settingsDAV(t), Username: "old-user"}
	oldStore := storage.NewMemory()
	oldCredentials, err := json.Marshal(storage.Credentials{Username: "old-user", Password: "old-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Set("connection:"+id, string(oldCredentials)); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, Connection{Config: cfg, Tested: true, Capabilities: storage.Capabilities{ConditionalWrite: true}})
	s.stores[id] = oldStore
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return s, id, cfg, oldStore, string(oldCredentials)
}

func failSettingsUpdates(t *testing.T, s *Service) {
	t.Helper()
	s.mu.Lock()
	if err := s.saveLocked(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_settings_updates BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT, 'injected settings failure'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCredentialsRollsBackVaultConfigAndStoreOnDBFailure(t *testing.T) {
	vault := newAtomicTestVault()
	s, id, cfg, oldStore, oldRaw := setupCredentialUpdateService(t, vault)
	failSettingsUpdates(t, s)
	input := ConnectionInput{Config: cfg, Password: "new-secret"}
	input.Username = cfg.Username
	err := s.UpdateCredentials(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "injected settings failure") {
		t.Fatalf("UpdateCredentials error = %v", err)
	}
	gotRaw, err := vault.Get("connection:" + id)
	if err != nil || gotRaw != oldRaw {
		t.Fatalf("vault was not rolled back: %q err=%v", gotRaw, err)
	}
	connection, err := s.connection(id)
	if err != nil || connection.Config != cfg || !connection.Tested {
		t.Fatalf("connection config/state changed after failed update: %#v err=%v", connection, err)
	}
	gotStore, err := s.store(id)
	if err != nil || gotStore != oldStore {
		t.Fatalf("connection store was not rolled back: same=%v err=%v", gotStore == oldStore, err)
	}
}

func TestUpdateCredentialsDisablesConnectionWhenVaultRollbackFails(t *testing.T) {
	vault := newAtomicTestVault()
	s, id, cfg, _, oldRaw := setupCredentialUpdateService(t, vault)
	failSettingsUpdates(t, s)
	vault.failSet = func(key, value string) bool { return key == "connection:"+id && value == oldRaw }
	input := ConnectionInput{Config: cfg, Password: "new-secret"}
	input.Username = cfg.Username
	err := s.UpdateCredentials(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "连接已在当前运行中禁用") {
		t.Fatalf("failed rollback was not reported: %v", err)
	}
	connection, getErr := s.connection(id)
	if getErr != nil || connection.Tested || connection.Capabilities.ConditionalWrite || !strings.Contains(connection.Error, "连接已禁用") {
		t.Fatalf("connection was not visibly disabled: %#v err=%v", connection, getErr)
	}
	store, storeErr := s.store(id)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if _, listErr := store.List(context.Background(), ""); listErr == nil || !strings.Contains(listErr.Error(), "连接已禁用") {
		t.Fatalf("disabled connection store was usable: %v", listErr)
	}
	if raw, vaultErr := vault.Get("connection:" + id); vaultErr != nil || raw != "" {
		t.Fatalf("uncertain credentials were not poisoned for restart safety: %q err=%v", raw, vaultErr)
	}
}

func TestSetPreferencesRollsBackAutoStartWhenDatabaseFails(t *testing.T) {
	s, _ := testService(t)
	failSettingsUpdates(t, s)
	autoStart := false
	var calls []bool
	s.SetAutoStart = func(value bool) error {
		autoStart = value
		calls = append(calls, value)
		return nil
	}
	s.RequestNotifications = func() (bool, error) {
		if !s.stagingMu.TryLock() {
			return false, errors.New("staging lock held during system prompt")
		}
		s.stagingMu.Unlock()
		return true, nil
	}
	prefs := s.Preferences()
	prefs.AutoStart = true
	prefs.Notifications = true
	err := s.SetPreferences(prefs)
	if err == nil || !strings.Contains(err.Error(), "系统通知授权可能已授予") {
		t.Fatalf("database failure did not explain notification grant: %v", err)
	}
	if autoStart || len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("auto-start side effect was not rolled back: state=%v calls=%v", autoStart, calls)
	}
	got := s.Preferences()
	if got.AutoStart || got.Notifications {
		t.Fatalf("failed preference update changed in-memory settings: %#v", got)
	}
}

func TestSetPreferencesRechecksBudgetAfterSystemPrompt(t *testing.T) {
	s, _ := testService(t)
	prefs := s.Preferences()
	prefs.StagingBytes = prefs.MaxFileBytes
	if err := s.SetPreferences(prefs); err != nil {
		t.Fatal(err)
	}
	prefs = s.Preferences()
	prefs.Notifications = true
	lockWasFree := false
	s.RequestNotifications = func() (bool, error) {
		lockWasFree = s.stagingMu.TryLock()
		if lockWasFree {
			s.stagingMu.Unlock()
		}
		s.stagingMu.Lock()
		s.stagingReservations["system-prompt-test"] = prefs.StagingBytes + 1
		s.stagingMu.Unlock()
		return true, nil
	}
	err := s.SetPreferences(prefs)
	s.stagingMu.Lock()
	delete(s.stagingReservations, "system-prompt-test")
	s.stagingMu.Unlock()
	if !lockWasFree {
		t.Fatal("staging transfers remained blocked during the OS authorization prompt")
	}
	if err == nil || !strings.Contains(err.Error(), "额度不能低于") {
		t.Fatalf("budget change during the prompt was not rechecked: %v", err)
	}
	if s.Preferences().Notifications {
		t.Fatal("preferences committed after the post-prompt budget check failed")
	}
}
