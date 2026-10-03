package core

import (
	"context"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"tamiops/internal/storage"
)

// Editors that allow rename while open opt into FILE_SHARE_DELETE on Windows.
// Exercise the same retained-file behavior as Unix without assuming that all
// Windows editors allow it. The exclusive case is tested separately below.
func openCacheEditor(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func TestCacheRefreshPreservesWindowsLockedEditor(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	remote, err := b.Put(ctx, "editing", strings.NewReader("before"), 6, storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := s.FetchCache(ctx, id, "editing", true, false)
	if err != nil {
		t.Fatal(err)
	}
	editor, err := os.OpenFile(cached.LocalPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer editor.Close()
	if _, err = b.Put(ctx, "editing", strings.NewReader("remote"), 6, storage.Condition{IfMatch: remote.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.fetchCache(ctx, id, "editing", true, false, true); err == nil {
		t.Fatal("refresh replaced a file held by an editor that disallows rename")
	}
	if got, err := os.ReadFile(cached.LocalPath); err != nil || string(got) != "before" {
		t.Fatalf("locked cache was changed: %q, %v", got, err)
	}
	if _, err = editor.WriteAt([]byte("edited later"), 0); err != nil {
		t.Fatal(err)
	}
	if err = editor.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = editor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.fetchCache(ctx, id, "editing", true, false, true); err == nil {
		t.Fatal("refresh overwrote editor changes after the handle closed")
	}
	if got, err := os.ReadFile(cached.LocalPath); err != nil || string(got) != "edited later" {
		t.Fatalf("editor changes were lost: %q, %v", got, err)
	}
}
