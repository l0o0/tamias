package core

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStagingAdmissionCountsSpoolReturnedWhileOpen(t *testing.T) {
	s, _ := testService(t)
	p := s.Preferences()
	p.StagingBytes = 16 << 20
	p.MaxFileBytes = 16 << 20
	if err := s.SetPreferences(p); err != nil {
		t.Fatal(err)
	}

	staged, err := s.spool(t.Context(), bytes.NewReader(make([]byte, 2<<20)), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	other, err := os.CreateTemp(filepath.Join(s.dir, "staging"), "reserve-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = other.Close()
		_ = os.Remove(other.Name())
	}()
	if _, release, err := s.reserveStaging(other, 15<<20); err == nil {
		release()
		t.Fatal("admitted another 15 MiB while a 2 MiB spool file remained open under a 16 MiB limit")
	}
	_, release, err := s.reserveStaging(other, 14<<20)
	if err != nil {
		t.Fatal("double-counted or lost the open spool file's size", err)
	}
	release()
}
