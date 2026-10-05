package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"tamiops/internal/storage"
)

func syncJob(t *testing.T, s *Service, id, dir, mode string, extra Job) Job {
	t.Helper()
	extra.Name = "sync test"
	extra.ConnectionID = id
	extra.LocalPath = dir
	extra.Direction = mode
	j, err := s.AddJob(extra)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func seedRemote(t *testing.T, b Backend, key, data string) storage.Entry {
	t.Helper()
	e, err := b.Put(context.Background(), key, strings.NewReader(data), int64(len(data)), storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func actionOf(p Plan, key string) (Action, bool) {
	for _, a := range p.Actions {
		if a.Path == key {
			return a, true
		}
	}
	return Action{}, false
}

func localText(t *testing.T, dir, key string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func makeBaseline(t *testing.T, s *Service, id, dir, mode string, key string) Job {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, filepath.FromSlash(key))), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(key)), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	if err := ensureRemoteParents(context.Background(), b, key); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, key, "base")
	j := syncJob(t, s, id, dir, mode, Job{DeleteThreshold: 10})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestSyncFirstSameNameContentMergesBaseline(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	b := Backend{s, id}
	if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("identical"), 0600); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, "same.txt", "identical")
	j := syncJob(t, s, id, dir, "both", Job{})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := actionOf(p, "same.txt")
	if !ok || !a.BaselineMerge || a.Kind != "skip" {
		t.Fatalf("same-content first sync was not merged: %+v", a)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	var localHash, remoteTag string
	if err = s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", j.ID, "same.txt").Scan(&localHash, &remoteTag); err != nil {
		t.Fatal(err)
	}
	if localHash == "" || !strongTag(remoteTag) {
		t.Fatalf("missing valid baseline: %q %q", localHash, remoteTag)
	}
	p, err = s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := actionOf(p, "same.txt"); got.Kind != "skip" || got.BaselineMerge {
		t.Fatalf("unchanged merged file was planned again: %+v", got)
	}
}

func TestRunPlanCompletionCountsTransfersDeletesAndSkips(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "upload.txt"), []byte("new local file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("identical"), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	remotePath := "sync-count-test"
	if err := ensureRemoteDirectory(context.Background(), b, remotePath); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, remotePath+"/same.txt", "identical")
	seedRemote(t, b, remotePath+"/remote-only.txt", "remove me")
	j := syncJob(t, s, id, dir, "mirror-upload", Job{RemotePath: remotePath, DeleteThreshold: 10})

	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionOf(p, "same.txt"); !ok || a.Kind != "skip" || !a.BaselineMerge {
		t.Fatalf("expected identical first-run file to be skipped and merged into the baseline: %+v", a)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	finished, err := s.job(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := "已传输 1 个变化文件；已删除 1 项；已跳过 1 项"
	if finished.Detail != want {
		t.Fatalf("completion detail %q, want %q", finished.Detail, want)
	}
	if finished.QueueDone != 3 || finished.QueueTotal != 3 || finished.Progress != 100 {
		t.Fatalf("processed-action progress was not preserved: done=%d total=%d progress=%d", finished.QueueDone, finished.QueueTotal, finished.Progress)
	}
}

func TestRunPlanEmptyPlanCompletionDetail(t *testing.T) {
	s, id := testService(t)
	b := Backend{s, id}
	remotePath := "empty-plan-test"
	if err := ensureRemoteDirectory(context.Background(), b, remotePath); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, t.TempDir(), "both", Job{RemotePath: remotePath})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("empty sync unexpectedly planned actions: %+v", p.Actions)
	}
	finished, err := s.RunPlan(context.Background(), j.ID, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Detail != "已核对，无需同步" {
		t.Fatalf("empty-plan detail %q, want a no-op result", finished.Detail)
	}
	if finished.Status != "synced" || finished.Progress != 100 || finished.QueueDone != 0 || finished.QueueTotal != 0 {
		t.Fatalf("empty-plan completion state is inconsistent: %+v", finished)
	}
}

