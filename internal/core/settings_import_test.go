package core

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tamiops/internal/storage"
)

func importedWebDAVConfig(id, name string) storage.Config {
	return storage.Config{ID: id, Name: name, Kind: "webdav", Endpoint: "https://example.test/dav", Username: "imported-user"}
}

func importedDAV(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/dav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server.URL + "/dav"
}

func TestImportConfigurationAllowsCredentialRepair(t *testing.T) {
	vault := newAtomicTestVault()
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	connectionConfig := importedWebDAVConfig("source", "imported")
	connectionConfig.Endpoint = importedDAV(t)
	if _, err = s.ImportConfiguration(ConfigurationExport{Version: 1, Connections: []storage.Config{connectionConfig}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	imported := s.cfg.Connections[0]
	s.mu.Unlock()

	key := "connection:" + imported.ID
	raw, err := vault.Get(key)
	if err != nil {
		t.Fatalf("import did not initialize a credential entry: %v", err)
	}
	var initial storage.Credentials
	if err := json.Unmarshal([]byte(raw), &initial); err != nil || initial != (storage.Credentials{}) {
		t.Fatalf("imported placeholder credentials = %#v, err=%v", initial, err)
	}

	input := ConnectionInput{Config: storage.Config{ID: imported.ID, Username: imported.Username}, Password: "repaired-password"}
	input.Username = imported.Username
	if err := s.UpdateCredentials(context.Background(), input); err != nil {
		t.Fatalf("UpdateCredentials after import: %v", err)
	}
	updatedRaw, err := vault.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	var updated storage.Credentials
	if err := json.Unmarshal([]byte(updatedRaw), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Username != imported.Username || updated.Password != "repaired-password" {
		t.Fatalf("repaired credentials = %#v", updated)
	}
	connection, err := s.connection(imported.ID)
	if err != nil || !connection.Tested || connection.Error != "" {
		t.Fatalf("connection was not marked usable after credential repair: %#v err=%v", connection, err)
	}
}

func TestImportConfigurationRollsBackPartialVaultInitialization(t *testing.T) {
	vault := newAtomicTestVault()
	sets := 0
	vault.failSet = func(key, _ string) bool {
		if strings.HasPrefix(key, "connection:") {
			sets++
			return sets == 2
		}
		return false
	}
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	in := ConfigurationExport{Version: 1, Connections: []storage.Config{
		importedWebDAVConfig("one", "first"),
		importedWebDAVConfig("two", "second"),
	}}
	if _, err := s.ImportConfiguration(in); err == nil || !strings.Contains(err.Error(), "injected vault failure") {
		t.Fatalf("ImportConfiguration error = %v, want injected vault failure", err)
	}
	if sets != 2 {
		t.Fatalf("attempted connection credential writes = %d, want 2", sets)
	}
	vault.mu.Lock()
	remaining := len(vault.values)
	vault.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("partial credential initialization left %d vault entries", remaining)
	}
	s.mu.Lock()
	connectionCount := len(s.cfg.Connections)
	s.mu.Unlock()
	if connectionCount != 0 {
		t.Fatalf("vault failure partially imported %d connections", connectionCount)
	}
}

func TestImportConfigurationRollsBackCredentialsWhenDatabaseCommitFails(t *testing.T) {
	vault := newAtomicTestVault()
	s, err := New(t.TempDir(), vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.ensureBackupSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_imported_backups BEFORE INSERT ON backup_jobs BEGIN SELECT RAISE(ABORT,'injected database failure'); END`); err != nil {
		t.Fatal(err)
	}
	in := ConfigurationExport{
		Version:     1,
		Connections: []storage.Config{importedWebDAVConfig("source", "imported")},
		BackupJobs:  []BackupJob{{Name: "backup", ConnectionID: "source", RemotePrefix: "archive", RetainRecent: 2}},
	}
	if _, err := s.ImportConfiguration(in); err == nil || !strings.Contains(err.Error(), "injected database failure") {
		t.Fatalf("ImportConfiguration error = %v, want injected database failure", err)
	}
	vault.mu.Lock()
	remaining := len(vault.values)
	vault.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("database rollback left %d imported credential entries", remaining)
	}
	s.mu.Lock()
	connectionCount := len(s.cfg.Connections)
	s.mu.Unlock()
	if connectionCount != 0 {
		t.Fatalf("database failure partially imported %d connections", connectionCount)
	}
}
