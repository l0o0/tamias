//go:build !windows

package core

import (
	"fmt"
	"os"
)

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync directory %s: not a directory", dir)
	}
	return f.Sync()
}
