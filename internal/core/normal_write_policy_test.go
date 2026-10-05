package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tamiops/internal/storage"
)

func setNormalRecoveryForTest(t *testing.T, s *Service, id string, keep bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == id {
			s.cfg.Connections[i].KeepRecovery = keep
			return
		}
	}
	t.Fatalf("connection %q not found", id)
}

func recoveryDataFiles(t *testing.T, s *Service) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(s.dir, "recovery", "*.data"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestNormalUploadUsesSameRemoteNameWithoutRecovery(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Put(ctx, "notes.txt", strings.NewReader("before"), 6, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	remote := &compatibilityTestStore{Store: raw}
	s.stores[id] = remote
	setPolicyForTest(t, s, id, "", storage.Capabilities{})
	b := Backend{s, id}
	got, err := b.Upload(ctx, "notes.txt", strings.NewReader("after"), 5, storage.Condition{})
	if err != nil || got.Path != "notes.txt" || !strongTag(got.ETag) {
		t.Fatalf("normal overwrite: entry=%+v err=%v", got, err)
	}
	if content := readBody(t, b, "notes.txt"); content != "after" {
		t.Fatalf("remote content = %q", content)
	}
	entries, err := raw.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	matched := 0
	for _, entry := range entries {
		if entry.Path == "notes.txt" {
			matched++
		} else if strings.HasPrefix(entry.Path, "notes (") {
			t.Fatalf("normal upload created a duplicate: %+v", entry)
		}
	}
	if matched != 1 {
		t.Fatalf("expected exactly one notes.txt, got %d in %+v", matched, entries)
	}
	if files := recoveryDataFiles(t, s); len(files) != 0 {
		t.Fatalf("normal upload created recovery files: %v", files)
	}
	if remote.puts != 1 {
		t.Fatalf("PUT count = %d", remote.puts)
	}
	var state string
	if err = s.db.QueryRow(`SELECT state FROM operations WHERE kind='upload'`).Scan(&state); err != nil || state != "committed" {
		t.Fatalf("upload state=%q err=%v", state, err)
	}
}

func TestNormalHTTPUploadUpdatesSameRemoteName(t *testing.T) {
	s, id := testService(t)
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	s.stores[id] = &compatibilityTestStore{Store: raw}
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	for _, body := range []string{"first", "second"} {
		request := httptest.NewRequest(http.MethodPost, "/api/files/upload?connectionId="+url.QueryEscape(id)+"&path=notes.txt", strings.NewReader(body))
		request.Header.Set("X-Tami-Client", "desktop")
		response := httptest.NewRecorder()
		s.Handler(http.NotFoundHandler()).ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"path":"notes.txt"`) {
			t.Fatalf("HTTP upload status=%d response=%s", response.Code, response.Body.String())
		}
	}
	if content := readBody(t, Backend{s, id}, "notes.txt"); content != "second" {
		t.Fatalf("remote content = %q", content)
	}
	entries, err := raw.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Path, "notes (") {
			t.Fatalf("HTTP upload created a duplicate: %+v", entry)
		}
	}
	if files := recoveryDataFiles(t, s); len(files) != 0 {
		t.Fatalf("HTTP upload created recovery files: %v", files)
	}
}

func TestNormalUploadKeepsOldBytesWhenRecoverySelected(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Put(ctx, "notes.txt", strings.NewReader("before"), 6, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	s.stores[id] = &compatibilityTestStore{Store: raw}
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	setNormalRecoveryForTest(t, s, id, true)
	b := Backend{s, id}
	if _, err = b.Upload(ctx, "notes.txt", strings.NewReader("after"), 5, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	if content := readBody(t, b, "notes.txt"); content != "after" {
		t.Fatalf("remote content = %q", content)
	}
	files := recoveryDataFiles(t, s)
	if len(files) != 1 {
		t.Fatalf("recovery count = %d", len(files))
	}
	old, err := os.ReadFile(files[0])
	if err != nil || string(old) != "before" {
		t.Fatalf("saved old content=%q err=%v", old, err)
	}
}

func TestNormalPostCommitMismatchKeepsUncertainStagingWithoutRecovery(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Put(ctx, "notes.txt", strings.NewReader("before"), 6, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	remote := &compatibilityTestStore{Store: raw}
	remote.afterPut = func(ctx context.Context, key string) {
		_, _ = raw.Put(ctx, key, strings.NewReader("external"), 8, storage.Condition{})
	}
	s.stores[id] = remote
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	b := Backend{s, id}
	if _, err = b.Upload(ctx, "notes.txt", strings.NewReader("after"), 5, storage.Condition{}); err == nil || errors.Is(err, storage.ErrConflict) {
		t.Fatalf("postcommit mismatch was classified as a safe conflict: %v", err)
	}
	var state, staging string
	if err = s.db.QueryRow(`SELECT state,staging FROM operations WHERE kind='upload'`).Scan(&state, &staging); err != nil || state != "uncertain" {
		t.Fatalf("operation state=%q staging=%q err=%v", state, staging, err)
	}
	if _, err = os.Stat(staging); err != nil {
		t.Fatalf("uncertain upload lost staging: %v", err)
	}
	if files := recoveryDataFiles(t, s); len(files) != 0 {
		t.Fatalf("recovery was enabled unexpectedly: %v", files)
	}
	if _, err = b.Upload(ctx, "notes.txt", strings.NewReader("retry"), 5, storage.Condition{}); err == nil || !strings.Contains(err.Error(), "待核对") {
		t.Fatalf("uncertain resource allowed a second write: %v", err)
	}
}

func TestNormalUnknownResponseCanReconcileVerifiedContentWithoutRecovery(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Put(ctx, "notes.txt", strings.NewReader("before"), 6, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	s.stores[id] = uncertainStore{&compatibilityTestStore{Store: raw}}
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	if _, err = (Backend{s, id}).Upload(ctx, "notes.txt", strings.NewReader("after"), 5, storage.Condition{}); err == nil {
		t.Fatal("unknown PUT response was treated as success")
	}
	var operationID, staging, state string
	if err = s.db.QueryRow(`SELECT id,staging,state FROM operations WHERE kind='upload'`).Scan(&operationID, &staging, &state); err != nil || state != "uncertain" {
		t.Fatalf("operation id=%q state=%q err=%v", operationID, state, err)
	}
	if err = s.ReconcileOperation(ctx, operationID); err != nil {
		t.Fatalf("verified final content could not reconcile: %v", err)
	}
	if err = s.db.QueryRow(`SELECT state FROM operations WHERE id=?`, operationID).Scan(&state); err != nil || state != "committed" {
		t.Fatalf("reconciled state=%q err=%v", state, err)
	}
	if _, err = os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reconciled operation retained staging: %v", err)
	}
	if content := readBody(t, Backend{s, id}, "notes.txt"); content != "after" {
		t.Fatalf("verified final content = %q", content)
	}
	if files := recoveryDataFiles(t, s); len(files) != 0 {
		t.Fatalf("reconciled upload created recovery files: %v", files)
	}
}

type mutateDuringRead struct {
	reader io.Reader
	mutate func() error
	done   bool
}

func (r *mutateDuringRead) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		if err := r.mutate(); err != nil {
			return 0, err
		}
	}
	return r.reader.Read(p)
}

func TestNormalUploadWithoutConditionRejectsRemoteChangeDuringSpool(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Put(ctx, "notes.txt", strings.NewReader("before"), 6, storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	remote := &compatibilityTestStore{Store: raw}
	s.stores[id] = remote
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	reader := &mutateDuringRead{
		reader: strings.NewReader("after"),
		mutate: func() error {
			_, err := raw.Put(ctx, "notes.txt", strings.NewReader("external"), 8, storage.Condition{})
			return err
		},
	}
	if _, err = (Backend{s, id}).Upload(ctx, "notes.txt", reader, 5, storage.Condition{}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("changed remote accepted: %v", err)
	}
	if remote.puts != 0 {
		t.Fatalf("app wrote after remote changed: puts=%d", remote.puts)
	}
	if content := readBody(t, Backend{s, id}, "notes.txt"); content != "external" {
		t.Fatalf("external content changed: %q", content)
	}
	if files := recoveryDataFiles(t, s); len(files) != 0 {
		t.Fatalf("rejected upload created recovery files: %v", files)
	}
}

func TestNormalOverwriteIgnoresUnusedRecoveryBudget(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	prefs := s.Preferences()
	prefs.MaxFileBytes = 1 << 20
	prefs.RecoveryBytes = 16 << 20
	if err := s.SetPreferences(prefs); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", int(prefs.MaxFileBytes)+1)
	if _, err = raw.Put(ctx, "notes.txt", strings.NewReader(large), int64(len(large)), storage.Condition{}); err != nil {
		t.Fatal(err)
	}
	filler, err := os.Create(filepath.Join(s.dir, "recovery", "quota-filler"))
	if err != nil {
		t.Fatal(err)
	}
	if err = filler.Truncate(prefs.RecoveryBytes); err != nil {
		filler.Close()
		t.Fatal(err)
	}
	if err = filler.Close(); err != nil {
		t.Fatal(err)
	}
	s.stores[id] = &compatibilityTestStore{Store: raw}
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	b := Backend{s, id}
	if _, err = b.Upload(ctx, "notes.txt", strings.NewReader("after"), 5, storage.Condition{}); err != nil {
		t.Fatalf("recovery opt-out still blocked ordinary overwrite: %v", err)
	}
	if content := readBody(t, b, "notes.txt"); content != "after" {
		t.Fatalf("remote content = %q", content)
	}
	if files := recoveryDataFiles(t, s); len(files) != 0 {
		t.Fatalf("recovery files created: %v", files)
	}
}

func TestNormalModeDoesNotClaimConditionalCapabilitiesOrAllowDelete(t *testing.T) {
	s, id := testService(t)
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	c, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if !canUpload(c) || canWriteStrict(c) || canDelete(c) || c.Capabilities != (storage.Capabilities{}) {
		t.Fatalf("normal mode forged server capabilities: %+v", c)
	}
	ctx := context.Background()
	raw, err := s.store(id)
	if err != nil {
		t.Fatal(err)
	}
	old, err := raw.Put(ctx, "notes.txt", strings.NewReader("before"), 6, storage.Condition{})
	if err != nil {
		t.Fatal(err)
	}
	if err = (Backend{s, id}).Delete(ctx, "notes.txt", storage.Condition{IfMatch: old.ETag}); err == nil {
		t.Fatal("normal upload permission allowed a remote delete")
	}
	if content := readBody(t, Backend{s, id}, "notes.txt"); content != "before" {
		t.Fatalf("remote content after rejected delete = %q", content)
	}
}

func TestNormalConnectionReportsConditionalLimitsWithoutFailingReadCheck(t *testing.T) {
	s, id := testService(t)
	const detail = "条件请求未被远端执行"
	remote := &connectionRestrictionStore{Store: storage.NewMemory(), putErr: fmt.Errorf("%w: %s", storage.ErrConditionalUnsupported, detail)}
	s.mu.Lock()
	s.stores[id] = remote
	s.mu.Unlock()
	setPolicyForTest(t, s, id, storage.WriteModeStandard, storage.Capabilities{})
	caps, err := s.TestConnection(context.Background(), id, true)
	if err != nil {
		t.Fatalf("readable normal connection was reported unusable: %v", err)
	}
	if caps.ConditionalWrite || caps.ConditionalDelete {
		t.Fatalf("unverified conditional capabilities: %+v", caps)
	}
	c, err := s.connection(id)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Tested || c.Error != "" || !strings.Contains(c.WriteRestriction, detail) || !canUpload(c) || canDelete(c) {
		t.Fatalf("normal connection health/capabilities: %+v", c)
	}
}
