package core

import (
	"context"
	"errors"
	"tamiops/internal/storage"
)

// Upgrade older saved connections once when the desktop starts. Requests are
// serialized and lifecycle-owned; state polling never triggers remote probes.
func (s *Service) startAutomaticConnectionDetection() {
	s.mu.Lock()
	if s.closing || s.connectionDetectionStarted {
		s.mu.Unlock()
		return
	}
	s.connectionDetectionStarted = true
	if s.connectionDetectionPending == nil {
		s.connectionDetectionPending = make(map[string]bool)
	}
	var pending []Connection
	for _, c := range s.cfg.Connections {
		if c.Kind == "demo" || connectionDetectionCurrent(c) || s.connectionDetectionPending[c.ID] || c.Error == "请补充凭据并测试连接" {
			continue
		}
		s.connectionDetectionPending[c.ID] = true
		pending = append(pending, c)
	}
	s.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	ctx, finish, err := s.beginTask(context.Background())
	if err != nil {
		s.clearAutomaticDetection(pending)
		return
	}
	go func() {
		defer finish()
		defer s.clearAutomaticDetection(pending)
		for _, candidate := range pending {
			if ctx.Err() != nil {
				return
			}
			current, err := s.connection(candidate.ID)
			if err == nil && current.Config == candidate.Config && !connectionDetectionCurrent(current) {
				_, err = s.TestConnection(ctx, candidate.ID, true)
				if err != nil && ctx.Err() == nil && !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrNotFound) {
					// Early credential-loading failures occur before TestConnection can
					// persist a result. Surface them without erasing a concurrent edit.
					s.mu.Lock()
					for i := range s.cfg.Connections {
						c := &s.cfg.Connections[i]
						if c.ID == candidate.ID && c.Config == candidate.Config && !connectionDetectionCurrent(*c) {
							c.Error = err.Error()
							_ = s.saveLocked()
							break
						}
					}
					s.mu.Unlock()
				}
			}
			s.mu.Lock()
			delete(s.connectionDetectionPending, candidate.ID)
			s.mu.Unlock()
		}
		s.WakeScheduler()
	}()
}

func (s *Service) clearAutomaticDetection(connections []Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range connections {
		delete(s.connectionDetectionPending, c.ID)
	}
}
