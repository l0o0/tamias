package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"time"
)

var preferencesUpdateMu sync.Mutex

type unavailableCredentialStore struct{}

func (unavailableCredentialStore) unavailable() error {
	return errors.New("连接已禁用：凭据回滚失败，请重新输入并测试凭据")
}
func (s unavailableCredentialStore) List(context.Context, string) ([]storage.Entry, error) {
	return nil, s.unavailable()
}
func (s unavailableCredentialStore) Stat(context.Context, string) (storage.Entry, error) {
	return storage.Entry{}, s.unavailable()
}
func (s unavailableCredentialStore) Open(context.Context, string, string) (io.ReadCloser, storage.Entry, error) {
	return nil, storage.Entry{}, s.unavailable()
}
func (s unavailableCredentialStore) Put(context.Context, string, io.ReadSeeker, int64, storage.Condition) (storage.Entry, error) {
	return storage.Entry{}, s.unavailable()
}
func (s unavailableCredentialStore) Delete(context.Context, string, storage.Condition) error {
	return s.unavailable()
}
func (s unavailableCredentialStore) Mkdir(context.Context, string) error { return s.unavailable() }

type Preferences struct {
	Rules          ExecutionRules `json:"rules"`
	BandwidthBytes int64          `json:"bandwidthBytes"`
	MaxReaders     int            `json:"maxReaders"`
	StagingBytes   int64          `json:"stagingBytes"`
	RecoveryBytes  int64          `json:"recoveryBytes"`
	CacheBytes     int64          `json:"cacheBytes"`
	MaxFileBytes   int64          `json:"maxFileBytes"`
	RecoveryDays   int            `json:"recoveryDays"`
	AutoStart      bool           `json:"autoStart"`
	Notifications  bool           `json:"notifications"`
}

