package desktop

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Open is invoked only for a file that Core has downloaded to its private cache.
func Open(filename string) error {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("/usr/bin/open", absolute)
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", absolute)
	default:
		cmd = exec.Command("xdg-open", absolute)
		cmd.Env = externalEnvironment()
	}
	return cmd.Run()
}

// AppImage and the bundled GTK hook change paths and display settings for
// tamiops itself. Restore the caller's original values before invoking an
// external desktop application.
func externalEnvironment() []string {
	env := os.Environ()
	if os.Getenv("TAMIOPS_WEBKIT_SOURCE_EXEC_DIR") == "" {
		return env
	}

	privateKeys := []string{
		"APPDIR", "APPIMAGE", "WEBKIT_EXEC_PATH", "WEBKIT_INJECTED_BUNDLE_PATH",
		"TAMIOPS_WEBKIT_SOURCE_EXEC_DIR", "LD_LIBRARY_PATH", "LD_PRELOAD",
		"GTK_DATA_PREFIX", "GTK_THEME", "GDK_BACKEND", "XDG_DATA_DIRS",
		"GSETTINGS_SCHEMA_DIR", "GI_TYPELIB_PATH", "GTK_EXE_PREFIX", "GTK_PATH",
		"GDK_PIXBUF_MODULE_FILE",
	}
	private := make(map[string]struct{}, len(privateKeys)*3)
	for _, key := range privateKeys {
		private[key] = struct{}{}
		private["TAMIOPS_ORIGINAL_"+key] = struct{}{}
		private["TAMIOPS_ORIGINAL_"+key+"_SET"] = struct{}{}
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, drop := private[key]; drop {
			continue
		}
		result = append(result, entry)
	}
	for _, key := range privateKeys {
		if os.Getenv("TAMIOPS_ORIGINAL_"+key+"_SET") == "1" {
			if value, exists := os.LookupEnv("TAMIOPS_ORIGINAL_" + key); exists {
				result = append(result, key+"="+value)
			}
		}
	}
	return result
}

func SetAutoStart(enabled bool, dataDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var filename string
	switch runtime.GOOS {
	case "darwin":
		filename = filepath.Join(home, "Library", "LaunchAgents", "io.tamiops.tami.plist")
	case "linux":
		base, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		filename = filepath.Join(base, "autostart", "tami.desktop")
	case "windows":
		base := os.Getenv("APPDATA")
		if base == "" {
			return errors.New("无法找到启动目录")
		}
		filename = filepath.Join(base, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "Tami.cmd")
	default:
		return fmt.Errorf("%s 不支持自动启动配置", runtime.GOOS)
	}
	if !enabled {
		err = os.Remove(filename)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// The executable inside an AppImage lives at a temporary mount/extraction
	// path. Autostart must invoke the original, persistent AppImage file.
	if runtime.GOOS == "linux" && os.Getenv("APPDIR") != "" && os.Getenv("APPIMAGE") != "" {
		executable, err = filepath.Abs(os.Getenv("APPIMAGE"))
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(dataDir) == "" {
		return errors.New("数据目录不能为空")
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	content, err := renderAutoStartContent(runtime.GOOS, executable, dataDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	return os.WriteFile(filename, []byte(content), 0600)
}

func renderAutoStartContent(goos, executable, dataDir string) (string, error) {
	switch goos {
	case "darwin":
		var escapedExecutable, escapedDataDir strings.Builder
		_ = xml.EscapeText(&escapedExecutable, []byte(executable))
		_ = xml.EscapeText(&escapedDataDir, []byte(dataDir))
		content := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>io.tamiops.tami</string><key>ProgramArguments</key><array><string>` + escapedExecutable.String() + `</string><string>--data-dir</string><string>` + escapedDataDir.String() + `</string></array><key>RunAtLoad</key><true/></dict></plist>`
		return content, nil
	case "linux":
		if strings.ContainsAny(executable+dataDir, "\n\r\x00") {
			return "", errors.New("启动路径包含不支持的字符")
		}
		escape := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "`", "\\`", "$", "\\$", "%", "%%")
		quotedExecutable := escape.Replace(executable)
		quotedDataDir := escape.Replace(dataDir)
		content := "[Desktop Entry]\nType=Application\nName=tamiops\nExec=\"" + quotedExecutable + "\" --data-dir=\"" + quotedDataDir + "\"\nTerminal=false\n"
		return content, nil
	case "windows":
		if strings.ContainsAny(executable+dataDir, "%\"\r\n\x00") {
			return "", errors.New("启动路径包含不支持的字符")
		}
		return "@echo off\r\nstart \"\" \"" + executable + "\" --data-dir=\"" + dataDir + "\"\r\n", nil
	default:
		return "", fmt.Errorf("%s 不支持自动启动配置", goos)
	}
}