func TestSyncRemoteUpdateReplacesSafelyAndKeepsRecovery(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "both", "note.txt")
	setSyncKeepRecovery(t, s, id, true)
	b := Backend{s, id}
	old, err := b.Stat(context.Background(), "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(context.Background(), "note.txt", strings.NewReader("remote-v2"), 9, storage.Condition{IfMatch: old.ETag}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := actionOf(p, "note.txt")
	if !ok || a.Kind != "download" {
		t.Fatalf("remote-only change did not become download: %+v", a)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	if got := localText(t, dir, "note.txt"); got != "remote-v2" {
		t.Fatalf("downloaded value %q", got)
	}
	files, err := filepath.Glob(filepath.Join(s.dir, "recovery", "*.data"))
	if err != nil || len(files) == 0 {
		t.Fatalf("old local version was not retained: %v %v", files, err)
	}
	found := false
	for _, file := range files {
		raw, e := os.ReadFile(file)
		if e == nil && string(raw) == "base" {
			found = true
		}
	}
	if !found {
		t.Fatal("recovery does not contain the replaced local version")
	}
	var localHash, remoteTag string
	if err = s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", j.ID, "note.txt").Scan(&localHash, &remoteTag); err != nil {
		t.Fatal(err)
	}
	current, _ := b.Stat(context.Background(), "note.txt")
	if remoteTag != current.ETag {
		t.Fatal("baseline advanced to the wrong remote revision")
	}
}

func TestSyncDownloadRacePreservesConcurrentLocalEdit(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "both", "race.txt")
	b := Backend{s, id}
	old, _ := b.Stat(context.Background(), "race.txt")
	if _, err := b.Put(context.Background(), "race.txt", strings.NewReader("remote"), 6, storage.Condition{IfMatch: old.ETag}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingOpenStore{Store: s.stores[id], started: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	s.stores[id] = blocking
	s.mu.Unlock()
	result := make(chan error, 1)
	go func() { _, e := s.RunPlan(context.Background(), j.ID, p.Token); result <- e }()
	select {
	case <-blocking.started:
	case <-time.After(3 * time.Second):
		t.Fatal("download never started")
	}
	if err = os.WriteFile(filepath.Join(dir, "race.txt"), []byte("concurrent-edit"), 0600); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish")
	}
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("concurrent edit was not reported as conflict: %v", err)
	}
	if got := localText(t, dir, "race.txt"); got != "concurrent-edit" {
		t.Fatalf("concurrent content was overwritten: %q", got)
	}
	var localHash, remoteTag string
	if err = s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", j.ID, "race.txt").Scan(&localHash, &remoteTag); err != nil {
		t.Fatal(err)
	}
	if localHash == "" {
		t.Fatal("failed replacement unexpectedly removed baseline")
	}
}

type blockingOpenStore struct {
	storage.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingOpenStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	s.once.Do(func() { close(s.started); <-s.release })
	return s.Store.Open(ctx, key, etag)
}

func TestSyncConflictResolutions(t *testing.T) {
	for _, choice := range []string{"local", "remote", "keep-both"} {
		t.Run(choice, func(t *testing.T) {
			s, id := testService(t)
			dir := t.TempDir()
			j := makeBaseline(t, s, id, dir, "both", "conflict.txt")
			b := Backend{s, id}
			if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("local-v2"), 0600); err != nil {
				t.Fatal(err)
			}
			old, _ := b.Stat(context.Background(), "conflict.txt")
			if _, err := b.Put(context.Background(), "conflict.txt", strings.NewReader("remote-v2"), 9, storage.Condition{IfMatch: old.ETag}); err != nil {
				t.Fatal(err)
			}
			p, err := s.Preview(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a, ok := actionOf(p, "conflict.txt"); !ok || a.Kind != "conflict" {
				t.Fatalf("expected conflict, got %+v", a)
			}
			if _, err = s.ResolveConflict(context.Background(), j.ID, "conflict.txt", choice); err != nil {
				t.Fatal(err)
			}
			switch choice {
			case "local":
				if localText(t, dir, "conflict.txt") != "local-v2" || readBody(t, b, "conflict.txt") != "local-v2" {
					t.Fatal("local choice did not converge on local version")
				}
			case "remote":
				if localText(t, dir, "conflict.txt") != "remote-v2" || readBody(t, b, "conflict.txt") != "remote-v2" {
					t.Fatal("remote choice did not converge on remote version")
				}
			case "keep-both":
				if localText(t, dir, "conflict.txt") != "local-v2" || readBody(t, b, "conflict.txt") != "local-v2" {
					t.Fatal("keep-both failed to preserve local canonical version")
				}
				matches, _ := filepath.Glob(filepath.Join(dir, "*conflict remote*"))
				if len(matches) != 1 {
					t.Fatalf("remote conflict copy missing: %v", matches)
				}
				raw, e := os.ReadFile(matches[0])
				if e != nil || string(raw) != "remote-v2" {
					t.Fatalf("remote conflict copy %q %v", raw, e)
				}
			}
		})
	}
}

