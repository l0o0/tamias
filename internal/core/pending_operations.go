package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrPendingOperation distinguishes a write awaiting reconciliation from a
// transient transfer failure. Retrying the same write cannot resolve it.
var ErrPendingOperation = errors.New("存在结果待核对的操作，请先核对远端结果")

func (s *Service) jobPendingOperationIDs(j Job) ([]string, error) {
	c, err := s.connection(j.ConnectionID)
	if err != nil {
		return nil, err
	}
	ns, key := resource(c, j.RemotePath)
	key = strings.TrimSuffix(key, "/")
	rows, err := s.db.Query(`SELECT o.id,o.path,o.kind FROM operations o
	 WHERE o.state IN ('committing','uncertain') AND o.connection=?
	 AND (?='' OR o.path=? OR substr(o.path,1,length(?)+1)=?||'/' OR substr(?,1,length(o.path)+1)=o.path||'/')
	 UNION SELECT o.id,r.path,o.kind FROM operations o JOIN operation_resources r ON r.operation_id=o.id
	 WHERE o.state IN ('committing','uncertain') AND r.connection=?
	 AND (?='' OR r.path=? OR substr(r.path,1,length(?)+1)=?||'/' OR substr(?,1,length(r.path)+1)=r.path||'/')
	 ORDER BY 1,2`, ns, key, key, key, key, key, ns, key, key, key, key, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var id, remotePath, kind string
		if err = rows.Scan(&id, &remotePath, &kind); err != nil {
			return nil, err
		}
		// Excluded descendants do not authorize writes and must not stall an
		// otherwise independent task. Their receipt and staged bytes stay intact.
		rel, inside := strings.CutPrefix(remotePath, key+"/")
		if key == "" {
			rel, inside = strings.TrimPrefix(remotePath, "/"), true
		}
		fileOperation := kind == "upload" || kind == "copy" || kind == "move" || kind == "restore" || kind == "delete"
		if inside && (excludedPath(j.Exclude, rel) || fileOperation && isSyncMetadataPath(rel)) {
			continue
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, rows.Err()
}

func (s *Service) hasJobPendingOperations(j Job) (bool, error) {
	ids, err := s.jobPendingOperationIDs(j)
	return len(ids) > 0, err
}

// Reconciliation only reads remote state. A new plan is created after this
// step, so confirming an old receipt never approves an old synchronization plan.
func (s *Service) reconcileJobOperations(ctx context.Context, j Job) error {
	ids, err := s.jobPendingOperationIDs(j)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.ReconcileOperation(ctx, id); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w；请在活动的“未决操作”中查看", ErrPendingOperation)
		}
	}
	return nil
}
