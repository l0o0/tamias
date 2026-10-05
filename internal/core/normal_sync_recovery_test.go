package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tamiops/internal/storage"
)

func setSyncKeepRecovery(t *testing.T, s *Service, id string, keep bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID != id {
			continue
		}
		s.cfg.Connections[i].KeepRecovery = keep
		if err := s.saveLocked(); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("connection %q not found", id)
}

func recoveryFiles(t *testing.T, s *Service, extension string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(s.dir, "recovery", "*"+extension))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertNoSyncRecovery(t *testing.T, s *Service) {
	t.Helper()
	for _, extension := range []string{".data", ".json"} {
		if files := recoveryFiles(t, s, extension); len(files) != 0 {
			t.Fatalf("default sync created recovery %s files: %v", extension, files)
		}
	}
}

func TestStandardSyncUploadUpdatesExistingRemotePath(t *testing.T) {
	s, id := testService(t)
	dir := t.TempDir()
	j := makeBaseline(t, s, id, dir, "both", "note.txt")
	setSyncConnectionPolicy(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("local-v2"), 0600); err != nil {
		t.Fatal(err)
	}

	p, err := s.Preview(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := actionOf(p, "note.txt"); !ok || a.Kind != "upload" {
		t.Fatalf("local update did not plan an upload: %+v", a)
	}
	if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
		t.Fatal(err)
	}
	b := Backend{s, id}
	if got := readBody(t, b, "note.txt"); got != "local-v2" {
		t.Fatalf("updated remote content = %q", got)
	}
	entries, err := b.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	noteCount := 0
	for _, entry := range entries {
		if entry.Path == "note.txt" {
			noteCount++
		}
		if strings.Contains(entry.Path, "(副本-") {
			t.Fatalf("standard sync created a copy instead of updating the path: %+v", entries)
		}
	}
	if noteCount != 1 {
		t.Fatalf("standard sync did not keep exactly one note.txt path: %+v", entries)
	}
	assertNoSyncRecovery(t, s)
}

func TestSyncLargePriorRemoteFileRequiresRecoveryOnlyWhenEnabled(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := "disabled"
		if keep {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			s, id := testService(t)
			prefs := s.Preferences()
			prefs.MaxFileBytes = 1 << 20
			if err := s.SetPreferences(prefs); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			localPath := filepath.Join(dir, "large.txt")
			if err := os.WriteFile(localPath, []byte("base"), 0600); err != nil {
				t.Fatal(err)
			}
			b := Backend{s, id}
			prior := strings.Repeat("r", 2<<20)
			raw, err := s.store(id)
			if err != nil {
				t.Fatal(err)
			}
			remote, err := raw.Put(context.Background(), "large.txt", strings.NewReader(prior), int64(len(prior)), storage.Condition{IfNoneMatch: true})
			if err != nil {
				t.Fatal(err)
			}
			j := syncJob(t, s, id, dir, "both", Job{})
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			local, err := s.hashLocal(root, "large.txt")
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("INSERT INTO baseline(job,path,local_hash,remote_etag) VALUES(?,?,?,?)", j.ID, "large.txt", local.Hash, remote.ETag); err != nil {
				t.Fatal(err)
			}
			setSyncKeepRecovery(t, s, id, keep)
			setSyncConnectionPolicy(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
			if err = os.WriteFile(localPath, []byte("local-v2"), 0600); err != nil {
				t.Fatal(err)
			}

			p, err := s.Preview(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			a, ok := actionOf(p, "large.txt")
			if !ok {
				t.Fatal("preview omitted the changed local file")
			}
			if keep {
				if a.Kind != "conflict" {
					t.Fatalf("recovery-enabled sync did not block an oversized backup source: %+v", a)
				}
				return
			}
			if a.Kind != "upload" {
				t.Fatalf("default sync blocked a replacement because of the old remote size: %+v", a)
			}
			if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
				t.Fatal(err)
			}
			if got := readBody(t, b, "large.txt"); got != "local-v2" {
				t.Fatalf("replacement content = %q", got)
			}
		})
	}
}

func TestNormalSyncDownloadRecoveryIsOptIn(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := "disabled"
		if keep {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			s, id := testService(t)
			dir := t.TempDir()
			j := makeBaseline(t, s, id, dir, "both", "note.txt")
			b := Backend{s, id}
			old, err := b.Stat(context.Background(), "note.txt")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.Put(context.Background(), "note.txt", strings.NewReader("remote-v2"), 9, storage.Condition{IfMatch: old.ETag}); err != nil {
				t.Fatal(err)
			}
			setSyncKeepRecovery(t, s, id, keep)

			p, err := s.Preview(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a, ok := actionOf(p, "note.txt"); !ok || a.Kind != "download" {
				t.Fatalf("remote update did not plan a download: %+v", a)
			}
			if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
				t.Fatal(err)
			}
			if got := localText(t, dir, "note.txt"); got != "remote-v2" {
				t.Fatalf("downloaded content = %q", got)
			}
			files := recoveryFiles(t, s, ".data")
			if !keep && len(files) != 0 {
				t.Fatalf("default sync retained a local recovery copy: %v", files)
			}
			if !keep {
				assertNoSyncRecovery(t, s)
			} else if len(recoveryFiles(t, s, ".json")) != len(files) {
				t.Fatal("opt-in recovery metadata is missing")
			}
			if keep {
				found := false
				for _, file := range files {
					data, readErr := os.ReadFile(file)
					if readErr == nil && string(data) == "base" {
						found = true
					}
				}
				if !found {
					t.Fatalf("opt-in recovery did not retain the replaced local version: %v", files)
				}
			}
		})
	}
}

func TestNormalSyncLocalDeletionRecoveryFollowsSetting(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := "disabled"
		if keep {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			s, id := testService(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("local-only"), 0600); err != nil {
				t.Fatal(err)
			}
			setSyncKeepRecovery(t, s, id, keep)
			j := syncJob(t, s, id, dir, "mirror-download", Job{DeleteThreshold: 10})

			p, err := s.Preview(context.Background(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if a, ok := actionOf(p, "extra.txt"); !ok || a.Kind != "delete-local" {
				t.Fatalf("local-only file did not plan a deletion: %+v", a)
			}
			if _, err = s.RunPlan(context.Background(), j.ID, p.Token); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(dir, "extra.txt")); !os.IsNotExist(err) {
				t.Fatalf("local deletion left the file: %v", err)
			}
			files := recoveryFiles(t, s, ".data")
			if !keep && len(files) != 0 {
				t.Fatalf("default sync retained a deleted local file: %v", files)
			}
			if !keep {
				assertNoSyncRecovery(t, s)
			} else if len(recoveryFiles(t, s, ".json")) != len(files) {
				t.Fatal("opt-in recovery metadata is missing")
			}
			if keep {
				found := false
				for _, file := range files {
					data, readErr := os.ReadFile(file)
					if readErr == nil && string(data) == "local-only" {
						found = true
					}
				}
				if !found {
					t.Fatalf("opt-in recovery did not retain the deleted local file: %v", files)
				}
			}
		})
	}
}
