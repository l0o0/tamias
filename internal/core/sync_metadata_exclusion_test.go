package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tamiops/internal/storage"
)

func TestSyncScansExcludeDSStoreAtEveryDepthAndKeepOtherDotfiles(t *testing.T) {
	s, id := testService(t)
	localDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(localDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		".DS_Store":          "local metadata",
		"nested/.DS_Store":   "nested local metadata",
		".hidden":            "local dotfile",
		"nested/.hidden":     "nested local dotfile",
		"nested/document.md": "document",
	} {
		if err := os.WriteFile(filepath.Join(localDir, filepath.FromSlash(name)), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(localDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	local, _, err := scanLocalFiltered(context.Background(), root, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{".hidden", "nested/.hidden", "nested/document.md"} {
		if _, ok := local[key]; !ok {
			t.Errorf("ordinary local path %q was omitted", key)
		}
	}
	for _, key := range []string{".DS_Store", "nested/.DS_Store"} {
		if _, ok := local[key]; ok {
			t.Errorf("metadata path %q was included in local scan", key)
		}
	}

	b := Backend{Service: s, ConnectionID: id}
	for key, content := range map[string]string{
		".DS_Store":           "remote metadata",
		"nested/.DS_Store":    "nested remote metadata",
		".remote-hidden":      "remote dotfile",
		"nested/.remote-file": "nested remote dotfile",
	} {
		if err := ensureRemoteParents(context.Background(), b, key); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Put(context.Background(), key, strings.NewReader(content), int64(len(content)), storage.Condition{IfNoneMatch: true}); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	remote, _, err := scanRemoteFiltered(context.Background(), st, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{".remote-hidden", "nested/.remote-file"} {
		if _, ok := remote[key]; !ok {
			t.Errorf("ordinary remote path %q was omitted", key)
		}
	}
	for _, key := range []string{".DS_Store", "nested/.DS_Store"} {
		if _, ok := remote[key]; ok {
			t.Errorf("metadata path %q was included in remote scan", key)
		}
	}
}

func TestSyncMetadataBaselineAndQueueAreRetiredWithoutDeleteActions(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("same"), 0600); err != nil {
		t.Fatal(err)
	}
	b := Backend{Service: s, ConnectionID: id}
	seedRemote(t, b, ".DS_Store", "metadata")
	if err := ensureRemoteParents(context.Background(), b, "nested/.DS_Store"); err != nil {
		t.Fatal(err)
	}
	seedRemote(t, b, "nested/.DS_Store", "nested metadata")
	seedRemote(t, b, "notes.txt", "same")
	j := syncJob(t, s, id, dir, "mirror-upload", Job{})
	if err := s.ensureSyncSchema(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{".DS_Store", "nested/.DS_Store"} {
		if err := s.saveBaseline(j.ID, key, "stale-local-hash", "stale-etag"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO sync_queue(job,path,kind,state,updated) VALUES(?,?,?,'pending',?)`, j.ID, key, "delete-remote", time.Now().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.Actions {
		if isSyncMetadataPath(action.Path) && action.Kind != "skip" {
			t.Fatalf("metadata path produced a sync action: %+v", action)
		}
	}
	for _, key := range []string{".DS_Store", "nested/.DS_Store"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM baseline WHERE job=? AND path=?", j.ID, key).Scan(&count); err != nil || count != 0 {
			t.Fatalf("metadata baseline was not retired for %q: count=%d err=%v", key, count, err)
		}
		if err := s.db.QueryRow("SELECT count(*) FROM sync_queue WHERE job=? AND path=?", j.ID, key).Scan(&count); err != nil || count != 0 {
			t.Fatalf("metadata queue row was not retired for %q: count=%d err=%v", key, count, err)
		}
		if _, err := b.Stat(context.Background(), key); err != nil {
			t.Fatalf("remote metadata was altered for %q: %v", key, err)
		}
	}
}

func TestLegacySyncPlanCannotExecuteAfterMetadataExclusionChange(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := syncJob(t, s, id, dir, "mirror-upload", Job{})
	b := Backend{Service: s, ConnectionID: id}
	e := seedRemote(t, b, "keep.txt", "remote data")
	if err := s.ensureSyncSchema(); err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Token:             ID(),
		JobID:             j.ID,
		ConfigFingerprint: legacySyncPlanConfigFingerprint(j),
		Created:           time.Now(),
		Actions:           []Action{{Path: "keep.txt", Kind: "delete-remote", RemoteETag: e.ETag}},
		DeletePaths:       []string{"keep.txt"},
		DeleteCount:       1,
	}
	raw, err := marshalPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sync_plans(job,token,data,created) VALUES(?,?,?,?)`, j.ID, plan.Token, string(raw), plan.Created.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunPlan(context.Background(), j.ID, plan.Token, true); err == nil {
		t.Fatal("legacy plan was accepted after sync metadata behavior changed")
	}
	if _, err := b.Stat(context.Background(), "keep.txt"); err != nil {
		t.Fatalf("rejected legacy plan performed its delete action: %v", err)
	}
}

func legacySyncPlanConfigFingerprint(j Job) string {
	config := struct {
		ConnectionID    string
		RemotePath      string
		LocalPath       string
		Direction       string
		Exclude         []string
		DeleteThreshold int
	}{j.ConnectionID, j.RemotePath, filepath.Clean(j.LocalPath), j.Direction, append([]string(nil), j.Exclude...), j.DeleteThreshold}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestSyncPathExcludedOnlyIgnoresExactDSStoreBasename(t *testing.T) {
	for _, key := range []string{".DS_Store", "folder/.DS_Store", "folder\\nested\\.DS_Store"} {
		if !syncPathExcluded(nil, key) {
			t.Errorf("expected metadata path %q to be excluded", key)
		}
	}
	for _, key := range []string{".hidden", "folder/.DS_Store.txt", "folder/ds_store", "folder/.DS_Store/child.txt"} {
		if syncPathExcluded(nil, key) {
			t.Errorf("ordinary path %q was excluded", key)
		}
	}
	if !syncPathExcluded([]string{"*.hidden"}, "folder/.hidden") {
		t.Fatal("configured user exclusions stopped working")
	}
}
