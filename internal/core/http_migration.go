package core

import "net/http"

func (s *Service) migrationGet(r *http.Request) (any, error, bool) {
	q := r.URL.Query()
	switch r.URL.Path {
	case "/api/migrations":
		v, e := s.ListMigrationJobs()
		return v, e, true
	case "/api/migrations/runs":
		v, e := s.ListMigrationRuns(q.Get("jobId"))
		return v, e, true
	}
	return nil, nil, false
}
func (s *Service) migrationCommand(r *http.Request) (any, error, bool) {
	switch r.URL.Path {
	case "/api/migrations", "/api/migrations/update":
		var m MigrationJob
		if err := decode(r, &m); err != nil {
			return nil, err, true
		}
		if r.URL.Path == "/api/migrations" {
			v, e := s.CreateMigrationJob(m)
			return v, e, true
		}
		v, e := s.UpdateMigrationJob(m)
		return v, e, true
	}
	switch r.URL.Path {
	case "/api/migrations/delete", "/api/migrations/preview", "/api/migrations/start", "/api/migrations/resume", "/api/migrations/cancel", "/api/migrations/cleanup-preview", "/api/migrations/cleanup":
	default:
		return nil, nil, false
	}
	var c struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := decode(r, &c); err != nil {
		return nil, err, true
	}
	switch r.URL.Path {
	case "/api/migrations/delete":
		return nil, s.DeleteMigrationJob(c.ID), true
	case "/api/migrations/preview":
		v, e := s.PreviewMigration(r.Context(), c.ID)
		return v, e, true
	case "/api/migrations/start":
		v, e := s.StartMigration(r.Context(), c.ID, c.Token)
		return v, e, true
	case "/api/migrations/resume":
		v, e := s.ResumeMigration(r.Context(), c.ID)
		return v, e, true
	case "/api/migrations/cancel":
		return nil, s.CancelMigration(c.ID), true
	case "/api/migrations/cleanup-preview":
		v, e := s.PreviewMigrationCleanup(r.Context(), c.ID)
		return v, e, true
	case "/api/migrations/cleanup":
		return nil, s.CleanupMigrationSource(r.Context(), c.ID, c.Token), true
	}
	return nil, nil, false
}