func TestSyncDeletionRulesAndMirrorConfirmation(t *testing.T) {
	t.Run("both-way-baseline-delete", func(t *testing.T) {
		s, id := testService(t)
		setSyncKeepRecovery(t, s, id, true)
		dir := t.TempDir()
		j := makeBaseline(t, s, id, dir, "both", "gone.txt")
		b := Backend{s, id}
		if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
			t.Fatal(err)
		}
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "gone.txt"); a.Kind != "delete-remote" {
			t.Fatalf("expected propagated remote delete, got %+v", a)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if _, err = b.Stat(context.Background(), "gone.txt"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("remote delete did not apply: %v", err)
		}
		entries, err := s.recoveries()
		if err != nil {
			t.Fatal(err)
		}
		var saved *RecoveryEntry
		for i := range entries {
			if entries[i].ConnectionID == id && entries[i].Path == "gone.txt" {
				saved = &entries[i]
				break
			}
		}
		if saved == nil || saved.Integrity != "sha256" {
			t.Fatalf("remote deletion did not preserve a verified recovery copy: %+v", entries)
		}
		preview, err := s.PreviewRecovery(context.Background(), saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		if preview.CurrentExists {
			t.Fatal("deleted remote path unexpectedly exists in recovery preview")
		}
		if _, err = s.RestoreRecovery(context.Background(), saved.ID, ""); err != nil {
			t.Fatal(err)
		}
		if got := readBody(t, b, "gone.txt"); got != "base" {
			t.Fatalf("remote recovery restored %q", got)
		}
	})
	t.Run("both-way-remote-delete", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		j := makeBaseline(t, s, id, dir, "both", "remote-gone.txt")
		b := Backend{s, id}
		e, _ := b.Stat(context.Background(), "remote-gone.txt")
		if err := s.stores[id].Delete(context.Background(), "remote-gone.txt", storage.Condition{IfMatch: e.ETag}); err != nil {
			t.Fatal(err)
		}
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "remote-gone.txt"); a.Kind != "delete-local" {
			t.Fatalf("expected propagated local delete, got %+v", a)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(dir, "remote-gone.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local delete did not apply: %v", err)
		}
	})
	t.Run("upload-does-not-propagate-delete", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		j := makeBaseline(t, s, id, dir, "upload", "keep-remote.txt")
		b := Backend{s, id}
		if err := os.Remove(filepath.Join(dir, "keep-remote.txt")); err != nil {
			t.Fatal(err)
		}
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "keep-remote.txt"); a.Kind == "delete-remote" {
			t.Fatal("upload mode propagated local deletion")
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if got := readBody(t, b, "keep-remote.txt"); got != "base" {
			t.Fatalf("remote file was deleted: %q", got)
		}
	})
	t.Run("download-does-not-propagate-delete", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		j := makeBaseline(t, s, id, dir, "download", "keep-local.txt")
		b := Backend{s, id}
		e, _ := b.Stat(context.Background(), "keep-local.txt")
		if err := s.stores[id].Delete(context.Background(), "keep-local.txt", storage.Condition{IfMatch: e.ETag}); err != nil {
			t.Fatal(err)
		}
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "keep-local.txt"); a.Kind == "delete-local" {
			t.Fatal("download mode propagated remote deletion")
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if got := localText(t, dir, "keep-local.txt"); got != "base" {
			t.Fatalf("local file was deleted: %q", got)
		}
	})
	t.Run("mirror-bulk-delete-requires-confirmation", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		b := Backend{s, id}
		if err := b.Mkdir(context.Background(), "extra"); err != nil {
			t.Fatal(err)
		}
		seedRemote(t, b, "extra/a.txt", "a")
		seedRemote(t, b, "extra/b.txt", "b")
		j := syncJob(t, s, id, dir, "mirror-upload", Job{RemotePath: "extra", DeleteThreshold: 2})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p.DeleteCount != 2 || !p.RequiresDeleteConfirmation || len(p.DeletePaths) != 2 {
			t.Fatalf("missing delete preview: %+v", p)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err == nil {
			t.Fatal("bulk mirror delete ran without explicit confirmation")
		}
		for _, key := range []string{"extra/a.txt", "extra/b.txt"} {
			if _, err = b.Stat(context.Background(), key); err != nil {
				t.Fatalf("unconfirmed delete changed %s: %v", key, err)
			}
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token, true); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"extra/a.txt", "extra/b.txt"} {
			if _, err = b.Stat(context.Background(), key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("confirmed mirror delete left %s: %v", key, err)
			}
		}
	})
	t.Run("mirror-download-removes-local-extras", func(t *testing.T) {
		s, id := testService(t)
		setSyncKeepRecovery(t, s, id, true)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("extra"), 0600); err != nil {
			t.Fatal(err)
		}
		j := syncJob(t, s, id, dir, "mirror-download", Job{DeleteThreshold: 10})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "extra.txt"); a.Kind != "delete-local" {
			t.Fatalf("mirror download did not plan local deletion: %+v", a)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(dir, "extra.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mirror download left extra: %v", err)
		}
		if len(mustGlob(t, filepath.Join(s.dir, "recovery", "*.data"))) == 0 {
			t.Fatal("mirror deletion did not preserve recoverable data")
		}
	})
}

