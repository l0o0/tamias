package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppImageAutostartUsesPersistentImage(t *testing.T) {
	config := t.TempDir()
	image := filepath.Join(t.TempDir(), "tamiops.AppImage")
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("APPDIR", "/tmp/transient-appimage")
	t.Setenv("APPIMAGE", image)
	if err := SetAutoStart(true, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	entry, err := os.ReadFile(filepath.Join(config, "autostart", "tami.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), "Exec=\""+image+"\"") {
		t.Fatalf("autostart does not target persistent AppImage: %s", entry)
	}
}
