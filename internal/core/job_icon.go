package core

import (
	"errors"
	"tamiops/internal/storage"
)

var supportedJobIcons = map[string]struct{}{
	"folder": {}, "document": {}, "book": {}, "graduation": {}, "code": {},
	"image": {}, "music": {}, "video": {}, "archive": {}, "cloud": {},
	"briefcase": {}, "heart": {}, "star": {}, "squirrel": {},
}

func validateJobIcon(icon string) error {
	if icon == "" {
		return nil
	}
	if _, ok := supportedJobIcons[icon]; !ok {
		return errors.New("无效任务图标")
	}
	return nil
}

// UpdateJobIcon changes only presentation metadata. Sync state and its safety
// records are kept intact, including when the job is currently running.
func (s *Service) UpdateJobIcon(id, icon string) error {
	if err := validateJobIcon(icon); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		if s.cfg.Jobs[i].ID != id {
			continue
		}
		old := s.cfg.Jobs[i].Icon
		if old == icon {
			return nil
		}
		s.cfg.Jobs[i].Icon = icon
		if err := s.saveLocked(); err != nil {
			s.cfg.Jobs[i].Icon = old
			return err
		}
		return nil
	}
	return storage.ErrNotFound
}
