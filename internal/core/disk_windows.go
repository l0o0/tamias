//go:build windows

package core

import "golang.org/x/sys/windows"

func freeBytes(dir string) int64 {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var free, total, available uint64
	if windows.GetDiskFreeSpaceEx(p, &available, &total, &free) != nil {
		return -1
	}
	return int64(available)
}
