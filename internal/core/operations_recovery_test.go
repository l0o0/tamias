package core

import (
	"context"
	"errors"
	"io"
	"strings"
	"tamiops/internal/storage"
	"testing"
	"time"
)

type conflictDeleteStore struct{ storage.Store }

func (s conflictDeleteStore) Delete(context.Context, string, storage.Condition) error {
	return storage.ErrConflict
}

type failPutAtStore struct {
	storage.Store
	key string
}

func (s failPutAtStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond storage.Condition) (storage.Entry, error) {
	if key == s.key {
		return storage.Entry{}, storage.ErrUnsupported
	}
	return s.Store.Put(ctx, key, body, size, cond)
}

type wrongCommitStore struct{ storage.Store }

func (s wrongCommitStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, cond storage.Condition) (storage.Entry, error) {
	_, err := s.Store.Put(ctx, key, strings.NewReader("other"), int64(len("other")), cond)
	if err != nil {
		return storage.Entry{}, err
	}
	return storage.Entry{}, errors.New("response lost")
}

func TestMoveKeepsSourceWhenConditionalDeleteFails(t *testing.T) {
	s, id := testService(t)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	if _, err := b.Put(ctx, "source.txt", strings.NewReader("payload"), 7, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[id] = conflictDeleteStore{s.stores[id]}
	s.mu.Unlock()
	if _, err := b.Move(ctx, "source.txt", "target.txt", false); err == nil {
		t.Fatal("expected source deletion failure")
	}
	if got := readBody(t, b, "source.txt"); got != "payload" {
		t.Fatalf("source was removed or changed: %q", got)
	}
	if got := readBody(t, b, "target.txt"); got != "payload" {
		t.Fatalf("verified copy missing: %q", got)
	}
	var state string
	if err := s.db.QueryRow("SELECT state FROM operations WHERE kind='move' ORDER BY created DESC LIMIT 1").Scan(&state); err != nil || state != "partial" {
		t.Fatalf("move state = %q, err = %v", state, err)
	}
}

func TestDirectoryMoveStopsAtFailedCopyAndKeepsThatSource(t *testing.T) {
	s, id := testService(t)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	if err := b.Mkdir(ctx, "source"); err != nil {
		t.Fatal(err)
	}
	if err := b.Mkdir(ctx, "source/nested"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "source/a.txt", strings.NewReader("first"), 5, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "source/nested/b.txt", strings.NewReader("second"), 6, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[id] = failPutAtStore{Store: s.stores[id], key: "target/nested/b.txt"}
	s.mu.Unlock()
	if _, err := b.Move(ctx, "source", "target", false); !errors.Is(err, storage.ErrPartialOperation) {
		t.Fatalf("expected a partial directory move, got %v", err)
	}
	if _, err := b.Stat(ctx, "source/a.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("already copied file was not moved: %v", err)
	}
	if got := readBody(t, b, "target/a.txt"); got != "first" {
		t.Fatalf("first destination missing: %q", got)
	}
	if got := readBody(t, b, "source/nested/b.txt"); got != "second" {
		t.Fatalf("failed copy deleted its source: %q", got)
	}
	if _, err := b.Stat(ctx, "target/nested/b.txt"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed destination unexpectedly exists: %v", err)
	}
}

func TestCopyDoesNotReplaceExistingTargetWithoutOverwrite(t *testing.T) {
	s, id := testService(t)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	if _, err := b.Put(ctx, "source.txt", strings.NewReader("source"), 6, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "target.txt", strings.NewReader("target"), 6, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Copy(ctx, "source.txt", "target.txt", false); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if got := readBody(t, b, "source.txt"); got != "source" {
		t.Fatal(got)
	}
	if got := readBody(t, b, "target.txt"); got != "target" {
		t.Fatalf("target was overwritten: %q", got)
	}
}

func TestDeleteTreeRequiresFreshPreviewToken(t *testing.T) {
	s, id := testService(t)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	if err := b.Mkdir(ctx, "bulk"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "bulk/a.txt", strings.NewReader("a"), 1, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	preview, err := b.PreviewDeleteTree(ctx, "bulk")
	if err != nil || len(preview.Files) != 1 {
		t.Fatalf("preview = %#v, err = %v", preview, err)
	}
	if _, err = b.Put(ctx, "bulk/b.txt", strings.NewReader("b"), 1, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if err = b.DeleteTreeWithPreview(ctx, "bulk", preview.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale delete preview accepted: %v", err)
	}
	if got := readBody(t, b, "bulk/a.txt"); got != "a" {
		t.Fatalf("stale preview deleted a file: %q", got)
	}
	current, err := b.PreviewDeleteTree(ctx, "bulk")
	if err != nil || len(current.Files) != 2 {
		t.Fatalf("fresh preview = %#v, err = %v", current, err)
	}
	if err = b.DeleteTreeWithPreview(ctx, "bulk", current.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Stat(ctx, "bulk"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("directory remains after confirmed deletion: %v", err)
	}
}

func TestUnknownUploadReconcilesOnlyWhenContentMatches(t *testing.T) {
	t.Run("matching content", func(t *testing.T) {
		s, id := testService(t)
		s.mu.Lock()
		s.stores[id] = uncertainStore{s.stores[id]}
		s.mu.Unlock()
		b := Backend{Service: s, ConnectionID: id}
		ctx := context.Background()
		if _, err := b.Put(ctx, "uncertain.txt", strings.NewReader("payload"), 7, storage.Condition{IfNoneMatch: true}); err == nil {
			t.Fatal("expected lost response")
		}
		pending, err := s.PendingOperations()
		if err != nil || len(pending) != 1 || pending[0].State != "uncertain" {
			t.Fatalf("pending = %#v, err = %v", pending, err)
		}
		if err = s.ReconcileOperation(ctx, pending[0].ID); err != nil {
			t.Fatal(err)
		}
		var state string
		if err = s.db.QueryRow("SELECT state FROM operations WHERE id=?", pending[0].ID).Scan(&state); err != nil || state != "committed" {
			t.Fatalf("state = %q, err = %v", state, err)
		}
	})

	t.Run("mismatched content stays frozen", func(t *testing.T) {
		s, id := testService(t)
		s.mu.Lock()
		s.stores[id] = wrongCommitStore{s.stores[id]}
		s.mu.Unlock()
		b := Backend{Service: s, ConnectionID: id}
		ctx := context.Background()
		if _, err := b.Put(ctx, "uncertain.txt", strings.NewReader("planned"), 7, storage.Condition{IfNoneMatch: true}); err == nil {
			t.Fatal("expected lost response")
		}
		pending, err := s.PendingOperations()
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending = %#v, err = %v", pending, err)
		}
		if err = s.ReconcileOperation(ctx, pending[0].ID); err == nil {
			t.Fatal("mismatched remote content was accepted")
		}
		if _, err = b.Put(ctx, "uncertain.txt", strings.NewReader("replace"), 7, storage.Condition{}); err == nil || !strings.Contains(err.Error(), "待核对") {
			t.Fatalf("uncertain target was not frozen: %v", err)
		}
		if got := readBody(t, b, "uncertain.txt"); got != "other" {
			t.Fatalf("reconciliation rewrote remote content: %q", got)
		}
	})
}

func TestUnknownCrossConnectionMoveFreezesBothResources(t *testing.T) {
	s, sourceID := testService(t)
	source := Backend{Service: s, ConnectionID: sourceID}
	ctx := context.Background()
	if _, err := source.Put(ctx, "source.txt", strings.NewReader("payload"), 7, storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	other := s.cfg.Connections[0]
	other.ID = ID()
	other.Name = "destination"
	s.cfg.Connections = append(s.cfg.Connections, other)
	s.stores[other.ID] = storage.NewMemory()
	destStore := s.stores[other.ID]
	s.mu.Unlock()
	s.mu.Lock()
	s.stores[other.ID] = uncertainStore{destStore}
	s.mu.Unlock()
	destination := Backend{Service: s, ConnectionID: other.ID}
	if _, err := destination.MoveFrom(ctx, source, "source.txt", "target.txt", false); err == nil {
		t.Fatal("expected destination response loss")
	}
	if _, err := source.Put(ctx, "source.txt", strings.NewReader("changed"), 7, storage.Condition{}); err == nil || !strings.Contains(err.Error(), "待核对") {
		t.Fatalf("uncertain move did not freeze source: %v", err)
	}
	if _, err := destination.Put(ctx, "target.txt", strings.NewReader("changed"), 7, storage.Condition{}); err == nil || !strings.Contains(err.Error(), "待核对") {
		t.Fatalf("uncertain move did not freeze destination: %v", err)
	}
	pending, err := s.PendingOperations()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %#v, err = %v", pending, err)
	}
	if err = s.ReconcileOperation(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, source, "source.txt"); got != "payload" {
		t.Fatalf("source was lost during reconciliation: %q", got)
	}
	if got := readBody(t, destination, "target.txt"); got != "payload" {
		t.Fatalf("verified destination missing: %q", got)
	}
	var state string
	if err = s.db.QueryRow("SELECT state FROM operations WHERE id=?", pending[0].ID).Scan(&state); err != nil || state != "partial" {
		t.Fatalf("move state = %q, err = %v", state, err)
	}
}

func TestRecoveryRestoreRejectsChangedPreviewVersion(t *testing.T) {
	s, id := testService(t)
	setNormalRecoveryForTest(t, s, id, true)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	first, err := b.Put(ctx, "restore.txt", strings.NewReader("before"), 6, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(ctx, "restore.txt", strings.NewReader("middle"), 6, storage.Condition{IfMatch: first.ETag}); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.recoveries()
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("recoveries = %#v, err = %v", recoveries, err)
	}
	preview, err := s.PreviewRecovery(ctx, recoveries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Current == nil || preview.Current.ETag == "" || !preview.CurrentExists {
		t.Fatalf("preview did not expose current version: %#v", preview)
	}
	if _, err = b.Put(ctx, "restore.txt", strings.NewReader("latest"), 6, storage.Condition{IfMatch: preview.ExpectedCurrentETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RestoreRecovery(ctx, preview.ID, preview.ExpectedCurrentETag); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale recovery preview was accepted: %v", err)
	}
	if got := readBody(t, b, "restore.txt"); got != "latest" {
		t.Fatalf("restore replaced the latest version: %q", got)
	}
}

func TestRecoveryCanRestoreVerifiedCopyToOriginalRemotePath(t *testing.T) {
	s, id := testService(t)
	setNormalRecoveryForTest(t, s, id, true)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	first, err := b.Put(ctx, "restore.txt", strings.NewReader("before"), 6, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(ctx, "restore.txt", strings.NewReader("after!"), 6, storage.Condition{IfMatch: first.ETag}); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.recoveries()
	if err != nil || len(recoveries) != 1 || !recoveries[0].CanRestoreToRemote {
		t.Fatalf("recoveries = %#v, err = %v", recoveries, err)
	}
	preview, err := s.PreviewRecovery(ctx, recoveries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RestoreRecovery(ctx, preview.ID, preview.ExpectedCurrentETag); err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, b, "restore.txt"); got != "before" {
		t.Fatalf("restored content = %q", got)
	}
	var state string
	if err = s.db.QueryRow("SELECT state FROM operations WHERE kind='restore' ORDER BY created DESC LIMIT 1").Scan(&state); err != nil || state != "committed" {
		t.Fatalf("restore state = %q, err = %v", state, err)
	}
}

func TestUncertainRecoveryCopyCannotBePrunedOrManuallyRemoved(t *testing.T) {
	s, id := testService(t)
	setNormalRecoveryForTest(t, s, id, true)
	b := Backend{Service: s, ConnectionID: id}
	ctx := context.Background()
	first, err := b.Put(ctx, "keep.txt", strings.NewReader("before"), 6, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[id] = uncertainStore{s.stores[id]}
	s.mu.Unlock()
	if _, err = b.Put(ctx, "keep.txt", strings.NewReader("after!"), 6, storage.Condition{IfMatch: first.ETag}); err == nil {
		t.Fatal("expected lost response")
	}
	recoveries, err := s.recoveries()
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("recoveries = %#v, err = %v", recoveries, err)
	}
	if recoveries[0].State != "uncertain" {
		t.Fatalf("state = %q", recoveries[0].State)
	}
	if err = s.DeleteRecovery(recoveries[0].ID); err == nil {
		t.Fatal("manually deleted recovery data for an uncertain operation")
	}
	removed, err := s.PruneRecoveries(time.Now().AddDate(5, 0, 0))
	if err != nil || len(removed) != 0 {
		t.Fatalf("prune removed uncertain data: %v, %v", removed, err)
	}
	if _, data, readErr := s.readRecovery(recoveries[0].ID); readErr != nil {
		t.Fatalf("uncertain recovery data disappeared: %v", readErr)
	} else {
		data.Close()
	}
}
