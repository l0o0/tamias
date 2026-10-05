package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	_ "github.com/mattn/go-sqlite3"
	"github.com/zalando/go-keyring"
	"tamiops/internal/gateway"
	"tamiops/internal/storage"
)

// Version is set by the release build using -ldflags -X.
var Version = "0.2.0-dev"

const StagingLimit int64 = 1 << 30
const RecoveryLimit int64 = 1 << 30
const FileLimit int64 = 128 << 20

type Vault interface {
	Get(string) (string, error)
	Set(string, string) error
	Delete(string) error
}
type SystemVault struct{}

func (SystemVault) Get(k string) (string, error) { return keyring.Get("io.tamiops.tami", k) }
func (SystemVault) Set(k, v string) error        { return keyring.Set("io.tamiops.tami", k, v) }
func (SystemVault) Delete(k string) error        { return keyring.Delete("io.tamiops.tami", k) }

type Connection struct {
	storage.Config
	Tested                bool                 `json:"tested"`
	Capabilities          storage.Capabilities `json:"capabilities"`
	CompatibilityProfile  string               `json:"compatibilityProfile,omitempty"`
	CapabilityVersion     int                  `json:"capabilityVersion,omitempty"`
	CapabilitiesCheckedAt string               `json:"capabilitiesCheckedAt,omitempty"`
	Detecting             bool                 `json:"detecting,omitempty"`
	Error                 string               `json:"error,omitempty"`
	WriteRestriction      string               `json:"writeRestriction,omitempty"`
}
type Job struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Icon            string   `json:"icon,omitempty"`
	ConnectionID    string   `json:"connectionId"`
	LocalPath       string   `json:"localPath"`
	RemotePath      string   `json:"remotePath"`
	Direction       string   `json:"direction"`
	Status          string   `json:"status"`
	ActiveAction    string   `json:"activeAction,omitempty"`
	LastRun         string   `json:"lastRun"`
	LastScanAt      string   `json:"lastScanAt,omitempty"`
	LastScanSummary string   `json:"lastScanSummary,omitempty"`
	Detail          string   `json:"detail"`
	Enabled         bool     `json:"enabled"`
	Exclude         []string `json:"exclude"`
	ScheduleMinutes int      `json:"scheduleMinutes"`
	Watch           bool     `json:"watch"`
	DeleteThreshold int      `json:"deleteThreshold"`
	Progress        int      `json:"progress"`
	QueueTotal      int      `json:"queueTotal"`
	QueueDone       int      `json:"queueDone"`
}
type Gateway struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	ConnectionID string              `json:"connectionId"`
	Prefix       string              `json:"prefix"`
	Port         int                 `json:"port"`
	ListenHost   string              `json:"listenHost"`
	TLSCert      string              `json:"tlsCert"`
	TLSKey       string              `json:"tlsKey"`
	Access       gateway.AccessStats `json:"access"`
	Username     string              `json:"username"`
	ReadOnly     bool                `json:"readOnly"`
	Running      bool                `json:"running"`
	URL          string              `json:"url"`
}
type Activity struct {
	ID      int64  `json:"id"`
	Time    string `json:"time"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Path    string `json:"path"`
}
type config struct {
	Preferences Preferences  `json:"preferences"`
	Connections []Connection `json:"connections"`
	Jobs        []Job        `json:"jobs"`
	Gateways    []Gateway    `json:"gateways"`
}
type Service struct {
	stagingMu                  sync.Mutex
	stagingReservations        map[string]int64
	stagingWritten             map[string]int64
	closeOnce                  sync.Once
	closeError                 error
	backgroundCancels          map[string]context.CancelFunc
	connectionDetectionPending map[string]bool
	connectionDetectionStarted bool
	tasks                      sync.WaitGroup
	maintenanceCancel          context.CancelFunc
	maintenanceDone            chan struct{}
	downloadCancels            map[string]context.CancelFunc
	RequestNotifications       func() (bool, error)
	gatewayStats               map[string]interface{ Stats() gateway.AccessStats }
	cacheMu                    sync.Mutex
	schedulerWake              chan struct{}
	schedulerChanged           chan struct{}
	OpenLocal                  func(string) error
	Notify                     func(string, string)
	activeReaders              int
	readerChanged              chan struct{}
	transferMu                 sync.Mutex
	nextTransfer               time.Time
	SetAutoStart               func(bool) error
	mu                         sync.Mutex
	writes                     sync.Mutex // A single bounded writer also coordinates aliases and parent/child paths.
	db                         *sql.DB
	lock                       *flock.Flock
	vault                      Vault
	dir                        string
	cfg                        config
	stores                     map[string]storage.Store
	servers                    map[string]*http.Server
	passwords                  map[string]string
	plans                      map[string]Plan
	schedulerCancel            context.CancelFunc
	schedulerDone              chan struct{}
	jobCancels                 map[string]context.CancelFunc
	jobLocks                   map[string]*sync.Mutex
	closing                    bool
	PickFolder                 func() (string, error)
	PickSave                   func(string) (string, error)
}

func New(dir string, vault Vault) (*Service, error) {
	if vault == nil {
		vault = SystemVault{}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return nil, err
	}
	dir = canonical
	lock := flock.New(filepath.Join(dir, "instance.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("此数据目录已由另一个小花鼠实例使用")
	}
	failed := true
	defer func() {
		if failed {
			_ = lock.Unlock()
		}
	}()
	for _, p := range []string{"staging", "recovery", "cache"} {
		if err = os.MkdirAll(filepath.Join(dir, p), 0700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "state.db")+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, data TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS activity (id INTEGER PRIMARY KEY AUTOINCREMENT,time TEXT,kind TEXT,status TEXT,message TEXT,path TEXT);
 CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY,connection TEXT,path TEXT,kind TEXT,state TEXT,staging TEXT,receipt TEXT,created TEXT);
 CREATE TABLE IF NOT EXISTS baseline (job TEXT,path TEXT,local_hash TEXT,remote_etag TEXT,PRIMARY KEY(job,path));
 CREATE TABLE IF NOT EXISTS sync_queue (job TEXT,path TEXT,kind TEXT,local_hash TEXT,remote_etag TEXT,state TEXT,bytes_done INTEGER,error TEXT,updated TEXT,PRIMARY KEY(job,path,kind));
 CREATE TABLE IF NOT EXISTS sync_plans (job TEXT PRIMARY KEY,token TEXT,data TEXT,created TEXT);
 CREATE TABLE IF NOT EXISTS sync_dirty (job TEXT PRIMARY KEY,token TEXT NOT NULL,updated TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS operation_resources (operation_id TEXT,connection TEXT,path TEXT,PRIMARY KEY(operation_id,connection,path));
 CREATE TABLE IF NOT EXISTS dav_locks (token TEXT PRIMARY KEY,connection TEXT,path TEXT,owner TEXT,depth INTEGER,expires INTEGER);
 CREATE TABLE IF NOT EXISTS backup_jobs(id TEXT PRIMARY KEY,name TEXT,connection TEXT,local_path TEXT,remote_prefix TEXT,exclude_json TEXT,schedule_minutes INTEGER,retain_recent INTEGER,retain_daily INTEGER,retain_monthly INTEGER,enabled INTEGER,last_run TEXT,status TEXT,detail TEXT,created TEXT);
 CREATE TABLE IF NOT EXISTS backup_snapshots(id TEXT PRIMARY KEY,job_id TEXT,started TEXT,completed TEXT,status TEXT,files_total INTEGER,files_done INTEGER,files_failed INTEGER,bytes_total INTEGER,manifest_version INTEGER,error TEXT);
 CREATE TABLE IF NOT EXISTS backup_items(snapshot_id TEXT,path TEXT,sha256 TEXT,size INTEGER,remote_etag TEXT,state TEXT,error TEXT,modified TEXT,PRIMARY KEY(snapshot_id,path));
 CREATE TABLE IF NOT EXISTS migration_jobs(id TEXT PRIMARY KEY,name TEXT,source_connection TEXT,source_prefix TEXT,target_connection TEXT,target_prefix TEXT,overwrite INTEGER,created TEXT);
 CREATE TABLE IF NOT EXISTS migration_runs(id TEXT PRIMARY KEY,job_id TEXT,state TEXT,preview_token TEXT,started TEXT,updated TEXT,files_total INTEGER,files_done INTEGER,files_failed INTEGER,cancel_requested INTEGER,detail TEXT);
 CREATE TABLE IF NOT EXISTS migration_items(run_id TEXT,source_path TEXT,target_path TEXT,source_etag TEXT,source_size INTEGER,source_hash TEXT,dest_before_etag TEXT,dest_etag TEXT,state TEXT,error TEXT,PRIMARY KEY(run_id,source_path));
 CREATE TABLE IF NOT EXISTS downloads(id TEXT PRIMARY KEY,connection TEXT,path TEXT,etag TEXT,destination TEXT,staging TEXT,size INTEGER,received INTEGER,hash TEXT,state TEXT,updated TEXT);
 CREATE TABLE IF NOT EXISTS multipart_uploads (operation_id TEXT PRIMARY KEY,connection TEXT,path TEXT,hash TEXT,size INTEGER,state TEXT,data TEXT);
 CREATE TABLE IF NOT EXISTS cache_entries (id TEXT PRIMARY KEY,connection TEXT,path TEXT,etag TEXT,hash TEXT,size INTEGER,pinned INTEGER DEFAULT 0,updated TEXT,UNIQUE(connection,path));
 CREATE TABLE IF NOT EXISTS gateway_access(id INTEGER PRIMARY KEY AUTOINCREMENT,gateway TEXT,time TEXT,method TEXT,path TEXT,status INTEGER,bytes_in INTEGER,bytes_out INTEGER,milliseconds INTEGER);
 CREATE TABLE IF NOT EXISTS statistics (scope TEXT,id TEXT,requests INTEGER DEFAULT 0,uploaded INTEGER DEFAULT 0,downloaded INTEGER DEFAULT 0,errors INTEGER DEFAULT 0,last_access TEXT,PRIMARY KEY(scope,id));`)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Service{db: db, lock: lock, vault: vault, dir: dir, stores: map[string]storage.Store{}, servers: map[string]*http.Server{}, passwords: map[string]string{}, readerChanged: make(chan struct{}), gatewayStats: map[string]interface{ Stats() gateway.AccessStats }{}, downloadCancels: map[string]context.CancelFunc{}, backgroundCancels: map[string]context.CancelFunc{}, connectionDetectionPending: map[string]bool{}, stagingReservations: map[string]int64{}, stagingWritten: map[string]int64{}, plans: map[string]Plan{}, jobCancels: map[string]context.CancelFunc{}, jobLocks: map[string]*sync.Mutex{}}
	for _, column := range []struct{ table, name, definition string }{{"cache_entries", "remote_changed", "INTEGER NOT NULL DEFAULT 0"}, {"cache_entries", "offline", "INTEGER NOT NULL DEFAULT 0"}, {"cache_entries", "checked", "TEXT NOT NULL DEFAULT ''"}, {"migration_jobs", "enabled", "INTEGER NOT NULL DEFAULT 1"}, {"migration_items", "dest_absent", "INTEGER NOT NULL DEFAULT 0"}, {"migration_jobs", "deleted", "INTEGER NOT NULL DEFAULT 0"}} {
		if err = s.ensureColumn(column.table, column.name, column.definition); err != nil {
			db.Close()
			return nil, err
		}
	}
	var raw string
	err = db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &s.cfg)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	if s.cfg.Connections == nil {
		s.cfg.Connections = []Connection{}
	}
	if s.cfg.Jobs == nil {
		s.cfg.Jobs = []Job{}
	}
	if s.cfg.Gateways == nil {
		s.cfg.Gateways = []Gateway{}
	}
	for i := range s.cfg.Connections {
		s.cfg.Connections[i].Detecting = false
		if !connectionDetectionCurrent(s.cfg.Connections[i]) {
			s.cfg.Connections[i].Tested = false
			s.cfg.Connections[i].Capabilities = storage.Capabilities{}
		}
	}
	for i := range s.cfg.Gateways {
		s.cfg.Gateways[i].Running = false
	}
	for i := range s.cfg.Jobs {
		// This is runtime-only state. A previous process may have exited while
		// an action was active, so never expose that stale transfer direction.
		s.cfg.Jobs[i].ActiveAction = ""
		if s.cfg.Jobs[i].Status == "running" {
			s.cfg.Jobs[i].Status = "paused"
			s.cfg.Jobs[i].Detail = "上次执行中断，请重新预览"
		}
	}
	if _, err = db.Exec("UPDATE operations SET state='uncertain' WHERE state='committing'"); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.reconcileTemporaryFiles(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("UPDATE sync_queue SET state='pending' WHERE state='running'; UPDATE downloads SET state='paused' WHERE state='running'; UPDATE migration_runs SET state='paused',detail='上次执行中断，请重试以重新核对' WHERE state IN ('running','queued','cancel_requested'); UPDATE backup_snapshots SET status='partial',error='上次备份中断，远端清单可用于核对' WHERE status='running'; UPDATE backup_jobs SET status='paused' WHERE status='running'"); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.recoverInterruptedSyncScans(); err != nil {
		db.Close()
		return nil, err
	}
	failed = false
	return s, nil
}
func (s *Service) Close() error {
	s.closeOnce.Do(func() { s.closeError = s.closeService() })
	return s.closeError
}
func (s *Service) closeService() error {
	s.mu.Lock()
	s.closing = true
	for _, cancel := range s.backgroundCancels {
		cancel()
	}
	for _, cancel := range s.downloadCancels {
		cancel()
	}
	s.mu.Unlock()
	s.StopScheduler()
	s.stopMaintenance()
	s.mu.Lock()
	servers := make([]*http.Server, 0, len(s.servers))
	for _, srv := range s.servers {
		servers = append(servers, srv)
	}
	s.mu.Unlock()
	for _, srv := range servers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
		}
		cancel()
	}
	s.tasks.Wait()
	s.writes.Lock()
	defer s.writes.Unlock()
	err := s.db.Close()
	_ = s.lock.Unlock()
	return err
}
func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (s *Service) durableJSONLocked() ([]byte, error) {
	// Sandbox resources and credentials live only in this process.
	durable := config{Preferences: s.cfg.Preferences, Connections: []Connection{}, Jobs: []Job{}, Gateways: []Gateway{}}
	keep := map[string]bool{}
	for _, c := range s.cfg.Connections {
		if c.Kind != "demo" {
			c.Detecting = false
			durable.Connections = append(durable.Connections, c)
			keep[c.ID] = true
		}
	}
	for _, j := range s.cfg.Jobs {
		if keep[j.ConnectionID] {
			durable.Jobs = append(durable.Jobs, j)
		}
	}
	for _, g := range s.cfg.Gateways {
		if keep[g.ConnectionID] {
			durable.Gateways = append(durable.Gateways, g)
		}
	}
	return json.Marshal(durable)
}
func (s *Service) saveLocked() error {
	b, err := s.durableJSONLocked()
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(b))
	return err
}
func (s *Service) activity(kind, status, msg, key string) {
	if s.Notify != nil && (status == "error" || status == "conflict") {
		go s.Notify("小花鼠 · 需要处理", msg)
	}
	_, _ = s.db.Exec("INSERT INTO activity(time,kind,status,message,path) VALUES(?,?,?,?,?)", time.Now().Format(time.RFC3339), kind, status, msg, key)
	_, _ = s.db.Exec("DELETE FROM activity WHERE id NOT IN (SELECT id FROM activity ORDER BY id DESC LIMIT 500)")
}
func (s *Service) Snapshot() map[string]any {
	s.mu.Lock()
	c := append([]Connection{}, s.cfg.Connections...)
	j := append([]Job{}, s.cfg.Jobs...)
	g := append([]Gateway{}, s.cfg.Gateways...)
	for i := range c {
		c[i].Detecting = s.connectionDetectionPending[c[i].ID]
	}
	for i := range g {
		if stats := s.gatewayStats[g[i].ID]; stats != nil {
			g[i].Access = stats.Stats()
		}
	}
	s.mu.Unlock()
	a := []Activity{}
	rows, err := s.db.Query("SELECT id,time,kind,status,message,path FROM activity ORDER BY id DESC LIMIT 100")
	if err == nil {
		for rows.Next() {
			var v Activity
			if rows.Scan(&v.ID, &v.Time, &v.Kind, &v.Status, &v.Message, &v.Path) == nil {
				a = append(a, v)
			}
		}
		rows.Close()
	}
	return map[string]any{"preferences": s.Preferences(), "version": Version, "connections": c, "jobs": j, "gateways": g, "activities": a, "dataDir": s.dir, "disk": map[string]any{"stagingBytes": directorySize(filepath.Join(s.dir, "staging")), "stagingLimit": s.Preferences().StagingBytes, "recoveryBytes": directorySize(filepath.Join(s.dir, "recovery")), "recoveryLimit": s.Preferences().RecoveryBytes, "cacheBytes": directorySize(filepath.Join(s.dir, "cache")), "cacheLimit": s.Preferences().CacheBytes, "freeBytes": freeBytes(s.dir)}}
}
func (s *Service) connection(id string) (Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cfg.Connections {
		if c.ID == id {
			return c, nil
		}
	}
	return Connection{}, storage.ErrNotFound
}
func (s *Service) store(id string) (storage.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errors.New("应用正在退出")
	}
	if st := s.stores[id]; st != nil {
		return st, nil
	}
	var c *Connection
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == id {
			c = &s.cfg.Connections[i]
			break
		}
	}
	if c == nil {
		return nil, storage.ErrNotFound
	}
	raw, err := s.vault.Get("connection:" + id)
	if err != nil {
		return nil, errors.New("无法读取系统凭据库，请重新添加连接")
	}
	var creds storage.Credentials
	if json.Unmarshal([]byte(raw), &creds) != nil {
		return nil, errors.New("凭据格式无效")
	}
	st, err := storage.New(c.Config, creds)
	if err != nil {
		return nil, err
	}
	s.stores[id] = st
	return st, nil
}