func TestExcludeRulesDoNotCreateDeleteActions(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	b := Backend{s, id}
	if err := b.Mkdir(context.Background(), "sync"); err != nil {
		t.Fatal(err)
	}
	if err := b.Mkdir(context.Background(), "sync/cache"); err != nil {
		t.Fatal(err)
	}
	if err := b.Mkdir(context.Background(), "sync/cache/sub"); err != nil {
		t.Fatal(err)
	}
	if err := b.Mkdir(context.Background(), "sync/notes"); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, "sync/cache/sub/a.tmp", "one")
	seedRemote(t, b, "sync/notes/drop.tmp", "two")
	j := syncJob(t, s, id, dir, "mirror-upload", Job{RemotePath: "sync", Exclude: []string{"cache/**", "*.tmp"}})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.DeleteCount != 0 {
		t.Fatalf("excluded paths became deletions: %+v", p.DeletePaths)
	}
	for _, a := range p.Actions {
		if a.Kind == "delete-remote" {
			t.Fatalf("excluded path scheduled for deletion: %+v", a)
		}
	}
}

func TestSyncAlwaysProtectsManagedBackupPathsAndRetiresOldState(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	for _, key := range []string{".tamiops-backup/local.txt", "nested/.tamiops-backup/local.txt"} {
		full := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("preserve local"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := Backend{s, id}
	st, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot := "sync-test-root"
	if err := b.Mkdir(context.Background(), remoteRoot); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{remoteRoot + "/.tamiops-backup/remote.txt", remoteRoot + "/nested/.tamiops-backup/remote.txt", remoteRoot + "/ordinary-extra.txt"} {
		if isManagedPath(key) {
			if err := st.Mkdir(context.Background(), path.Dir(key)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Put(context.Background(), key, strings.NewReader("preserve remote"), int64(len("preserve remote")), storage.Condition{IfNoneMatch: true}); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := ensureRemoteParents(context.Background(), b, key); err != nil {
				t.Fatal(err)
			}
			seedRemote(t, b, key, "preserve remote")
		}
	}
	j := syncJob(t, s, id, dir, "mirror-upload", Job{RemotePath: remoteRoot, DeleteThreshold: 10})
	if err := s.ensureSyncSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?)", j.ID, "old/.tamiops-backup/gone.txt", "stale", "stale"); err != nil {
		t.Fatal(err)
	}
	if err := s.setQueue(j.ID, Action{Path: "old/.tamiops-backup/gone.txt", Kind: "delete-remote"}, "pending", nil); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Actions {
		if isManagedPath(a.Path) {
			t.Fatalf("managed backup path entered the sync plan: %+v", a)
		}
	}
	if p.DeleteCount != 1 || len(p.DeletePaths) != 1 || p.DeletePaths[0] != "ordinary-extra.txt" {
		t.Fatalf("mirror plan did not limit deletion to the ordinary extra file: %+v", p.DeletePaths)
	}
	var baselines, queue int
	if err = s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=? AND path=?", j.ID, "old/.tamiops-backup/gone.txt").Scan(&baselines); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM sync_queue WHERE job=? AND path=?", j.ID, "old/.tamiops-backup/gone.txt").Scan(&queue); err != nil {
		t.Fatal(err)
	}
	if baselines != 0 || queue != 0 {
		t.Fatalf("old managed baseline/queue was not retired: %d/%d", baselines, queue)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{remoteRoot + "/.tamiops-backup/remote.txt", remoteRoot + "/nested/.tamiops-backup/remote.txt"} {
		if got := readBody(t, b, key); got != "preserve remote" {
			t.Fatalf("managed remote backup changed at %s: %q", key, got)
		}
	}
	if got := localText(t, dir, ".tamiops-backup/local.txt"); got != "preserve local" {
		t.Fatalf("managed local backup changed: %q", got)
	}
	stale := Plan{Token: ID(), JobID: j.ID, ConfigFingerprint: syncPlanConfigFingerprint(j), Created: time.Now(), Actions: []Action{{Path: ".tamiops-backup/remote.txt", Kind: "delete-remote"}}}
	raw, err := marshalPlan(stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?) ON CONFLICT(job) DO UPDATE SET token=excluded.token,data=excluded.data,created=excluded.created", j.ID, stale.Token, string(raw), stale.Created.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, stale.Token); err == nil || !strings.Contains(err.Error(), "备份任务管理") {
		t.Fatalf("stale plan modified a managed backup path: %v", err)
	}
}

