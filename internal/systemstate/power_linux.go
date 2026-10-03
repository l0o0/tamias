//go:build linux

package systemstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func onAC(context.Context) (bool, bool) {
	const root = "/sys/class/power_supply"
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, false
	}
	known := false
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		typeData, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(typeData)), "Mains") {
			continue
		}
		online, err := os.ReadFile(filepath.Join(dir, "online"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(online)) {
		case "1":
			return true, true
		case "0":
			known = true
		}
	}
	return false, known
}
