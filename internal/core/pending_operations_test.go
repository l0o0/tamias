package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"testing"
)

// pendingReplyStore models a DAV server whose response is lost after a PUT.
// The response body of a later GET has no Content-Length, as with chunked DAV.
type pendingReplyStore struct {
	storage.Store
	outcome string
	puts    int
}

func (st *pendingReplyStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, _ storage.Condition) (storage.Entry, error) {
	st.puts++
	switch st.outcome {
	case "committed":
		if _, err := st.Store.Put(ctx, key, body, size, storage.Condition{}); err != nil {
			return storage.Entry{}, err
		}
	case "mismatch":
		if _, err := st.Store.Put(ctx, key, strings.NewReader("foreign content"), int64(len("foreign content")), storage.Condition{}); err != nil {
			return storage.Entry{}, err
		}
	case "absent":
		// The server accepted the request but the object never appeared.
	default:
		panic("unknown pending reply outcome")
	}
	return storage.Entry{}, errors.New("PUT response lost")
}

func (st *pendingReplyStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	body, entry, err := st.Store.Open(ctx, key, etag)
	if err == nil {
		entry.Size = -1
	}
	return body, entry, err
}

func usePendingReplyStore(t *testing.T, s *Service, id, outcome string) *pendingReplyStore {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &pendingReplyStore{Store: s.stores[id], outcome: outcome}
	s.stores[id] = st
	return st
}

func TestPendingUploadReconcilesBeforeFreshSyncPlan(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	local := t.TempDir()
	j := makeBaseline(t, s, id, local, "upload", "notes.txt")
	setSyncConnectionPolicy(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	if err := os.WriteFile(filepath.Join(local, "notes.txt"), []byte("new local content"), 0600); err != nil {
		t.Fatal(err)
	}
	st := usePendingReplyStore(t, s, id, "committed")
	plan, err := s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action, ok := actionOf(plan, "notes.txt"); !ok || action.Kind != "upload" {
		t.Fatalf("expected upload before lost reply, got %+v", action)
	}
	if _, err = s.RunPlan(ctx, j.ID, plan.Token); err == nil {
		t.Fatal("lost PUT reply was reported as a completed sync")
	}
	if st.puts != 1 {
		t.Fatalf("remote PUT count after failed run = %d", st.puts)
	}
	pending, err := s.PendingOperations()
	if err != nil || len(pending) != 1 || pending[0].State != "uncertain" {
		t.Fatalf("pending operation = %+v, error = %v", pending, err)
	}
	var staging string
	if err = s.db.QueryRow("SELECT staging FROM operations WHERE id=?", pending[0].ID).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(staging); err != nil {
		t.Fatalf("unknown upload lost its staging bytes: %v", err)
	}
	var baselineHash string
	if err = s.db.QueryRow("SELECT local_hash FROM baseline WHERE job=? AND path=?", j.ID, "notes.txt").Scan(&baselineHash); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("new local content"))
	if baselineHash == hex.EncodeToString(want[:]) {
		t.Fatal("failed run advanced the baseline")
	}

	// Preview first reconciles the journal against the actual remote bytes,
	// then computes a fresh plan from current local and remote state.
	fresh, err := s.Preview(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action, ok := actionOf(fresh, "notes.txt"); !ok || action.Kind != "skip" || !action.BaselineMerge {
		t.Fatalf("matching remote content did not produce a baseline merge: %+v", action)
	}
	var operationState string
	if err = s.db.QueryRow("SELECT state FROM operations WHERE id=?", pending[0].ID).Scan(&operationState); err != nil || operationState != "committed" {
		t.Fatalf("reconciled operation state = %q, error = %v", operationState, err)
	}
	if _, err = os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified operation still retained staging: %v", err)
	}
	if _, err = s.RunPlan(ctx, j.ID, fresh.Token); err != nil {
		t.Fatal(err)
	}
	var remoteETag string
	if err = s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", j.ID, "notes.txt").Scan(&baselineHash, &remoteETag); err != nil {
		t.Fatal(err)
	}
	remote, err := (Backend{s, id}).Stat(ctx, "notes.txt")
	if err != nil || baselineHash != hex.EncodeToString(want[:]) || remoteETag != remote.ETag {
		t.Fatalf("fresh baseline = %q/%q, remote = %+v, error = %v", baselineHash, remoteETag, remote, err)
	}
	if st.puts != 1 {
		t.Fatalf("fresh sync repeated an already committed PUT: %d", st.puts)
	}
}

