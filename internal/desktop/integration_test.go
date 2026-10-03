package desktop

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

func TestExternalEditorDoesNotInheritAppImageLoader(t *testing.T) {
	t.Setenv("TAMIOPS_WEBKIT_SOURCE_EXEC_DIR", "/usr/lib/webkitgtk-6.0")
	t.Setenv("APPDIR", "/tmp/appimage")
	t.Setenv("APPIMAGE", "/home/user/tamiops.AppImage")
	t.Setenv("LD_LIBRARY_PATH", "/tmp/appimage/usr/lib")
	t.Setenv("LD_PRELOAD", "/tmp/appimage/usr/lib/path-shim.so")
	t.Setenv("WEBKIT_EXEC_PATH", "/tmp/appimage/usr/lib/webkitgtk-6.0")
	t.Setenv("WEBKIT_INJECTED_BUNDLE_PATH", "/tmp/appimage/usr/lib/injected-bundle")
	t.Setenv("TAMIOPS_ORIGINAL_LD_LIBRARY_PATH", "/opt/user/lib")
	t.Setenv("TAMIOPS_ORIGINAL_LD_PRELOAD", "")
	t.Setenv("DISPLAY", ":42")
	values := make(map[string]string)
	for _, entry := range externalEnvironment() {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["LD_LIBRARY_PATH"] != "/opt/user/lib" || values["DISPLAY"] != os.Getenv("DISPLAY") {
		t.Fatal("external editor lost the caller's library or display settings")
	}
	for _, key := range []string{"LD_PRELOAD", "APPDIR", "APPIMAGE", "WEBKIT_EXEC_PATH", "WEBKIT_INJECTED_BUNDLE_PATH", "TAMIOPS_WEBKIT_SOURCE_EXEC_DIR"} {
		if _, exists := values[key]; exists {
			t.Fatalf("external editor inherited AppImage setting %s", key)
		}
	}
}

func TestAutoStartContentCarriesDataDirectory(t *testing.T) {
	executable := "/Applications/tamiops.app/Contents/MacOS/tamiops"
	dataDir := "/Users/李明/Library/Application Support/Tami & Data"

	macContent, err := renderAutoStartContent("darwin", executable, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var plist struct {
		Dict struct {
			Arguments []string `xml:"array>string"`
		} `xml:"dict"`
	}
	if err = xml.Unmarshal([]byte(macContent), &plist); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(macContent, "<string>io.tamiops.tami</string>") {
		t.Fatalf("LaunchAgent bundle identity changed: %s", macContent)
	}
	want := []string{executable, "--data-dir", dataDir}
	if len(plist.Dict.Arguments) != len(want) {
		t.Fatalf("LaunchAgent arguments=%q", plist.Dict.Arguments)
	}
	for i := range want {
		if plist.Dict.Arguments[i] != want[i] {
			t.Fatalf("LaunchAgent arguments=%q, want %q", plist.Dict.Arguments, want)
		}
	}

	linuxContent, err := renderAutoStartContent("linux", executable, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(linuxContent, "\nName=tamiops\n") {
		t.Fatalf("desktop entry did not show the product name: %s", linuxContent)
	}
	wantExec := "Exec=\"/Applications/tamiops.app/Contents/MacOS/tamiops\" --data-dir=\"/Users/李明/Library/Application Support/Tami & Data\""
	if !strings.Contains(linuxContent, wantExec) {
		t.Fatalf("desktop entry did not quote executable and data dir: %s", linuxContent)
	}

	windowsContent, err := renderAutoStartContent("windows", `C:\Program Files\Tami\Tami.exe`, `C:\Users\李明\App Data`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `start "" "C:\Program Files\Tami\Tami.exe" --data-dir="C:\Users\李明\App Data"`; !strings.Contains(windowsContent, want) {
		t.Fatalf("startup command omitted or misquoted data dir: %s", windowsContent)
	}
}

func TestAutoStartContentEscapesAndRejectsInjectionCharacters(t *testing.T) {
	linuxContent, err := renderAutoStartContent("linux", "/opt/Tami/app$(touch bad).sh", `/home/李明/$HOME/100%/`+"quote'&`test`")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`app\$(touch bad).sh`, `\$HOME`, "100%%", "\\`test\\`"} {
		if !strings.Contains(linuxContent, fragment) {
			t.Fatalf("desktop entry is missing escape %q: %s", fragment, linuxContent)
		}
	}

	if _, err = renderAutoStartContent("windows", `C:\Tami\Tami.exe`, `C:\Users\x%PATH%\Tami`); err == nil {
		t.Fatal("Windows command accepted percent expansion")
	}
	if _, err = renderAutoStartContent("linux", "/opt/Tami", "/home/user/Tami\nExec=/usr/bin/evil"); err == nil {
		t.Fatal("desktop entry accepted a newline injection")
	}
}