func TestSyncRejectsManagedDirectoryAsEitherTaskRoot(t *testing.T) {
	s, id := testService(t)
	managedLocal := filepath.Join(t.TempDir(), ".tamiops-backup", "nested")
	if err := os.MkdirAll(managedLocal, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddJob(Job{Name: "managed local", ConnectionID: id, LocalPath: managedLocal, Direction: "upload"}); err == nil {
		t.Fatal("sync accepted a local root inside the managed backup directory")
	}
	if _, err := s.AddJob(Job{Name: "managed remote", ConnectionID: id, LocalPath: t.TempDir(), RemotePath: "nested/.tamiops-backup", Direction: "upload"}); err == nil {
		t.Fatal("sync accepted a remote root inside the managed backup directory")
	}
}

func TestSymlinkProtectionRetiresBaselineBeforeLinkRemoval(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "both", "link/file.txt")
	if err := os.RemoveAll(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionOf(p, "link/file.txt"); ok {
		t.Fatalf("remote path under a symlink was planned during protection: %+v", a)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=? AND path=?", j.ID, "link/file.txt").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("baseline under the active symlink was not retired")
	}
	if err = os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	p, err = s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionOf(p, "link/file.txt"); !ok || a.Kind != "download" {
		t.Fatalf("retired baseline still authorized a remote deletion: %+v", a)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	if got := localText(t, dir, "link/file.txt"); got != "base" {
		t.Fatalf("remote file was not safely re-established after link removal: %q", got)
	}
}

func TestSyncCreatesEmptyDirectoriesAndRemovesOnlyVerifiedEmptyLocalDirs(t *testing.T) {
	t.Run("both-directions-create-empty-dirs", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "local-empty"), 0700); err != nil {
			t.Fatal(err)
		}
		b := Backend{s, id}
		if err := b.Mkdir(context.Background(), "remote-empty"); err != nil {
			t.Fatal(err)
		}
		j := syncJob(t, s, id, dir, "both", Job{})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "local-empty"); a.Kind != "mkdir-remote" {
			t.Fatalf("local empty directory was not planned remotely: %+v", a)
		}
		if a, _ := actionOf(p, "remote-empty"); a.Kind != "mkdir-local" {
			t.Fatalf("remote empty directory was not planned locally: %+v", a)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if e, err := b.Stat(context.Background(), "local-empty"); err != nil || !e.IsDir {
			t.Fatalf("remote empty directory was not created: %+v %v", e, err)
		}
		if info, err := os.Stat(filepath.Join(dir, "remote-empty")); err != nil || !info.IsDir() {
			t.Fatalf("local empty directory was not created: %v %v", info, err)
		}
	})
	t.Run("local-directory-delete-requires-empty-read", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "empty"), 0700); err != nil {
			t.Fatal(err)
		}
		kept := filepath.Join(dir, "has-excluded-child")
		if err := os.Mkdir(kept, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(kept, "keep.tmp"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		j := syncJob(t, s, id, dir, "mirror-download", Job{Exclude: []string{"*.tmp"}, DeleteThreshold: 10})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "empty"); a.Kind != "delete-local-dir" {
			t.Fatalf("verified empty local directory was not planned for removal: %+v", a)
		}
		if a, _ := actionOf(p, "has-excluded-child"); a.Kind == "delete-local-dir" {
			t.Fatalf("directory with an excluded child was treated as empty: %+v", a)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(dir, "empty")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("verified empty local directory remained: %v", err)
		}
		if got := localText(t, dir, "has-excluded-child/keep.tmp"); got != "keep" {
			t.Fatalf("excluded child was lost with its parent: %q", got)
		}
	})
	t.Run("remote-directory-delete-is-never-inferred", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		b := Backend{s, id}
		if err := b.Mkdir(context.Background(), "target-only-empty"); err != nil {
			t.Fatal(err)
		}
		j := syncJob(t, s, id, dir, "mirror-upload", Job{})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "target-only-empty"); a.Kind == "delete-remote" {
			t.Fatalf("remote directory was planned for deletion: %+v", a)
		}
		if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
			t.Fatal(err)
		}
		if e, err := b.Stat(context.Background(), "target-only-empty"); err != nil || !e.IsDir {
			t.Fatalf("remote directory was not preserved: %+v %v", e, err)
		}
	})
}

