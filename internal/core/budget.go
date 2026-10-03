package core

import (
	"errors"
	"io"
	"math"
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

// stagingBytesLocked counts files that aren't being written from their on-disk
// sizes. Open staging files use tracked byte counts instead: Windows may deny
// path-based metadata queries while their handles are open, and those files are
// already represented by both written bytes and their remaining reservation.
func (s *Service) stagingBytesLocked(excludePath string) int64 {
	root := filepath.Join(s.dir, "staging")
	excludePath = filepath.Clean(excludePath)
	var total int64
	visited := make(map[string]bool, len(s.stagingReservations))
	add := func(n int64) {
		if n < 0 || total > math.MaxInt64-n {
			total = math.MaxInt64
		} else {
			total += n
		}
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		key := filepath.Clean(path)
		if key == excludePath {
			return nil
		}
		if remaining, active := s.stagingReservations[key]; active {
			visited[key] = true
			add(s.stagingWritten[key])
			add(remaining)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		add(info.Size())
		return nil
	})
	if err != nil {
		return math.MaxInt64
	}
	for key, remaining := range s.stagingReservations {
		if key != excludePath && !visited[key] {
			add(s.stagingWritten[key])
			add(remaining)
		}
	}
	return total
}

func (s *Service) stagingUsage() (int64, int64) {
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	return s.stagingBytesLocked(""), s.stagingReservedLocked()
}

type stagingWriter struct {
	s         *Service
	file      *os.File
	key       string
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
	w.s.stagingReservations[w.key] -= int64(n)
	w.s.stagingWritten[w.key] += int64(n)
	return n, err
}
func (s *Service) reserveStaging(f *os.File, remaining int64) (io.Writer, func(), error) {
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	key := filepath.Clean(f.Name())
	if _, active := s.stagingReservations[key]; active {
		return nil, nil, errors.New("此暂存文件正在传输")
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	written := info.Size()
	p := s.Preferences()
	reserved := s.stagingReservedLocked()
	staged := s.stagingBytesLocked(key)
	if remaining < 0 || written < 0 || written > p.StagingBytes || remaining > p.StagingBytes-written || staged > p.StagingBytes-written-remaining || freeBytes(s.dir)-reserved-remaining < 256<<20 {
		return nil, nil, errors.New("暂存额度或磁盘剩余空间不足")
	}
	s.stagingReservations[key] = remaining
	s.stagingWritten[key] = written
	return &stagingWriter{s, f, key, remaining}, func() {
		s.stagingMu.Lock()
		delete(s.stagingReservations, key)
		delete(s.stagingWritten, key)
		s.stagingMu.Unlock()
	}, nil
}