func defaults(p Preferences) Preferences {
	if p.MaxReaders == 0 {
		p.MaxReaders = 4
	}
	if p.StagingBytes == 0 {
		p.StagingBytes = StagingLimit
	}
	if p.RecoveryBytes == 0 {
		p.RecoveryBytes = RecoveryLimit
	}
	if p.CacheBytes == 0 {
		p.CacheBytes = 512 << 20
	}
	if p.MaxFileBytes == 0 {
		p.MaxFileBytes = FileLimit
	}
	if p.RecoveryDays == 0 {
		p.RecoveryDays = 30
	}
	return p
}
func (s *Service) Preferences() Preferences {
	s.mu.Lock()
	defer s.mu.Unlock()
	return defaults(s.cfg.Preferences)
}
func validatePreferences(p Preferences) error {
	if err := ValidateExecutionRules(p.Rules); err != nil {
		return err
	}
	if p.BandwidthBytes < 0 || p.BandwidthBytes > 1<<40 || p.MaxReaders < 1 || p.MaxReaders > 16 || p.StagingBytes < 16<<20 || p.StagingBytes > 32<<30 || p.RecoveryBytes < 16<<20 || p.RecoveryBytes > 32<<30 || p.CacheBytes < 16<<20 || p.CacheBytes > 32<<30 || p.MaxFileBytes < 1<<20 || p.MaxFileBytes > p.StagingBytes || p.RecoveryDays < 1 || p.RecoveryDays > 3650 {
		return errors.New("设置超出范围：并发 1–16，额度 16 MiB–32 GiB，单文件上限不得超过暂存额度")
	}
	return nil
}
func (s *Service) SetPreferences(p Preferences) error {
	if err := validatePreferences(p); err != nil {
		return err
	}
	preferencesUpdateMu.Lock()
	defer preferencesUpdateMu.Unlock()
	checkBudget := func(old Preferences) error {
		if p.StagingBytes < directorySize(filepath.Join(s.dir, "staging"))+s.stagingReservedLocked() || p.RecoveryBytes < directorySize(filepath.Join(s.dir, "recovery")) {
			return errors.New("额度不能低于现有受保护数据，请先检查恢复与未决操作")
		}
		if len(s.stagingReservations) > 0 && p.MaxFileBytes != old.MaxFileBytes {
			return errors.New("暂存接收中，请完成后调整单文件上限")
		}
		return nil
	}
	s.writes.Lock()
	s.stagingMu.Lock()
	old := s.Preferences()
	if err := checkBudget(old); err != nil {
		s.stagingMu.Unlock()
		s.writes.Unlock()
		return err
	}
	s.stagingMu.Unlock()
	s.writes.Unlock()
	needNotifications := p.Notifications && !old.Notifications
	needAutoStart := p.AutoStart != old.AutoStart
	if needNotifications && s.RequestNotifications == nil {
		return errors.New("当前宿主不支持系统通知")
	}
	if needAutoStart && s.SetAutoStart == nil {
		return errors.New("当前宿主不支持开机启动设置")
	}
	notificationsAuthorized := false
	noteNotificationAuthorization := func(err error) error {
		if notificationsAuthorized {
			return fmt.Errorf("%w；系统通知授权可能已授予，但偏好未保存，通知尚未启用", err)
		}
		return err
	}
	if needNotifications {
		ok, err := s.RequestNotifications()
		notificationsAuthorized = ok
		if err != nil {
			return noteNotificationAuthorization(err)
		}
		if !ok {
			return errors.New("系统通知未获授权")
		}
	}
	autoStartApplied := false
	rollbackAutoStart := func(value bool) error {
		if !autoStartApplied {
			return nil
		}
		return s.SetAutoStart(value)
	}
	if needAutoStart {
		if err := s.SetAutoStart(p.AutoStart); err != nil {
			rollbackErr := s.SetAutoStart(old.AutoStart)
			if rollbackErr != nil {
				return noteNotificationAuthorization(errors.Join(err, fmt.Errorf("回滚开机启动状态失败：%w", rollbackErr)))
			}
			return noteNotificationAuthorization(err)
		}
		autoStartApplied = true
	}

	s.writes.Lock()
	s.stagingMu.Lock()
	s.mu.Lock()
	current := defaults(s.cfg.Preferences)
	oldStored := s.cfg.Preferences
	if !reflect.DeepEqual(current, old) {
		s.mu.Unlock()
		s.stagingMu.Unlock()
		s.writes.Unlock()
		rollbackErr := rollbackAutoStart(current.AutoStart)
		if rollbackErr != nil {
			return noteNotificationAuthorization(errors.Join(storage.ErrConflict, fmt.Errorf("回滚开机启动状态失败：%w", rollbackErr)))
		}
		return noteNotificationAuthorization(storage.ErrConflict)
	}
	if err := checkBudget(old); err != nil {
		s.mu.Unlock()
		s.stagingMu.Unlock()
		s.writes.Unlock()
		rollbackErr := rollbackAutoStart(old.AutoStart)
		if rollbackErr != nil {
			return noteNotificationAuthorization(errors.Join(err, fmt.Errorf("回滚开机启动状态失败：%w", rollbackErr)))
		}
		return noteNotificationAuthorization(err)
	}
	s.cfg.Preferences = p
	persistErr := s.saveLocked()
	if persistErr != nil {
		s.cfg.Preferences = oldStored
	}
	s.mu.Unlock()
	s.stagingMu.Unlock()
	s.writes.Unlock()
	if persistErr == nil {
		return nil
	}
	rollbackErr := rollbackAutoStart(old.AutoStart)
	if rollbackErr != nil {
		persistErr = errors.Join(persistErr, fmt.Errorf("回滚开机启动状态失败：%w", rollbackErr))
	}
	return noteNotificationAuthorization(persistErr)
}

type throttledReader struct {
	s   *Service
	ctx context.Context
	r   io.Reader
}