func TestCrossPlatformNamesAndCaseCollisions(t *testing.T) {
	t.Run("windows-illegal", func(t *testing.T) {
		if err := validSyncPath("bad?.txt"); !errors.Is(err, storage.ErrInvalidPath) {
			t.Fatalf("illegal path was accepted: %v", err)
		}
		if runtime.GOOS == "windows" {
			t.Skip("Windows cannot create the illegal-name filesystem fixture")
		}
		s, id := testService(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bad?.txt"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		j := syncJob(t, s, id, dir, "upload", Job{})
		if _, err := s.Preview(context.Background(), j.ID); !errors.Is(err, storage.ErrInvalidPath) {
			t.Fatalf("illegal cross-platform name accepted: %v", err)
		}
	})
	t.Run("invalid-utf8", func(t *testing.T) {
		if err := validSyncPath(string([]byte{0xff})); !errors.Is(err, storage.ErrInvalidPath) {
			t.Fatalf("invalid UTF-8 path was accepted: %v", err)
		}
		if runtime.GOOS == "windows" {
			t.Skip("Windows converts filenames to UTF-16; invalid UTF-8 cannot round-trip")
		}
		s, id := testService(t)
		dir := t.TempDir()
		name := string([]byte{0xff})
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Skipf("filesystem rejects invalid UTF-8 names: %v", err)
		}
		j := syncJob(t, s, id, dir, "upload", Job{})
		if _, err := s.Preview(context.Background(), j.ID); !errors.Is(err, storage.ErrInvalidPath) {
			t.Fatalf("invalid Unicode filename accepted: %v", err)
		}
	})
	t.Run("case-fold-collision", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		b := Backend{s, id}
		if err := os.WriteFile(filepath.Join(dir, "Readme.txt"), []byte("local"), 0600); err != nil {
			t.Fatal(err)
		}
		seedRemote(t, b, "README.TXT", "remote")
		j := syncJob(t, s, id, dir, "both", Job{})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, _ := actionOf(p, "README.TXT"); a.Kind != "conflict" {
			found := false
			for _, x := range p.Actions {
				if x.Kind == "conflict" {
					found = true
				}
			}
			if !found {
				t.Fatalf("case collision was not reported: %+v", p.Actions)
			}
		}
	})
	t.Run("unicode-normalization-collision", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		b := Backend{s, id}
		if err := os.WriteFile(filepath.Join(dir, "café.txt"), []byte("local"), 0600); err != nil {
			t.Fatal(err)
		}
		seedRemote(t, b, "cafe\u0301.txt", "remote")
		j := syncJob(t, s, id, dir, "both", Job{})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, x := range p.Actions {
			if x.Kind == "conflict" {
				found = true
			}
		}
		if !found {
			t.Fatalf("NFC/NFD collision was not reported: %+v", p.Actions)
		}
	})
	t.Run("file-directory-collision", func(t *testing.T) {
		s, id := testService(t)
		dir := t.TempDir()
		b := Backend{s, id}
		if err := os.WriteFile(filepath.Join(dir, "tree"), []byte("local"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := b.Mkdir(context.Background(), "tree"); err != nil {
			t.Fatal(err)
		}
		seedRemote(t, b, "tree/child.txt", "remote")
		j := syncJob(t, s, id, dir, "both", Job{})
		p, err := s.Preview(context.Background(), j.ID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, x := range p.Actions {
			if x.Kind == "conflict" {
				found = true
			}
		}
		if !found {
			t.Fatalf("file-directory collision was not reported: %+v", p.Actions)
		}
		for _, a := range p.Actions {
			if strings.HasPrefix(a.Path, "tree/") {
				t.Fatalf("descendant action crossed a file-directory conflict: %+v", a)
			}
		}
	})
}

type corruptPutStore struct{ storage.Store }

func (s corruptPutStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition storage.Condition) (storage.Entry, error) {
	if size < 0 || size > 1<<20 {
		return storage.Entry{}, errors.New("test size")
	}
	return s.Store.Put(ctx, key, strings.NewReader(strings.Repeat("x", int(size))), size, condition)
}

func TestUploadMustVerifyActualRemoteContentBeforeBaseline(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "verify.txt"), []byte("planned"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stores[id] = corruptPutStore{s.stores[id]}
	s.mu.Unlock()
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err == nil {
		t.Fatal("incorrect remote content was accepted")
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=? AND path=?", j.ID, "verify.txt").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed content verification advanced the baseline")
	}
}

type blockingPutStore struct {
	storage.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingPutStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, condition storage.Condition) (storage.Entry, error) {
	s.once.Do(func() { close(s.started); <-s.release })
	return s.Store.Put(ctx, key, body, size, condition)
}

func TestSyncPlanCannotRunTwiceConcurrently(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "once.txt"), []byte("once"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingPutStore{Store: s.stores[id], started: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	s.stores[id] = blocking
	s.mu.Unlock()
	type result struct{ err error }
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() { _, e := s.RunPlan(context.Background(), j.ID, p.Token); first <- result{e} }()
	select {
	case <-blocking.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first run never reached remote write")
	}
	go func() { _, e := s.RunPlan(context.Background(), j.ID, p.Token); second <- result{e} }()
	time.Sleep(30 * time.Millisecond)
	close(blocking.release)
	if r := <-first; r.err != nil {
		t.Fatalf("first execution failed: %v", r.err)
	}
	if r := <-second; r.err == nil {
		t.Fatal("same plan token executed twice")
	}
	if got := readBody(t, Backend{s, id}, "once.txt"); got != "once" {
		t.Fatalf("wrong final content %q", got)
	}
}

func TestSchedulerPollsAndDebouncesChanges(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	_ = syncJob(t, s, id, dir, "upload", Job{Watch: true, DeleteThreshold: 10})
	s.StartScheduler()
	s.StartScheduler()
	defer s.StopScheduler()
	defer s.StopScheduler()
	if err := os.WriteFile(filepath.Join(dir, "watched.txt"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := readRemoteMaybe(b, "watched.txt"); err == nil && got == "ready" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("polling watcher did not sync a stable local change")
}

func readRemoteMaybe(b Backend, key string) (string, error) {
	r, _, err := b.Open(context.Background(), key, "")
	if err != nil {
		return "", err
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	return string(raw), err
}

func mustGlob(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestUpdateJobClearsBaselineWhenScopeChanges(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "both", "scope.txt")
	changed := j
	changed.RemotePath = "other"
	updated, err := s.UpdateJob(changed)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "idle" {
		t.Fatalf("scope change did not require preview: %s", updated.Status)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=?", j.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("baseline from old scope remained")
	}
}

func TestRunPlanRejectsPersistedPlanAfterJobScopeChanges(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "victim.txt"), []byte("local version"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "mirror-upload", Job{RemotePath: "old-scope"})
	b := Backend{Service: s, ConnectionID: id}
	if err := ensureRemoteDirectory(context.Background(), b, "old-scope"); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "victim.txt")); err != nil {
		t.Fatal(err)
	}
	p, err = s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.DeletePaths) != 1 || p.DeletePaths[0] != "victim.txt" {
		t.Fatalf("expected a remote deletion preview, got %+v", p.DeletePaths)
	}

	if err = ensureRemoteDirectory(context.Background(), b, "new-scope"); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, "new-scope/victim.txt", "new scope valuable data")
	// Simulate a stale process/config writer and force RunPlan to load the
	// persisted token rather than the in-memory copy.
	s.mu.Lock()
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID == j.ID {
			s.cfg.Jobs[i].RemotePath = "new-scope"
		}
	}
	for token, plan := range s.plans {
		if plan.JobID == j.ID {
			delete(s.plans, token)
		}
	}
	s.mu.Unlock()
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err == nil || !strings.Contains(err.Error(), "配置已更改") {
		t.Fatalf("stale persisted plan was accepted after scope change: %v", err)
	}
	if got := readBody(t, b, "new-scope/victim.txt"); got != "new scope valuable data" {
		t.Fatalf("stale mirror deletion removed data from the new scope: %q", got)
	}
}

