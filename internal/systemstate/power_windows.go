//go:build windows

package systemstate

import (
	"context"
	"syscall"
	"unsafe"
)

type systemPowerStatus struct {
	status      [4]byte
	batteryLife uint32
	batteryFull uint32
}

func onAC(context.Context) (bool, bool) {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemPowerStatus")
	if err := proc.Find(); err != nil {
		return false, false
	}
	var status systemPowerStatus
	result, _, _ := proc.Call(uintptr(unsafe.Pointer(&status)))
	if result == 0 {
		return false, false
	}
	switch status.status[0] {
	case 0:
		return false, true
	case 1:
		return true, true
	default:
		return false, false
	}
}
