package core

import (
	"context"
	"errors"
	"time"
)

func (s *Service) beginTask(parent context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, nil, errors.New("应用正在退出")
	}
	ctx, cancel := context.WithCancel(parent)
	id := ID()
	s.backgroundCancels[id] = cancel
	s.tasks.Add(1)
	return ctx, func() { cancel(); s.mu.Lock(); delete(s.backgroundCancels, id); s.mu.Unlock(); s.tasks.Done() }, nil
}
func (s *Service) StartMaintenance() {
	s.mu.Lock()
	if s.closing || s.maintenanceCancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.maintenanceCancel = cancel
	s.maintenanceDone = done
	s.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cycle, finish, err := s.beginTask(ctx)
				if err != nil {
					return
				}
				_, _ = s.RunDueBackups(cycle)
				if cycle.Err() == nil {
					_, _ = s.PruneRecoveries(time.Now())
				}
				finish()
			}
		}
	}()
}
func (s *Service) stopMaintenance() {
	s.mu.Lock()
	cancel, done := s.maintenanceCancel, s.maintenanceDone
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
