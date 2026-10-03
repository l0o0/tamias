//go:build darwin

package systemstate

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

func onAC(parent context.Context) (bool, bool) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "pmset", "-g", "batt").Output()
	if err != nil {
		return false, false
	}
	text := strings.ToLower(string(output))
	if strings.Contains(text, "now drawing from 'ac power'") || strings.Contains(text, `now drawing from "ac power"`) {
		return true, true
	}
	if strings.Contains(text, "now drawing from 'battery power'") || strings.Contains(text, `now drawing from "battery power"`) {
		return false, true
	}
	return false, false
}
