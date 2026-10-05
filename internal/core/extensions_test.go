package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"tamiops/internal/storage"
	"testing"
	"time"
)

func TestDAVLeaseCoordinatesCoreWritersAndAliases(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	e, err := b.Put(ctx, "lease.txt", strings.NewReader("one"), 3, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := b.AcquireDAVLock(ctx, "lease.txt", "editor", false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Put(ctx, "lease.txt", strings.NewReader("two"), 3, storage.Condition{IfMatch: e.ETag}); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("unlocked internal writer: %v", err)
	}
	leases, err := b.DAVLocks(ctx, "lease.txt")
	if err != nil || len(leases) != 1 || leases[0].Token != token {
		t.Fatalf("lock discovery: %v %v", leases, err)
	}
	authenticated := storage.WithLockTokens(ctx, []string{token})
	if _, err = b.Put(authenticated, "lease.txt", strings.NewReader("two"), 3, storage.Condition{IfMatch: e.ETag}); err != nil {
		t.Fatal(err)
	}
	if err = b.UnlockDAVLock(ctx, "lease.txt", "wrong"); !errors.Is(err, storage.ErrLocked) {
		t.Fatal(err)
	}
	if err = b.UnlockDAVLock(ctx, "lease.txt", token); err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.AcquireDAVLock(ctx, "Documents", "editor", true, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = b.Mkdir(ctx, "Documents/blocked"); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("deep lock not honored: %v", err)
	}
}
func TestCacheDirtyDataProtectedAndConditionalUpload(t *testing.T) {
	s, id := testService(t)
	ctx := context.Background()
	b := Backend{s, id}
	e, err := b.Put(ctx, "edit.txt", strings.NewReader("first"), 5, storage.Condition{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := s.FetchCache(ctx, id, "edit.txt", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cached.LocalPath, []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveCache(cached.ID); err == nil {
		t.Fatal("dirty cache was removed")
	}
	if _, err = b.Put(ctx, "edit.txt", strings.NewReader("remote edit"), 11, storage.Condition{IfMatch: e.ETag}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UploadCache(ctx, cached.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected remote conflict, got %v", err)
	}
	content, err := os.ReadFile(cached.LocalPath)
	if err != nil || string(content) != "local edit" {
		t.Fatalf("cache lost edit %q %v", content, err)
	}
	again, err := s.FetchCache(ctx, id, "edit.txt", true, false)
	if err != nil || !again.Dirty || again.ID != cached.ID {
		t.Fatalf("dirty fetch %v %v", again, err)
	}
}
func TestConfigExportStripsLocalRootsAndCredentials(t *testing.T) {
	s, _ := testService(t)
	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, Connection{Config: storage.Config{ID: "origin", Kind: "s3", Name: "S3", Bucket: "bucket", Endpoint: "https://example.test"}})
	s.cfg.Jobs = append(s.cfg.Jobs, Job{ID: "job", ConnectionID: "origin", LocalPath: "/secret/local/path", Direction: "both", Enabled: true})
	s.mu.Unlock()
	export, err := s.ExportConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret/local") || len(export.Connections) != 1 || export.Jobs[0].Enabled {
		t.Fatalf("unsafe export: %s", raw)
	}
	counts, err := s.ImportConfiguration(export)
	if err != nil || counts["jobs"] != 1 {
		t.Fatal(counts, err)
	}
	s.mu.Lock()
	last := s.cfg.Jobs[len(s.cfg.Jobs)-1]
	s.mu.Unlock()
	if last.LocalPath != "" || last.Enabled || last.ID == "job" {
		t.Fatalf("unsafe import %+v", last)
	}
	diag, _ := json.Marshal(s.Diagnostics())
	if bytes.Contains(diag, []byte("example.test")) || bytes.Contains(diag, []byte("bucket")) {
		t.Fatalf("diagnostics includes remote identity: %s", diag)
	}
}
func TestEncryptedVaultRejectsWrongMasterKeyAndTampering(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	v, err := NewEncryptedVault(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Set("connection:demo", "sensitive-value"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(v.filename("connection:demo"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("sensitive-value")) {
		t.Fatal("plaintext secret on disk")
	}
	got, err := v.Get("connection:demo")
	if err != nil || got != "sensitive-value" {
		t.Fatal(got, err)
	}
	wrong, _ := NewEncryptedVault(dir, bytes.Repeat([]byte{8}, 32))
	if _, err = wrong.Get("connection:demo"); err == nil {
		t.Fatal("wrong key accepted")
	}
	raw[len(raw)-1] ^= 1
	if err = os.WriteFile(v.filename("connection:demo"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Get("connection:demo"); err == nil {
		t.Fatal("tampering accepted")
	}
}
func TestBandwidthCancellationAndReadAdmission(t *testing.T) {
	s, id := testService(t)
	p := s.Preferences()
	p.MaxReaders = 1
	p.BandwidthBytes = 1
	if err := s.SetPreferences(p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, _, err := (Backend{s, id}).Open(ctx, "欢迎使用小花鼠.md", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, _, err = (Backend{s, id}).Open(ctx, "欢迎使用小花鼠.md", ""); err == nil {
		t.Fatal("reader quota exceeded")
	}
	cancel()
	if _, err = r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read didn't cancel: %v", err)
	}
}

type resumableFixture struct {
	storage.Store
	parts     []storage.MultipartPart
	data      map[int32][]byte
	failed    bool
	completed int
}

func (f *resumableFixture) CreateMultipart(context.Context, string) (string, error) {
	return "fixture-upload", nil
}
func (f *resumableFixture) UploadPart(_ context.Context, _, _ string, n int32, body io.Reader, size int64) (storage.MultipartPart, error) {
	if n == 2 && !f.failed {
		f.failed = true
		return storage.MultipartPart{}, errors.New("connection interrupted")
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return storage.MultipartPart{}, err
	}
	f.data[n] = content
	p := storage.MultipartPart{Number: n, Size: size, ETag: strings.Repeat("x", int(n))}
	if int(n) > len(f.parts) {
		f.parts = append(f.parts, p)
	} else {
		f.parts[n-1] = p
	}
	return p, nil
}
func (f *resumableFixture) ListParts(context.Context, string, string) ([]storage.MultipartPart, error) {
	return append([]storage.MultipartPart{}, f.parts...), nil
}
func (f *resumableFixture) CompleteMultipart(ctx context.Context, key, _ string, parts []storage.MultipartPart, cond storage.Condition) (storage.Entry, error) {
	var all bytes.Buffer
	for _, p := range parts {
		all.Write(f.data[p.Number])
	}
	f.completed++
	return f.Store.Put(ctx, key, bytes.NewReader(all.Bytes()), int64(all.Len()), cond)
}
func (f *resumableFixture) AbortMultipart(context.Context, string, string) error { return nil }
func TestMultipartInterruptedThenResumeVerifiedParts(t *testing.T) {
	s, id := testService(t)
	s.mu.Lock()
	mp := &resumableFixture{Store: s.stores[id], data: map[int32][]byte{}}
	s.stores[id] = mp
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == id {
			s.cfg.Connections[i].Capabilities.MultipartConditional = true
		}
	}
	s.mu.Unlock()
	content := strings.Repeat("m", int(2*multipartPartSize))
	_, err := (Backend{s, id}).Put(context.Background(), "large.dat", strings.NewReader(content), int64(len(content)), storage.Condition{IfNoneMatch: true})
	if err == nil {
		t.Fatal("interruption ignored")
	}
	uploads, err := s.MultipartUploads()
	if err != nil || len(uploads) != 1 {
		t.Fatal(uploads, err)
	}
	if len(uploads[0].Parts) != 1 || mp.completed != 0 {
		t.Fatalf("invalid checkpoint %+v", uploads[0])
	}
	if _, err = s.ResumeUpload(context.Background(), uploads[0].OperationID); err != nil {
		t.Fatal(err)
	}
	if mp.completed != 1 {
		t.Fatal("unexpected completion replay")
	}
	if directorySize(filepath.Join(s.dir, "staging")) != 0 {
		t.Fatal("completed staging leaked")
	}
	if readBody(t, Backend{s, id}, "large.dat") != content {
		t.Fatal("resumed contents differ")
	}
}

type interruptedDownloadStore struct {
	storage.Store
	interrupted bool
}

func (s *interruptedDownloadStore) Open(ctx context.Context, key, etag string) (io.ReadCloser, storage.Entry, error) {
	r, e, err := s.Store.Open(ctx, key, etag)
	if err != nil {
		return nil, e, err
	}
	if !s.interrupted {
		s.interrupted = true
		return &cutoffReadCloser{source: r, remaining: (4 << 20) + 123}, e, nil
	}
	return r, e, nil
}
func (s *interruptedDownloadStore) OpenRange(ctx context.Context, key, etag string, start, length int64) (io.ReadCloser, storage.Entry, error) {
	return s.Store.(storage.RangeStore).OpenRange(ctx, key, etag, start, length)
}

type cutoffReadCloser struct {
	source    io.ReadCloser
	remaining int64
}

func (r *cutoffReadCloser) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("network disconnected")
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	return n, err
}
func (r *cutoffReadCloser) Close() error { return r.source.Close() }
func TestDownloadSurvivesRestartAndUsesStableRange(t *testing.T) {
	dir := t.TempDir()
	vault := &memoryVault{m: map[string]string{}}
	s, err := New(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Demo()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	content := strings.Repeat("range", (2 << 20))
	b := Backend{s, id}
	if _, err = b.Put(ctx, "download.dat", strings.NewReader(content), int64(len(content)), storage.Condition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	fixture := &interruptedDownloadStore{Store: s.stores[id]}
	s.stores[id] = fixture
	conn := s.cfg.Connections[0]
	s.mu.Unlock()
	destination := filepath.Join(t.TempDir(), "saved.dat")
	d, err := s.StartDownload(ctx, id, "download.dat", destination)
	if err == nil || d.Received == 0 || d.Received >= d.Size {
		t.Fatalf("no partial checkpoint %+v %v", d, err)
	}
	if _, err = os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial download published")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopened.mu.Lock()
	reopened.cfg.Connections = append(reopened.cfg.Connections, conn)
	reopened.stores[id] = fixture
	reopened.mu.Unlock()
	if _, err = os.Stat(d.Staging); err != nil {
		t.Fatalf("restart erased active checkpoint: %v", err)
	}
	completed, err := reopened.ResumeDownload(ctx, d.ID)
	if err != nil || completed.State != "done" {
		t.Fatal(completed, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != content {
		t.Fatal("resumed download corrupt", err)
	}
}
func TestDeepDAVLockAcrossEquivalentConnections(t *testing.T) {
	s, id := testService(t)
	s.mu.Lock()
	s.cfg.Connections[0].Kind = "s3"
	s.cfg.Connections[0].Endpoint = "https://store.test"
	s.cfg.Connections[0].Bucket = "files"
	alias := s.cfg.Connections[0]
	alias.ID = "alias"
	s.cfg.Connections = append(s.cfg.Connections, alias)
	s.stores[alias.ID] = s.stores[id]
	s.mu.Unlock()
	b := Backend{s, id}
	ctx := context.Background()
	if _, _, err := b.AcquireDAVLock(ctx, "Documents", "editor", true, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := (Backend{s, alias.ID}).Put(ctx, "Documents/new.txt", strings.NewReader("x"), 1, storage.Condition{IfNoneMatch: true}); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("alias bypassed DAV lock: %v", err)
	}
}
