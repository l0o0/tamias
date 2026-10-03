//go:build !windows

package core

import "os"

func openCacheEditor(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0600)
}
