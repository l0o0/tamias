package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncDirectorySupportsNativeDirectoryHandles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "content")
	if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(dir); err != nil {
		t.Fatalf("sync existing directory: %v", err)
	}
	if err := syncDirectory(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing directory: %v", err)
	}
	if err := syncDirectory(file); err == nil {
		t.Fatal("regular file accepted as a directory")
	}
}
