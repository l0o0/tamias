package core

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"testing"
	"time"
)

func TestCloseCancelsAndJoinsGatewayReader(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	if _, err := b.Put(context.Background(), "slow", strings.NewReader("content"), 7, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	remote := installGatedDownloadStore(t, s, id)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	g, password, err := s.AddGateway(Gateway{Name: "test", ConnectionID: id, Username: "test", Port: port, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.StartGateway(g.ID); err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		request, _ := http.NewRequest("GET", g.URL+"slow", nil)
		request.SetBasicAuth("test", password)
		response, e := http.DefaultClient.Do(request)
		if e == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	waitDownloadSignal(t, remote.started)
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		close(remote.release)
		<-closed
		t.Fatal("gateway read was not cancelled and joined")
	}
	select {
	case <-remote.closed:
	default:
		t.Fatal("database closed before reader close")
	}
	<-clientDone
}

type changingManifestStore struct {
	storage.Store
	changed bool
}

func (s *changingManifestStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition storage.Condition) (storage.Entry, error) {
	if strings.HasSuffix(key, "/files/file") && !s.changed {
		s.changed = true
		manifest := path.Join(strings.TrimSuffix(key, "/files/file"), "manifest.json")
		current, err := s.Store.Stat(ctx, manifest)
		if err != nil {
			return storage.Entry{}, err
		}
		if _, err = s.Store.Put(ctx, manifest, strings.NewReader("foreign"), 7, storage.Condition{IfMatch: current.ETag}); err != nil {
			return storage.Entry{}, err
		}
	}
	return s.Store.Put(ctx, key, body, size, condition)
}
func TestFinalBackupManifestCannotOverwriteConcurrentChange(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	base, _ := s.store(id)
	s.mu.Lock()
	s.stores[id] = &changingManifestStore{Store: base}
	s.mu.Unlock()
	job, err := s.CreateBackupJob(BackupJob{Name: "manifest", ConnectionID: id, LocalPath: local, RemotePrefix: "backups", RetainRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := s.RunBackup(context.Background(), job.ID, plan.Token)
	if snapshot.Status != "partial" {
		t.Fatal("changed manifest incorrectly accepted", snapshot)
	}
	reader, _, err := base.Open(context.Background(), path.Join("backups/.tamiops-backup", snapshot.ID, "manifest.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, _ := io.ReadAll(reader)
	if string(data) != "foreign" {
		t.Fatal("external manifest was overwritten")
	}
}