type ConnectionInput struct {
	storage.Config
	Password     string `json:"password"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	SessionToken string `json:"sessionToken"`
}

func (s *Service) AddConnection(ctx context.Context, in ConnectionInput) (Connection, error) {
	in.ID = ID()
	in.Name = strings.TrimSpace(in.Name)
	writeMode, err := storage.NormalizeWriteMode(in.WriteMode)
	if err != nil {
		return Connection{}, err
	}
	in.WriteMode = writeMode
	if in.Name == "" {
		return Connection{}, errors.New("请填写连接名称")
	}
	if in.Kind != "s3" && in.Kind != "webdav" {
		return Connection{}, errors.New("请选择 S3 或 WebDAV")
	}
	creds := storage.Credentials{Username: in.Username, Password: in.Password, AccessKey: in.AccessKey, SecretKey: in.SecretKey, SessionToken: in.SessionToken}
	st, err := storage.New(in.Config, creds)
	if err != nil {
		return Connection{}, err
	}
	detection, detectErr := detectConnectionCapabilities(ctx, st, in.WriteMode, true)
	if detectErr != nil {
		return Connection{}, detectErr
	}
	c := Connection{Config: in.Config}
	applyConnectionDetection(&c, detection)
	raw, _ := json.Marshal(creds)
	if err = s.vault.Set("connection:"+in.ID, string(raw)); err != nil {
		return Connection{}, errors.New("系统凭据库保存失败，连接未保存")
	}
	s.mu.Lock()
	s.cfg.Connections = append(s.cfg.Connections, c)
	s.stores[c.ID] = st
	err = s.saveLocked()
	if err != nil {
		s.cfg.Connections = s.cfg.Connections[:len(s.cfg.Connections)-1]
		delete(s.stores, c.ID)
		_ = s.vault.Delete("connection:" + c.ID)
	}
	s.mu.Unlock()
	if err != nil {
		return Connection{}, err
	}
	s.activity("connection", "success", "已连接 "+c.Name, "")
	return c, nil
}
func (s *Service) TestConnection(ctx context.Context, id string, write bool) (storage.Capabilities, error) {
	s.writes.Lock()
	connection, err := s.connection(id)
	if err != nil {
		s.writes.Unlock()
		return storage.Capabilities{}, err
	}
	var st storage.Store
	var credentials storage.Credentials
	if connection.Kind == "demo" {
		st, err = s.store(id)
	} else {
		credentials, _, err = loadConnectionCredentials(s, id)
		if err == nil {
			st, err = storage.New(connection.Config, credentials)
		}
	}
	s.writes.Unlock()
	if err != nil {
		return storage.Capabilities{}, err
	}
	probeWrite := write && connection.WriteMode != storage.WriteModeCopy
	detection, detectErr := detectConnectionCapabilities(ctx, st, connection.WriteMode, probeWrite)
	caps := detection.Capabilities
	if errors.Is(detectErr, context.Canceled) || errors.Is(detectErr, context.DeadlineExceeded) {
		return caps, detectErr
	}
	var testErr error
	var connectionErr error
	readVerified := detectErr == nil
	writeVerified := probeWrite && readVerified && detection.CompatibilityProfile == connectionProfileConditional
	writeRestriction := detection.WriteRestriction
	if detectErr != nil {
		testErr = errors.New("连接异常：只读列表测试失败；请检查网络、地址与凭据。")
		connectionErr = testErr
	} else if probeWrite && !writeVerified {
		if detection.ProbeUnsupported && ordinaryWriteMode(connection) {
			// Unsupported atomic conditions remain a recorded limit while normal
			// same-path uploads keep their checked compatibility path.
			testErr = nil
		} else if detection.ProbeUnsupported {
			testErr = fmt.Errorf("连接可读取，但%w", storage.ErrConditionalUnsupported)
			if detection.ProbeWarning != "" {
				testErr = fmt.Errorf("%w：%s", testErr, detection.ProbeWarning)
			}
		} else if ordinaryWriteMode(connection) {
			testErr = errors.New("连接可读取，但写入验证未完成；请检查存储权限或稍后重试。")
		} else {
			testErr = errors.New("连接可读取，但读写能力验证未通过；远端写入和删除已禁用。")
		}
	}

	// Probing can perform remote writes and may route back through a gateway
	// served by this Service. Reacquire the writer only to validate the
	// snapshot and commit its result.
	s.writes.Lock()
	defer s.writes.Unlock()
	current, lookupErr := s.connection(id)
	if lookupErr != nil {
		return caps, lookupErr
	}
	if current.Config != connection.Config {
		return caps, storage.ErrConflict
	}
	if connection.Kind != "demo" {
		currentCredentials, _, credentialErr := loadConnectionCredentials(s, id)
		if credentialErr != nil {
			return caps, credentialErr
		}
		if currentCredentials != credentials {
			return caps, storage.ErrConflict
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return caps, storage.ErrNotFound
	}
	c := &s.cfg.Connections[index]
	if c.Config != connection.Config {
		return caps, storage.ErrConflict
	}
	c.Tested = readVerified
	c.Capabilities = caps
	c.CompatibilityProfile = detection.CompatibilityProfile
	c.CapabilityVersion = detection.CapabilityVersion
	c.CapabilitiesCheckedAt = detection.CapabilitiesCheckedAt
	if probeWrite && writeVerified {
		c.WriteRestriction = ""
	} else if probeWrite && writeRestriction != "" {
		c.WriteRestriction = writeRestriction
	}
	c.Error = ""
	if connectionErr != nil {
		c.Error = connectionErr.Error()
	}
	saveErr := s.saveLocked()
	if testErr != nil {
		return caps, testErr
	}
	return caps, saveErr
}
func (s *Service) Demo() (string, error) {
	s.mu.Lock()
	for _, c := range s.cfg.Connections {
		if c.Kind == "demo" {
			if s.stores[c.ID] != nil {
				s.mu.Unlock()
				return c.ID, nil
			}
		}
	}
	s.mu.Unlock()
	st := storage.NewMemory()
	ctx := context.Background()
	_ = st.Mkdir(ctx, "Documents")
	for k, v := range map[string]string{"欢迎使用小花鼠.md": "# 欢迎使用小花鼠\n\n这是隔离演示空间，不会连接你的真实存储。\n可以上传文件、试用 WebDAV 网关，并创建同步任务。\n演示远端内容在退出后清空。\n", "Documents/项目笔记.md": "# 项目笔记\n\n连接你自己的存储，让文件自由流动。\n"} {
		_, _ = st.Put(ctx, k, strings.NewReader(v), int64(len(v)), storage.Condition{IfNoneMatch: true})
	}
	c := Connection{Config: storage.Config{ID: ID(), Name: "演示空间", Kind: "demo", Endpoint: "仅内存 · 退出后清空"}, Tested: true, Capabilities: storage.Capabilities{ConditionalWrite: true, ConditionalDelete: true, RangeRead: true}}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Connections = append(s.cfg.Connections, c)
	s.stores[c.ID] = st
	// Demo is intentionally excluded from the durable configuration.
	s.activity("connection", "success", "已创建隔离演示空间；内容在退出后清空", "")
	return c.ID, nil
}
func directorySize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			if i, e := d.Info(); e == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}
func validKey(k string) error {
	if strings.HasPrefix(k, "/") || strings.ContainsAny(k, "\\\x00") {
		return storage.ErrInvalidPath
	}
	for _, p := range strings.Split(k, "/") {
		if p == ".." || p == "." {
			return storage.ErrInvalidPath
		}
	}
	return nil
}