func TestUpdateJobSharesRunPlanJobLock(t *testing.T) {
	s, id := testService(t)
	j := syncJob(t, s, id, t.TempDir(), "upload", Job{})
	lock := s.syncJobLock(j.ID)
	lock.Lock()
	done := make(chan error, 1)
	changed := j
	changed.RemotePath = "new-scope"
	go func() {
		_, err := s.UpdateJob(changed)
		done <- err
	}()
	select {
	case err := <-done:
		lock.Unlock()
		t.Fatalf("UpdateJob bypassed the job execution lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateJob did not continue after the job lock was released")
	}
}

func TestSyncDeleteConflictOnChangedRemote(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "both", "changed.txt")
	b := Backend{s, id}
	if err := os.Remove(filepath.Join(dir, "changed.txt")); err != nil {
		t.Fatal(err)
	}
	e, _ := b.Stat(context.Background(), "changed.txt")
	if _, err := b.Put(context.Background(), "changed.txt", strings.NewReader("changed remote"), 14, storage.Condition{IfMatch: e.ETag}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := actionOf(p, "changed.txt"); a.Kind != "conflict" {
		t.Fatalf("deletion raced a remote change without conflict: %+v", a)
	}
}

func TestSchedulerLifecycleIsIdempotent(t *testing.T) {
	s, _ := testService(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.StartScheduler() }()
	}
	wg.Wait()
	s.StopScheduler()
	s.StopScheduler()
	s.mu.Lock()
	running := s.schedulerCancel != nil
	s.mu.Unlock()
	if running {
		t.Fatal("scheduler remained active after stop")
	}
	s.StartScheduler()
	s.StopScheduler()
}

