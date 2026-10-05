package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

type historyTestStore struct {
	storage.Store
	version storage.ObjectVersion
	content []byte
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *historyTestStore) ListVersions(ctx context.Context, key string) ([]storage.ObjectVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key != s.version.Entry.Path {
		return nil, storage.ErrNotFound
	}
	return []storage.ObjectVersion{s.version}, nil
}

func (s *historyTestStore) OpenVersion(ctx context.Context, key, versionID string) (io.ReadCloser, storage.Entry, error) {
	if key != s.version.Entry.Path || versionID != s.version.VersionID {
		return nil, storage.Entry{}, storage.ErrNotFound
	}
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, storage.Entry{}, ctx.Err()
		}
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), s.content...))), s.version.Entry, nil
}

func setupVersionRestore(t *testing.T, current, historical string) (*Service, string, *historyTestStore, storage.Entry) {
	t.Helper()
	s, connectionID := testService(t)
	ctx := context.Background()
	b := Backend{Service: s, ConnectionID: connectionID}
	currentEntry, err := b.Put(ctx, "restore.txt", strings.NewReader(current), int64(len(current)), storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	versionID := "version:opaque/id?do-not-rewrite"
	versionEntry := storage.Entry{
		Path: "restore.txt", Name: path.Base("restore.txt"), Size: int64(len(historical)),
		ETag: `"historical-opaque"`, VersionID: versionID,
	}
	history := &historyTestStore{
		Store:   s.stores[connectionID],
		version: storage.ObjectVersion{Entry: versionEntry, VersionID: versionID},
		content: []byte(historical),
	}
	s.mu.Lock()
	s.stores[connectionID] = history
	s.mu.Unlock()
	return s, connectionID, history, currentEntry
}

func TestVersionRestoreWritesExactVersionAndKeepsReplacedContent(t *testing.T) {
	s, connectionID, _, current := setupVersionRestore(t, "current contents", "historical contents")
	setNormalRecoveryForTest(t, s, connectionID, true)
	ctx := context.Background()
	preview, err := s.previewVersionRestore(ctx, connectionID, "restore.txt", "version:opaque/id?do-not-rewrite")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token == "" || preview.Path != "restore.txt" || preview.ExpectedCurrentETag != current.ETag || !preview.CurrentExists {
		t.Fatalf("restore preview did not pin the exact target: %+v", preview)
	}
	result, err := s.restoreVersion(ctx, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "restore.txt" || readBody(t, Backend{Service: s, ConnectionID: connectionID}, "restore.txt") != "historical contents" {
		t.Fatalf("historical version was not written to the original path: %+v", result)
	}
	recoveries, err := s.recoveries()
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("replaced content was not preserved in recovery: recoveries=%+v err=%v", recoveries, err)
	}
	_, recovered, err := s.readRecovery(recoveries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	recoveredBytes, err := io.ReadAll(recovered)
	if err != nil || string(recoveredBytes) != "current contents" {
		t.Fatalf("recovery copy=%q err=%v", recoveredBytes, err)
	}
}

func TestVersionRestoreRejectsExpiredAndChangedTargetPreview(t *testing.T) {
	s, connectionID, _, current := setupVersionRestore(t, "before", "historical")
	ctx := context.Background()
	preview, err := s.previewVersionRestore(ctx, connectionID, "restore.txt", "version:opaque/id?do-not-rewrite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE version_restore_previews SET expires=? WHERE token=?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), preview.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.restoreVersion(ctx, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expired preview was accepted: %v", err)
	}

	preview, err = s.previewVersionRestore(ctx, connectionID, "restore.txt", "version:opaque/id?do-not-rewrite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (Backend{Service: s, ConnectionID: connectionID}).Put(ctx, "restore.txt", strings.NewReader("newer contents"), int64(len("newer contents")), storage.Condition{IfMatch: current.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.restoreVersion(ctx, preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("preview accepted a changed target: %v", err)
	}
	if got := readBody(t, Backend{Service: s, ConnectionID: connectionID}, "restore.txt"); got != "newer contents" {
		t.Fatalf("stale preview damaged newer target: %q", got)
	}
}

func TestVersionRestoreTokenCannotBeAppliedConcurrently(t *testing.T) {
	s, connectionID, history, _ := setupVersionRestore(t, "current", "historical")
	history.entered = make(chan struct{})
	release := make(chan struct{})
	history.release = release
	preview, err := s.previewVersionRestore(context.Background(), connectionID, "restore.txt", "version:opaque/id?do-not-rewrite")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		_, restoreErr := s.restoreVersion(context.Background(), preview.Token)
		first <- restoreErr
	}()
	select {
	case <-history.entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("restore did not reach the version read")
	}
	if _, err = s.restoreVersion(context.Background(), preview.Token); !errors.Is(err, storage.ErrConflict) {
		close(release)
		t.Fatalf("same preview token was claimed twice: %v", err)
	}
	close(release)
	select {
	case err = <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first restore did not finish")
	}
	if got := readBody(t, Backend{Service: s, ConnectionID: connectionID}, "restore.txt"); got != "historical" {
		t.Fatalf("concurrent restore produced wrong content: %q", got)
	}
}
