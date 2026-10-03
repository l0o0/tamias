//go:build !linux && !darwin && !windows

package systemstate

import "context"

func onAC(context.Context) (bool, bool) { return false, false }
