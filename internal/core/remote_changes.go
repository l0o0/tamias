package core

import (
	"database/sql"
	"encoding/json"
	"time"
)

type changedResource struct{ namespace, path string }

// finalizeOperation commits the receipt and its invalidations together. Callers
// hold writes; no cacheMu is acquired here (cache operations lock it first).
func (s *Service) finalizeOperation(id, state string, evidence operationReceipt) error {
	s.mu.Lock()
	connections := append([]Connection(nil), s.cfg.Connections...)
	jobs := append([]Job(nil), s.cfg.Jobs...)
	s.mu.Unlock()
	byID := make(map[string]Connection, len(connections))
	for _, c := range connections {
		byID[c.ID] = c
	}
	affected := []changedResource{}
	add := func(id, key string) {
		if c, ok := byID[id]; ok {
			ns, p := resource(c, key)
			affected = append(affected, changedResource{ns, p})
		}
	}
	changed := state == "committed" || state == "partial" || state == "uncertain" || evidence.Step == "destination-verified" || evidence.Step == "source-delete-unknown"
	if changed {
		add(evidence.DestinationConnection, evidence.DestinationPath)
		if evidence.Kind == "delete" || evidence.Kind == "rmdir" || evidence.Kind == "move" && state != "partial" {
			add(evidence.SourceConnection, evidence.SourcePath)
		}
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE operations SET state=?,receipt=? WHERE id=?", state, string(raw), id); err != nil {
		return err
	}
	// Legacy receipts may only identify resources through the journal table.
	if changed && len(affected) == 0 {
		rows, e := tx.Query("SELECT connection,path FROM operation_resources WHERE operation_id=?", id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var r changedResource
			if e = rows.Scan(&r.namespace, &r.path); e != nil {
				rows.Close()
				return e
			}
			affected = append(affected, r)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	if len(affected) > 0 {
		if err = invalidateRemoteResources(tx, byID, jobs, affected); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(affected) > 0 {
		s.wakeRemoteChanges()
	}
	return nil
}

func invalidateRemoteResources(tx *sql.Tx, connections map[string]Connection, jobs []Job, affected []changedResource) error {
	matches := func(connection, key string) bool {
		c, ok := connections[connection]
		if !ok {
			return false
		}
		ns, p := resource(c, key)
		for _, r := range affected {
			if ns == r.namespace && (p == "" || p == "." || r.path == "" || r.path == "." || pathOverlaps(p, r.path)) {
				return true
			}
		}
		return false
	}
	rows, err := tx.Query("SELECT id,connection,path FROM cache_entries")
	if err != nil {
		return err
	}
	cacheIDs := []string{}
	for rows.Next() {
		var id, connection, key string
		if err = rows.Scan(&id, &connection, &key); err != nil {
			rows.Close()
			return err
		}
		if matches(connection, key) {
			cacheIDs = append(cacheIDs, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range cacheIDs {
		if _, err = tx.Exec("UPDATE cache_entries SET remote_changed=1,checked='' WHERE id=?", id); err != nil {
			return err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, j := range jobs {
		if !matches(j.ConnectionID, j.RemotePath) {
			continue
		}
		if _, err = tx.Exec("INSERT INTO sync_dirty(job,token,updated) VALUES(?,?,?) ON CONFLICT(job) DO UPDATE SET token=excluded.token,updated=excluded.updated", j.ID, ID(), now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) remoteChangeToken(job string) string {
	var token string
	_ = s.db.QueryRow("SELECT token FROM sync_dirty WHERE job=?", job).Scan(&token)
	return token
}

func (s *Service) acknowledgeRemoteChange(job, token string) {
	// A newer write during this scan must survive for the next reconciliation.
	_, _ = s.db.Exec("DELETE FROM sync_dirty WHERE job=? AND token=?", job, token)
}

func (s *Service) wakeRemoteChanges() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if s.schedulerChanged == nil {
		s.schedulerChanged = make(chan struct{}, 1)
	}
	wake, running := s.schedulerChanged, s.schedulerCancel != nil
	s.mu.Unlock()
	if running {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
