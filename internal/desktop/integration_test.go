package desktop

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

func TestExternalEditorRestoresOriginalAppImageAndGtkEnvironment(t *testing.T) {
	t.Setenv("TAMIOPS_WEBKIT_SOURCE_EXEC_DIR", "/usr/lib/webkitgtk-6.0")
	t.Setenv("APPDIR", "/tmp/appimage")
	t.Setenv("APPIMAGE", "/home/user/tamiops.AppImage")
	t.Setenv("LD_LIBRARY_PATH", "/tmp/appimage/usr/lib")
	t.Setenv("LD_PRELOAD", "/tmp/appimage/usr/lib/path-shim.so")
	t.Setenv("WEBKIT_EXEC_PATH", "/tmp/appimage/usr/lib/webkitgtk-6.0")
	t.Setenv("WEBKIT_INJECTED_BUNDLE_PATH", "/tmp/appimage/usr/lib/injected-bundle")
	t.Setenv("GTK_DATA_PREFIX", "/tmp/appimage/usr")
	t.Setenv("GTK_THEME", "Adwaita:dark")
	t.Setenv("GDK_BACKEND", "x11")
	t.Setenv("XDG_DATA_DIRS", "/tmp/appimage/usr/share:/usr/share")
	t.Setenv("GSETTINGS_SCHEMA_DIR", "/tmp/appimage/usr/share/glib-2.0/schemas")
	t.Setenv("GI_TYPELIB_PATH", "/tmp/appimage/usr/lib/girepository-1.0")
	t.Setenv("GTK_EXE_PREFIX", "/tmp/appimage/usr")
	t.Setenv("GTK_PATH", "/tmp/appimage/usr/lib/gtk-4.0")
	t.Setenv("GDK_PIXBUF_MODULE_FILE", "/tmp/appimage/usr/lib/gdk-pixbuf-2.0/loaders.cache")

	originals := map[string]struct {
		set   bool
		value string
	}{
		"APPDIR":                         {set: false},
		"APPIMAGE":                       {set: false},
		"WEBKIT_EXEC_PATH":               {set: true, value: "/home/user/custom-webkit"},
		"WEBKIT_INJECTED_BUNDLE_PATH":    {set: true, value: "/home/user/injected $(touch /tmp/nope); 'quoted'"},
		"TAMIOPS_WEBKIT_SOURCE_EXEC_DIR": {set: false},
		"LD_LIBRARY_PATH":                {set: true, value: "/opt/user lib;$HOME/$(touch /tmp/nope); 'quoted'"},
		"LD_PRELOAD":                     {set: false},
		"GTK_DATA_PREFIX":                {set: true, value: ""},
		"GTK_THEME":                      {set: true, value: "Adwaita:user-theme"},
		"GDK_BACKEND":                    {set: false},
		"XDG_DATA_DIRS":                  {set: true, value: "/opt/user/share:/usr/share"},
		"GSETTINGS_SCHEMA_DIR":           {set: false},
		"GI_TYPELIB_PATH":                {set: true, value: "/opt/user/girepository"},
		"GTK_EXE_PREFIX":                 {set: false},
		"GTK_PATH":                       {set: true, value: "/opt/user/gtk path;$(touch /tmp/nope)"},
		"GDK_PIXBUF_MODULE_FILE":         {set: false},
	}
	for key, original := range originals {
		marker := "TAMIOPS_ORIGINAL_" + key
		if original.set {
			t.Setenv(marker+"_SET", "1")
		} else {
			t.Setenv(marker+"_SET", "0")
		}
		t.Setenv(marker, original.value)
	}
	t.Setenv("DISPLAY", ":42")
	values := make(map[string]string)
	for _, entry := range externalEnvironment() {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	for key, original := range originals {
		value, exists := values[key]
		if original.set && (!exists || value != original.value) {
			t.Errorf("external editor did not restore %s: got (%q, %t), want (%q, true)", key, value, exists, original.value)
		}
		if !original.set && exists {
			t.Errorf("external editor retained %s although it was originally unset", key)
		}
	}
	if values["DISPLAY"] != os.Getenv("DISPLAY") {
		t.Fatal("external editor lost the caller's display setting")
	}
	for key := range originals {
		if _, exists := values["TAMIOPS_ORIGINAL_"+key]; exists {
			t.Errorf("external editor inherited saved private value for %s", key)
		}
		if _, exists := values["TAMIOPS_ORIGINAL_"+key+"_SET"]; exists {
			t.Errorf("external editor inherited saved private presence marker for %s", key)
		}
	}
}

func TestAutoStartContentCarriesDataDirectory(t *testing.T) {
	executable := "/Applications/tamias.app/Contents/MacOS/tamias"
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
	if !strings.Contains(linuxContent, "\nName=Tamias\n") {
		t.Fatalf("desktop entry did not show the product name: %s", linuxContent)
	}
	wantExec := "Exec=\"/Applications/tamias.app/Contents/MacOS/tamias\" --data-dir=\"/Users/李明/Library/Application Support/Tami & Data\""
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