func TestRetryUsesFreshPlanAndProgressCompletes(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "retry.txt"), []byte("retry"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	out, err := s.RetryJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "synced" || out.Progress != 100 || out.QueueDone != out.QueueTotal {
		t.Fatalf("job progress/status not finalized: %+v", out)
	}
}

func TestSyncPlanCanBeRecoveredFromPersistentStore(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "persist.txt"), []byte("persist"), 0600); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, dir, "upload", Job{})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.plans = map[string]Plan{}
	s.mu.Unlock()
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatalf("saved preview did not survive in-memory plan loss: %v", err)
	}
}

func TestSyncPlanStaleLocalNewFileNeverOverwrites(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	seedRemote(t, Backend{s, id}, "new.txt", "remote")
	j := syncJob(t, s, id, dir, "download", Job{})
	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "new.txt"), []byte("created-after-preview"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale download did not conflict: %v", err)
	}
	if got := localText(t, dir, "new.txt"); got != "created-after-preview" {
		t.Fatalf("stale download overwrote a new file: %q", got)
	}
}

func TestScheduleDueCalculation(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		last string
		mins int
		want bool
	}{{"", 10, true}, {now.Add(-11 * time.Minute).Format(time.RFC3339), 10, true}, {now.Add(-9 * time.Minute).Format(time.RFC3339), 10, false}, {"bad", 10, true}, {"", 0, false}} {
		if got := scheduleIsDue(tc.last, tc.mins, now); got != tc.want {
			t.Fatalf("scheduleIsDue(%q,%d)=%v want %v", tc.last, tc.mins, got, tc.want)
		}
	}
}
