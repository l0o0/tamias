package core

import (
	"context"
	"tamiops/internal/gateway"
	"time"
)

type scopeKey struct{}
type transferScope struct {
	Kind string
	ID   string
}

func withTransferScope(ctx context.Context, kind, id string) context.Context {
	return context.WithValue(ctx, scopeKey{}, transferScope{kind, id})
}
func (s *Service) scopedStats(ctx context.Context, connection string, up, down int64, failed bool) {
	s.recordStats("connection", connection, up, down, failed)
	if v, ok := ctx.Value(scopeKey{}).(transferScope); ok {
		s.recordStats(v.Kind, v.ID, up, down, failed)
	}
}

type GatewayAccess struct {
	Time         string `json:"time"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Status       int    `json:"status"`
	BytesIn      int64  `json:"bytesIn"`
	BytesOut     int64  `json:"bytesOut"`
	Milliseconds int64  `json:"milliseconds"`
}

func (s *Service) gatewayAccess(id string, e gateway.AccessEvent) {
	s.recordStats("gateway", id, e.BytesIn, e.BytesOut, e.Status >= 400)
	key := e.Path
	if len(key) > 1024 {
		key = key[:1024]
	}
	_, _ = s.db.Exec("INSERT INTO gateway_access(gateway,time,method,path,status,bytes_in,bytes_out,milliseconds) VALUES(?,?,?,?,?,?,?,?)", id, e.At.UTC().Format(time.RFC3339), e.Method, key, e.Status, e.BytesIn, e.BytesOut, e.Duration.Milliseconds())
	_, _ = s.db.Exec("DELETE FROM gateway_access WHERE id NOT IN (SELECT id FROM gateway_access ORDER BY id DESC LIMIT 500)")
}
func (s *Service) GatewayAccess(id string) ([]GatewayAccess, error) {
	rows, err := s.db.Query("SELECT time,method,path,status,bytes_in,bytes_out,milliseconds FROM gateway_access WHERE gateway=? ORDER BY id DESC LIMIT 100", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GatewayAccess{}
	for rows.Next() {
		var v GatewayAccess
		if err = rows.Scan(&v.Time, &v.Method, &v.Path, &v.Status, &v.BytesIn, &v.BytesOut, &v.Milliseconds); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type QueueItem struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	BytesDone int64  `json:"bytesDone"`
	Error     string `json:"error"`
	Updated   string `json:"updated"`
}

func (s *Service) SyncQueue(id string) ([]QueueItem, error) {
	rows, err := s.db.Query("SELECT path,kind,state,bytes_done,error,updated FROM sync_queue WHERE job=? ORDER BY path LIMIT 10000", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueItem{}
	for rows.Next() {
		var v QueueItem
		if err = rows.Scan(&v.Path, &v.Kind, &v.State, &v.BytesDone, &v.Error, &v.Updated); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