func TestUnverifiablePendingUploadBlocksSyncWithoutChangingData(t *testing.T) {
	for _, outcome := range []string{"absent", "mismatch"} {
		t.Run(outcome, func(t *testing.T) {
			s, id := testService(t)
			ctx := context.Background()
			local := t.TempDir()
			var j Job
			if outcome == "mismatch" {
				j = makeBaseline(t, s, id, local, "upload", "notes.txt")
			} else {
				j = syncJob(t, s, id, local, "upload", Job{})
			}
			setSyncConnectionPolicy(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
			if err := os.WriteFile(filepath.Join(local, "notes.txt"), []byte("wanted content"), 0600); err != nil {
				t.Fatal(err)
			}
			var beforeHash, beforeETag string
			err := s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", j.ID, "notes.txt").Scan(&beforeHash, &beforeETag)
			if outcome == "absent" && !errors.Is(err, sql.ErrNoRows) || outcome == "mismatch" && err != nil {
				t.Fatalf("initial baseline query: %v", err)
			}
			st := usePendingReplyStore(t, s, id, outcome)
			plan, err := s.Preview(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, runErr := s.RunPlan(ctx, j.ID, plan.Token)
			if runErr == nil {
				t.Fatal("lost response was reported as success")
			}
			pending, err := s.PendingOperations()
			if err != nil || len(pending) != 1 || pending[0].State != "uncertain" {
				t.Fatalf("run error = %v; pending operation = %+v, query error = %v", runErr, pending, err)
			}
			var staging string
			if err = s.db.QueryRow("SELECT staging FROM operations WHERE id=?", pending[0].ID).Scan(&staging); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Preview(ctx, j.ID); !errors.Is(err, ErrPendingOperation) {
				t.Fatalf("unverifiable remote state did not block preview: %v", err)
			}
			if _, err = os.Stat(staging); err != nil {
				t.Fatalf("unverifiable operation lost staged bytes: %v", err)
			}
			var state string
			if err = s.db.QueryRow("SELECT state FROM operations WHERE id=?", pending[0].ID).Scan(&state); err != nil || state != "uncertain" {
				t.Fatalf("unverifiable operation state = %q, error = %v", state, err)
			}
			var afterHash, afterETag string
			err = s.db.QueryRow("SELECT local_hash,remote_etag FROM baseline WHERE job=? AND path=?", j.ID, "notes.txt").Scan(&afterHash, &afterETag)
			if outcome == "absent" && !errors.Is(err, sql.ErrNoRows) || outcome == "mismatch" && (err != nil || afterHash != beforeHash || afterETag != beforeETag) {
				t.Fatalf("unverifiable operation changed baseline: %q/%q, error = %v", afterHash, afterETag, err)
			}
			if got := localText(t, local, "notes.txt"); got != "wanted content" {
				t.Fatalf("unverifiable operation changed local content: %q", got)
			}
			if st.puts != 1 {
				t.Fatalf("pending operation was retried: %d PUTs", st.puts)
			}
		})
	}
}

func TestJobPendingOperationsIgnoreMetadataOnly(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	local := t.TempDir()
	b := Backend{s, id}
	if err := b.Mkdir(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	j := syncJob(t, s, id, local, "upload", Job{})
	c, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	insertPending := func(key string) string {
		t.Helper()
		id := ID()
		evidence := operationReceipt{Kind: "upload", DestinationConnection: c.ID, DestinationPath: key, DestinationAbsent: true, DestinationHash: strings.Repeat("0", 64), DestinationSize: 1}
		if _, err := s.beginWithEvidence(c, key, "upload", "", id, evidence); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE operations SET state='uncertain' WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	metadataID := insertPending("docs/.DS_Store")
	ids, err := s.jobPendingOperationIDs(j)
	if err != nil || len(ids) != 0 {
		t.Fatalf("metadata-only pending operations = %v, error = %v", ids, err)
	}
	notesID := insertPending("docs/notes.txt")
	ids, err = s.jobPendingOperationIDs(j)
	if err != nil || len(ids) != 1 || ids[0] != notesID {
		t.Fatalf("pending operations = %v, want only %s, error = %v", ids, notesID, err)
	}
	if _, err = s.Preview(ctx, j.ID); !errors.Is(err, ErrPendingOperation) {
		t.Fatalf("ordinary pending upload did not block sync preview: %v", err)
	}
	if _, err = s.db.Exec("UPDATE operations SET state='failed' WHERE id=?", notesID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Preview(ctx, j.ID); err != nil {
		t.Fatalf("excluded metadata pending upload blocked sync preview: %v", err)
	}
	var state string
	if err = s.db.QueryRow("SELECT state FROM operations WHERE id=?", metadataID).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("metadata receipt was changed: %q, error = %v", state, err)
	}
}

func TestJobPendingDirectoryNamedDSStoreStillBlocks(t *testing.T) {
	s, id := testService(t)
	j := syncJob(t, s, id, t.TempDir(), "upload", Job{})
	c, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	operationID := ID()
	evidence := operationReceipt{Kind: "mkdir", DestinationConnection: c.ID, DestinationPath: "docs/.DS_Store"}
	if _, err = s.beginWithEvidence(c, "docs/.DS_Store", "mkdir", "", operationID, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE operations SET state='uncertain' WHERE id=?", operationID); err != nil {
		t.Fatal(err)
	}
	ids, err := s.jobPendingOperationIDs(j)
	if err != nil || len(ids) != 1 || ids[0] != operationID {
		t.Fatalf("directory operation at .DS_Store was hidden: ids=%v, want=%s, err=%v", ids, operationID, err)
	}
}