func (r throttledReader) Read(p []byte) (int, error) {
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	bps := r.s.Preferences().BandwidthBytes
	if n > 0 && bps > 0 {
		r.s.transferMu.Lock()
		now := time.Now()
		if r.s.nextTransfer.Before(now) {
			r.s.nextTransfer = now
		}
		r.s.nextTransfer = r.s.nextTransfer.Add(time.Duration(float64(n) / float64(bps) * float64(time.Second)))
		wait := time.Until(r.s.nextTransfer)
		r.s.transferMu.Unlock()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		case <-timer.C:
		}
	}
	return n, err
}
func (s *Service) limitReader(ctx context.Context, r io.Reader) io.Reader {
	if _, ok := r.(interface{ rateLimited() }); ok {
		return r
	}
	return throttledReader{s, ctx, r}
}

type ConfigurationExport struct {
	BackupJobs    []BackupJob      `json:"backupJobs"`
	MigrationJobs []MigrationJob   `json:"migrationJobs"`
	Version       int              `json:"version"`
	Connections   []storage.Config `json:"connections"`
	Jobs          []Job            `json:"jobs"`
	Gateways      []Gateway        `json:"gateways"`
	Preferences   Preferences      `json:"preferences"`
}

func (s *Service) ExportConfiguration() (ConfigurationExport, error) {
	backups, err := s.ListBackupJobs()
	if err != nil {
		return ConfigurationExport{}, err
	}
	migrations, err := s.ListMigrationJobs()
	if err != nil {
		return ConfigurationExport{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ConfigurationExport{Version: 1, Connections: []storage.Config{}, Jobs: []Job{}, Gateways: []Gateway{}, Preferences: defaults(s.cfg.Preferences)}
	keep := map[string]bool{}
	for _, c := range s.cfg.Connections {
		if c.Kind != "demo" {
			out.Connections = append(out.Connections, c.Config)
			keep[c.ID] = true
		}
	}
	for _, j := range s.cfg.Jobs {
		if keep[j.ConnectionID] {
			j.LocalPath = ""
			j.Enabled = false
			j.Status = "paused"
			j.LastRun = ""
			j.Detail = "导入后重新选择本地目录"
			j.Progress = 0
			j.QueueDone = 0
			j.QueueTotal = 0
			out.Jobs = append(out.Jobs, j)
		}
	}
	for _, g := range s.cfg.Gateways {
		if keep[g.ConnectionID] {
			g.Running = false
			g.TLSCert = ""
			g.TLSKey = ""
			g.ListenHost = "127.0.0.1"
			g.URL = ""
			out.Gateways = append(out.Gateways, g)
		}
	}
	for _, j := range backups {
		if keep[j.ConnectionID] {
			j.LocalPath = ""
			j.Enabled = false
			j.LastRun = ""
			j.Status = "paused"
			j.Detail = "重新选择本地目录并补充凭据后启用"
			out.BackupJobs = append(out.BackupJobs, j)
		}
	}
	for _, j := range migrations {
		if keep[j.SourceConnection] && keep[j.TargetConnection] {
			j.Enabled = false
			out.MigrationJobs = append(out.MigrationJobs, j)
		}
	}
	return out, nil
}
func (s *Service) ImportConfiguration(in ConfigurationExport) (map[string]int, error) {
	if in.Version != 1 || len(in.Connections) > 100 || len(in.Jobs) > 500 || len(in.Gateways) > 100 || len(in.BackupJobs) > 500 || len(in.MigrationJobs) > 500 {
		return nil, errors.New("配置版本或数量无效")
	}
	idmap := map[string]string{}
	connections := []Connection{}
	jobs := []Job{}
	gates := []Gateway{}
	for _, c := range in.Connections {
		if c.ID == "" || idmap[c.ID] != "" || strings.TrimSpace(c.Name) == "" {
			return nil, errors.New("连接标识或名称无效")
		}
		if _, err := storage.New(c, storage.Credentials{AccessKey: "validate", SecretKey: "validate"}); err != nil {
			return nil, err
		}
		idmap[c.ID] = ID()
		c.ID = idmap[c.ID]
		connections = append(connections, Connection{Config: c, Error: "请补充凭据并测试连接"})
	}
	for _, j := range in.Jobs {
		if idmap[j.ConnectionID] == "" {
			return nil, errors.New("任务引用了缺失连接")
		}
		if err := validKey(j.RemotePath); err != nil {
			return nil, err
		}
		if err := validateExcludePatterns(j.Exclude); err != nil {
			return nil, err
		}
		switch j.Direction {
		case "both", "upload", "download", "mirror-upload", "mirror-download":
		default:
			return nil, errors.New("无效同步方向")
		}
		j.ID = ID()
		j.ConnectionID = idmap[j.ConnectionID]
		j.LocalPath = ""
		j.Enabled = false
		j.Status = "paused"
		j.LastRun = ""
		j.Detail = "重新选择本地目录后启用"
		j.Progress = 0
		j.QueueTotal = 0
		j.QueueDone = 0
		jobs = append(jobs, j)
	}
	for _, g := range in.Gateways {
		if idmap[g.ConnectionID] == "" {
			return nil, errors.New("网关引用了缺失连接")
		}
		if err := validKey(g.Prefix); err != nil {
			return nil, err
		}
		if g.Port < 1024 || g.Port > 65535 || strings.ContainsAny(g.Username, ":\r\n") {
			return nil, errors.New("入口配置无效")
		}
		g.TLSCert = ""
		g.TLSKey = ""
		g.ListenHost = "127.0.0.1"
		g.ID = ID()
		g.ConnectionID = idmap[g.ConnectionID]
		g.Running = false
		g.URL = fmt.Sprintf("http://127.0.0.1:%d/", g.Port)
		gates = append(gates, g)
	}
	backups := []BackupJob{}
	migrations := []MigrationJob{}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	configByID := map[string]Connection{}
	for _, c := range connections {
		configByID[c.ID] = c
	}
	for _, j := range in.BackupJobs {
		if idmap[j.ConnectionID] == "" || strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 || j.RemotePrefix == "" || validKey(j.RemotePrefix) != nil || validSyncPath(j.RemotePrefix) != nil || validateExcludePatterns(j.Exclude) != nil || j.ScheduleMinutes < 0 || j.ScheduleMinutes > 525600 || !validRetention(j.RetainRecent) || !validRetention(j.RetainDaily) || !validRetention(j.RetainMonthly) {
			return nil, errors.New("备份任务配置无效")
		}
		if j.RetainRecent+j.RetainDaily+j.RetainMonthly == 0 {
			j.RetainRecent = 5
			j.RetainDaily = 7
			j.RetainMonthly = 3
		}
		j.ID = ID()
		j.ConnectionID = idmap[j.ConnectionID]
		j.LocalPath = ""
		j.Enabled = false
		j.Status = "paused"
		j.LastRun = ""
		j.Detail = "重新选择本地目录并补充凭据后启用"
		j.Created = now
		_, key := resource(configByID[j.ConnectionID], j.RemotePrefix)
		if isManagedPath(key) {
			return nil, errors.New("备份前缀不能嵌套于受管理目录")
		}
		backups = append(backups, j)
	}
	for _, j := range in.MigrationJobs {
		if idmap[j.SourceConnection] == "" || idmap[j.TargetConnection] == "" || strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 || validKey(j.SourcePrefix) != nil || validKey(j.TargetPrefix) != nil || validateExcludePatterns(j.Exclude) != nil {
			return nil, errors.New("迁移任务配置无效")
		}
		j.ID = ID()
		j.SourceConnection = idmap[j.SourceConnection]
		j.TargetConnection = idmap[j.TargetConnection]
		j.Enabled = false
		j.Deleted = false
		j.Created = now
		ns1, k1 := resource(configByID[j.SourceConnection], j.SourcePrefix)
		ns2, k2 := resource(configByID[j.TargetConnection], j.TargetPrefix)
		if isManagedPath(k1) || isManagedPath(k2) || (ns1 == ns2 && pathsOverlap(k1, k2)) {
			return nil, errors.New("迁移范围重叠或包含受保护目录")
		}
		migrations = append(migrations, j)
	}
	p := defaults(in.Preferences)
	if err := validatePreferences(p); err != nil {
		return nil, err
	}
	if err := s.ensureBackupSchema(); err != nil {
		return nil, err
	}
	if err := s.ensureMigrationSchema(); err != nil {
		return nil, err
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	s.stagingMu.Lock()
	defer s.stagingMu.Unlock()
	if p.StagingBytes < directorySize(filepath.Join(s.dir, "staging"))+s.stagingReservedLocked() || p.RecoveryBytes < directorySize(filepath.Join(s.dir, "recovery")) {
		return nil, errors.New("导入额度小于现有受保护数据")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cfg
	if len(s.stagingReservations) > 0 && p.MaxFileBytes != defaults(old.Preferences).MaxFileBytes {
		return nil, errors.New("传输期间不能导入不同单文件上限")
	}
	p.AutoStart = old.Preferences.AutoStart
	p.Notifications = old.Preferences.Notifications
	s.cfg.Connections = append(append([]Connection{}, old.Connections...), connections...)
	s.cfg.Jobs = append(append([]Job{}, old.Jobs...), jobs...)
	s.cfg.Gateways = append(append([]Gateway{}, old.Gateways...), gates...)
	s.cfg.Preferences = p
	committed := false
	defer func() {
		if !committed {
			s.cfg = old
		}
	}()
	raw, err := s.durableJSONLocked()
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(raw)); err != nil {
		return nil, err
	}
	for _, j := range backups {
		ex, _ := json.Marshal(j.Exclude)
		if _, err = tx.Exec(`INSERT INTO backup_jobs(id,name,connection,local_path,remote_prefix,exclude_json,schedule_minutes,retain_recent,retain_daily,retain_monthly,enabled,last_run,status,detail,created) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.Name, j.ConnectionID, "", j.RemotePrefix, string(ex), j.ScheduleMinutes, j.RetainRecent, j.RetainDaily, j.RetainMonthly, 0, "", j.Status, j.Detail, j.Created); err != nil {
			return nil, err
		}
	}
	for _, j := range migrations {
		ex, _ := json.Marshal(j.Exclude)
		if _, err = tx.Exec(`INSERT INTO migration_jobs(id,name,source_connection,source_prefix,target_connection,target_prefix,overwrite,created,enabled,deleted,exclude_json) VALUES(?,?,?,?,?,?,?,?,0,0,?)`, j.ID, j.Name, j.SourceConnection, j.SourcePrefix, j.TargetConnection, j.TargetPrefix, backupBoolInt(j.Overwrite), j.Created, string(ex)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return map[string]int{"connections": len(connections), "jobs": len(jobs), "gateways": len(gates), "backupJobs": len(backups), "migrationJobs": len(migrations)}, nil

}
func (s *Service) UpdateCredentials(ctx context.Context, in ConnectionInput) error {
	c, err := s.connection(in.ID)
	if err != nil {
		return err
	}
	if c.Kind == "demo" {
		return errors.New("演示空间无需凭据")
	}
	if c.Kind == "webdav" {
		requested := strings.TrimSpace(in.Username)
		if requested != "" && requested != c.Username {
			return errors.New("更改 WebDAV 用户名会改变连接存储身份，请使用“编辑配置”重新验证并重置同步基线")
		}
		in.Username = c.Username
	}
	creds := storage.Credentials{Username: in.Username, Password: in.Password, AccessKey: in.AccessKey, SecretKey: in.SecretKey, SessionToken: in.SessionToken}
	cfg := c.Config
	cfg.Username = in.Username
	st, err := storage.New(cfg, creds)
	if err != nil {
		return err
	}
	if _, err = st.List(ctx, ""); err != nil {
		return err
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == in.ID {
			index = i
			break
		}
	}
	if index < 0 {
		return storage.ErrNotFound
	}
	if s.cfg.Connections[index].Config != c.Config {
		return storage.ErrConflict
	}
	oldConnection := s.cfg.Connections[index]
	oldStore, hadStore := s.stores[in.ID]
	key := "connection:" + in.ID
	oldCredential, err := s.vault.Get(key)
	if err != nil {
		return fmt.Errorf("无法读取现有凭据；为保证失败时可回滚，凭据未更新：%w", err)
	}
	disableAfterRollbackFailure := func(cause error) error {
		conn := oldConnection
		conn.Tested = false
		conn.Capabilities = storage.Capabilities{}
		conn.Error = "凭据更新回滚失败，连接已禁用；请重新输入并测试凭据"
		s.cfg.Connections[index] = conn
		s.stores[in.ID] = unavailableCredentialStore{}
		persistErr := s.saveLocked()
		poisonErr := s.vault.Set(key, "")
		if poisonErr != nil {
			poisonErr = s.vault.Delete(key)
		}
		result := fmt.Errorf("凭据更新失败且无法恢复原凭据；连接已在当前运行中禁用：%w", cause)
		if persistErr != nil {
			result = errors.Join(result, fmt.Errorf("禁用状态未能保存到配置：%w", persistErr))
		}
		if poisonErr != nil {
			result = errors.Join(result, fmt.Errorf("无法清除不确定凭据；应用重启后仍需检查连接：%w", poisonErr))
		}
		return result
	}
	if err = s.vault.Set(key, string(raw)); err != nil {
		if rollbackErr := s.vault.Set(key, oldCredential); rollbackErr != nil {
			return disableAfterRollbackFailure(errors.Join(err, fmt.Errorf("恢复原凭据失败：%w", rollbackErr)))
		}
		return fmt.Errorf("凭据库保存失败：%w", err)
	}
	updated := oldConnection
	updated.Config = cfg
	updated.Capabilities = storage.Capabilities{}
	updated.Tested = true
	updated.Error = ""
	s.cfg.Connections[index] = updated
	s.stores[in.ID] = st
	if err = s.saveLocked(); err == nil {
		return nil
	}
	s.cfg.Connections[index] = oldConnection
	if hadStore {
		s.stores[in.ID] = oldStore
	} else {
		delete(s.stores, in.ID)
	}
	if rollbackErr := s.vault.Set(key, oldCredential); rollbackErr != nil {
		return disableAfterRollbackFailure(errors.Join(err, fmt.Errorf("恢复原凭据失败：%w", rollbackErr)))
	}
	return err
}
func (s *Service) ResetGatewayPassword(id string) (string, error) {
	if err := s.StopGateway(id); err != nil {
		return "", err
	}
	secret := ID()
	if err := s.vault.Set("gateway:"+id, secret); err != nil {
		return "", errors.New("凭据保存失败")
	}
	s.mu.Lock()
	s.passwords[id] = secret
	s.mu.Unlock()
	return secret, nil
}
func (s *Service) Diagnostics() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	connections := []map[string]any{}
	for _, c := range s.cfg.Connections {
		u, _ := url.Parse(c.Endpoint)
		scheme := ""
		if u != nil {
			scheme = u.Scheme
		}
		connections = append(connections, map[string]any{"kind": c.Kind, "scheme": scheme, "tested": c.Tested, "capabilities": c.Capabilities})
	}
	states := map[string]int{}
	for _, j := range s.cfg.Jobs {
		states[j.Status]++
	}
	return map[string]any{"version": Version, "os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version(), "generated": time.Now().UTC(), "connections": connections, "jobStates": states, "gatewayCount": len(s.cfg.Gateways), "freeBytes": freeBytes(s.dir), "stagingBytes": directorySize(filepath.Join(s.dir, "staging")), "recoveryBytes": directorySize(filepath.Join(s.dir, "recovery"))}
}
func writeJSONFile(dest string, value any) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encErr := json.NewEncoder(f).Encode(value)
	if encErr == nil {
		encErr = f.Sync()
	}
	closeErr := f.Close()
	if encErr != nil {
		_ = os.Remove(dest)
		return encErr
	}
	return closeErr
}
