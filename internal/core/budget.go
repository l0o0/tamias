package core

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// remaining reservations count bytes not yet written. File growth and reservation
// reduction happen under the same mutex as admission, avoiding double-counting.
func (s *Service) stagingReservedLocked() int64 {
	var n int64
	for _, v := range s.stagingReservations {
		n += v
	}
	return n
}
func (s *Service) unreservedFreeBytes() int64 {
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	return freeBytes(s.dir) - s.stagingReservedLocked()
}

type stagingWriter struct {
	s         *Service
	file      *os.File
	remaining int64
}

func (w *stagingWriter) Write(p []byte) (int, error) {
	w.s.stagingMu.Lock()
	defer w.s.stagingMu.Unlock()
	if int64(len(p)) > w.remaining {
		return 0, errors.New("传输超过预留暂存额度")
	}
	n, err := w.file.Write(p)
	w.remaining -= int64(n)
	w.s.stagingReservations[w.file.Name()] -= int64(n)
	return n, err
}
func (s *Service) reserveStaging(f *os.File, remaining int64) (io.Writer, func(), error) {
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	p := s.Preferences()
	reserved := s.stagingReservedLocked()
	if remaining < 0 || remaining+directorySize(filepath.Join(s.dir, "staging"))+reserved > p.StagingBytes || freeBytes(s.dir)-reserved-remaining < 256<<20 {
		return nil, nil, errors.New("暂存额度或磁盘剩余空间不足")
	}
	if _, active := s.stagingReservations[f.Name()]; active {
		return nil, nil, errors.New("此暂存文件正在传输")
	}
	s.stagingReservations[f.Name()] = remaining
	return &stagingWriter{s, f, remaining}, func() { s.stagingMu.Lock(); delete(s.stagingReservations, f.Name()); s.stagingMu.Unlock() }, nil
}
