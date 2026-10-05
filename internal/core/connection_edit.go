package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"tamiops/internal/storage"
)

const connectionEditPreviewTTL = 5 * time.Minute

type connectionEditReference struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type connectionEditImpact struct {
	ScopeChanged    bool                      `json:"scopeChanged"`
	SyncJobs        []connectionEditReference `json:"syncJobs"`
	Gateways        []connectionEditReference `json:"gateways"`
	Backups         int                       `json:"backups"`
	BackupSnapshots int                       `json:"backupSnapshots"`
	Migrations      int                       `json:"migrations"`
	CacheEntries    int                       `json:"cacheEntries"`
	Downloads       int                       `json:"downloads"`
	PendingOps      int                       `json:"pendingOperations"`
	DAVLocks        int                       `json:"davLocks"`
	Blockers        []string                  `json:"blockers"`
}

type connectionEditPreview struct {
	Token                 string               `json:"token"`
	ExpiresAt             time.Time            `json:"expiresAt"`
	Connection            storage.Config       `json:"connection"`
	Candidate             storage.Config       `json:"candidate"`
	WriteModeChanged      bool                 `json:"writeModeChanged"`
	RecoveryChanged       bool                 `json:"recoveryChanged"`
	PolicyChanged         bool                 `json:"policyChanged"`
	Impact                connectionEditImpact `json:"impact"`
	Capabilities          storage.Capabilities `json:"capabilities"`
	WriteRestriction      string               `json:"writeRestriction,omitempty"`
	CompatibilityProfile  string               `json:"compatibilityProfile,omitempty"`
	CapabilityVersion     int                  `json:"capabilityVersion,omitempty"`
	CapabilitiesCheckedAt string               `json:"capabilitiesCheckedAt,omitempty"`
}

type connectionEditStoredPreview struct {
	ConnectionID string
	OldConfig    string
	Candidate    string
	ScopeChanged bool
	Capabilities string
	Expires      string
}

func ensureConnectionEditSchema(s *Service) error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS connection_edit_previews (
		token TEXT PRIMARY KEY, connection TEXT NOT NULL, old_config TEXT NOT NULL,
		candidate TEXT NOT NULL, scope_changed INTEGER NOT NULL,
		capabilities TEXT NOT NULL, expires TEXT NOT NULL)`)
	if err != nil {
		return err
	}
	// The dirty marker is owned by sync scheduling. Creating the same compact
	// table here also makes edits safe against databases upgraded independently.
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS sync_dirty(job TEXT PRIMARY KEY,token TEXT,updated TEXT)`)
	return err
}

func normalizeConnectionEdit(in ConnectionInput) (storage.Config, error) {
	c := in.Config
	c.ID = strings.TrimSpace(c.ID)
	c.Name = strings.TrimSpace(c.Name)
	c.Endpoint = strings.TrimSpace(c.Endpoint)
	c.Region = strings.TrimSpace(c.Region)
	c.Bucket = strings.TrimSpace(c.Bucket)
	c.Prefix = strings.Trim(c.Prefix, "/")
	c.Username = strings.TrimSpace(c.Username)
	writeMode, err := storage.NormalizeWriteMode(c.WriteMode)
	if err != nil {
		return storage.Config{}, err
	}
	c.WriteMode = writeMode
	if c.ID == "" || c.Name == "" {
		return storage.Config{}, errors.New("连接编号或名称无效")
	}
	if c.Kind != "s3" && c.Kind != "webdav" {
		return storage.Config{}, errors.New("此连接类型不支持编辑")
	}
	if err := validKey(c.Prefix); err != nil {
		return storage.Config{}, err
	}
	return c, nil
}

func connectionStorageScopeChanged(a, b storage.Config) bool {
	if a.Kind != b.Kind || a.Endpoint != b.Endpoint || a.Prefix != b.Prefix {
		return true
	}
	if a.Kind == "webdav" {
		return a.Username != b.Username
	}
	return a.Region != b.Region || a.Bucket != b.Bucket || a.PathStyle != b.PathStyle
}

func connectionWriteModeChanged(a, b storage.Config) bool {
	oldMode, _ := storage.NormalizeWriteMode(a.WriteMode)
	newMode, _ := storage.NormalizeWriteMode(b.WriteMode)
	return oldMode != newMode
}

func connectionRecoveryChanged(a, b storage.Config) bool {
	return a.KeepRecovery != b.KeepRecovery
}

func connectionPolicyChanged(a, b storage.Config) bool {
	return connectionWriteModeChanged(a, b) || connectionRecoveryChanged(a, b)
}

func connectionCredentialKey(in ConnectionInput, old storage.Credentials, cfg storage.Config) storage.Credentials {
	creds := old
	if in.Password != "" {
		creds.Password = in.Password
	}
	if in.AccessKey != "" {
		creds.AccessKey = in.AccessKey
	}
	if in.SecretKey != "" {
		creds.SecretKey = in.SecretKey
	}
	if in.SessionToken != "" {
		creds.SessionToken = in.SessionToken
	}
	if cfg.Username != "" {
		creds.Username = cfg.Username
	}
	return creds
}

func loadConnectionCredentials(s *Service, id string) (storage.Credentials, string, error) {
	raw, err := s.vault.Get("connection:" + id)
	if err != nil {
		return storage.Credentials{}, "", errors.New("无法读取系统凭据库，连接未修改")
	}
	var creds storage.Credentials
	if err = json.Unmarshal([]byte(raw), &creds); err != nil {
		return storage.Credentials{}, "", errors.New("现有凭据格式无效，连接未修改")
	}
	return creds, raw, nil
}

func editCandidateFingerprint(cfg storage.Config, creds storage.Credentials) string {
	b, _ := json.Marshal(struct {
		Config      storage.Config
		Credentials storage.Credentials
	}{cfg, creds})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func editConfigFingerprint(cfg storage.Config) string {
	b, _ := json.Marshal(cfg)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// connectionEditReferences makes storage-scope edits require a fresh baseline
// and no live consumers of the previous remote namespace. Disabled sync tasks
// may remain; the commit atomically clears their plans and baselines.
func (s *Service) connectionEditReferences(id string, old storage.Config, scopeChanged bool) (connectionEditImpact, error) {
	impact := connectionEditImpact{ScopeChanged: scopeChanged, SyncJobs: []connectionEditReference{}, Gateways: []connectionEditReference{}, Blockers: []string{}}
	s.mu.Lock()
	for _, j := range s.cfg.Jobs {
		if j.ConnectionID != id {
			continue
		}
		impact.SyncJobs = append(impact.SyncJobs, connectionEditReference{ID: j.ID, Name: j.Name})
		if j.Status == "running" {
			impact.Blockers = append(impact.Blockers, "请等待正在执行的同步任务结束")
		} else if scopeChanged && j.Enabled {
			impact.Blockers = append(impact.Blockers, "范围变更前请暂停并禁用所有同步任务")
		}
	}
	for _, g := range s.cfg.Gateways {
		if g.ConnectionID != id {
			continue
		}
		impact.Gateways = append(impact.Gateways, connectionEditReference{ID: g.ID, Name: g.Name})
		if scopeChanged {
			impact.Blockers = append(impact.Blockers, "范围变更前请删除引用此连接的网关")
		} else if g.Running || s.servers[g.ID] != nil {
			impact.Blockers = append(impact.Blockers, "请先停止正在使用此连接的网关")
		}
	}
	s.mu.Unlock()
	if err := s.ensureBackupSchema(); err != nil {
		return impact, err
	}
	if err := s.ensureMigrationSchema(); err != nil {
		return impact, err
	}
	if scopeChanged {
		if err := ensureVersionRestoreSchema(s); err != nil {
			return impact, err
		}
		var activeRestores int
		if err := s.db.QueryRow(`SELECT count(*) FROM version_restore_previews WHERE connection=? AND active=1 AND expires>?`, id, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&activeRestores); err != nil {
			return impact, err
		}
		if activeRestores > 0 {
			impact.Blockers = append(impact.Blockers, "请等待正在进行的历史版本恢复完成")
		}
		if err := s.db.QueryRow(`SELECT count(*) FROM backup_jobs WHERE connection=? AND status!='deleted'`, id).Scan(&impact.Backups); err != nil {
			return impact, err
		}
		if err := s.db.QueryRow(`SELECT count(*) FROM backup_snapshots WHERE remote_connection=? AND status!='cleaned'`, id).Scan(&impact.BackupSnapshots); err != nil {
			return impact, err
		}
		if err := s.db.QueryRow(`SELECT count(*) FROM migration_jobs WHERE deleted=0 AND (source_connection=? OR target_connection=?)`, id, id).Scan(&impact.Migrations); err != nil {
			return impact, err
		}
		if err := s.db.QueryRow(`SELECT count(*) FROM cache_entries WHERE connection=?`, id).Scan(&impact.CacheEntries); err != nil {
			return impact, err
		}
		var activeUploads int
		if err := s.db.QueryRow(`SELECT count(*) FROM multipart_uploads WHERE connection=? AND state NOT IN ('completed','aborted','failed')`, id).Scan(&activeUploads); err != nil {
			return impact, err
		}
		if activeUploads > 0 {
			impact.Blockers = append(impact.Blockers, "请先完成或中止此连接的分片上传")
		}
		if impact.Backups > 0 || impact.BackupSnapshots > 0 {
			impact.Blockers = append(impact.Blockers, "此连接仍有备份任务或保留中的历史快照；请保留原连接，另建连接使用新存储范围")
		}
		if impact.Migrations > 0 {
			impact.Blockers = append(impact.Blockers, "范围变更前请删除引用此连接的迁移任务")
		}
		if impact.CacheEntries > 0 {
			impact.Blockers = append(impact.Blockers, "范围变更前请清除本机缓存，避免旧缓存上传到新存储")
		}
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM downloads WHERE connection=? AND state NOT IN ('done','cancelled')`, id).Scan(&impact.Downloads); err != nil {
		return impact, err
	}
	namespace, _ := resource(Connection{Config: old}, "")
	if err := s.db.QueryRow(`SELECT count(*) FROM operations WHERE state IN ('committing','uncertain') AND (connection=? OR json_extract(receipt,'$.sourceConnection')=? OR json_extract(receipt,'$.destinationConnection')=?)`, namespace, id, id).Scan(&impact.PendingOps); err != nil {
		return impact, err
	}
	if scopeChanged {
		if err := s.db.QueryRow(`SELECT count(*) FROM dav_locks WHERE connection=? AND expires>?`, namespace, time.Now().Unix()).Scan(&impact.DAVLocks); err != nil {
			return impact, err
		}
	}
	if impact.Downloads > 0 {
		impact.Blockers = append(impact.Blockers, "请先完成或取消此连接的下载")
	}
	if impact.PendingOps > 0 {
		impact.Blockers = append(impact.Blockers, "请先核对并处理此连接的未决提交")
	}
	if impact.DAVLocks > 0 {
		impact.Blockers = append(impact.Blockers, "请等待此连接上的 WebDAV 锁过期")
	}
	sort.Slice(impact.SyncJobs, func(i, j int) bool { return impact.SyncJobs[i].ID < impact.SyncJobs[j].ID })
	sort.Slice(impact.Gateways, func(i, j int) bool { return impact.Gateways[i].ID < impact.Gateways[j].ID })
	sort.Strings(impact.Blockers)
	impact.Blockers = dedupeStrings(impact.Blockers)
	return impact, nil
}

func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func (s *Service) previewConnectionEdit(ctx context.Context, in ConnectionInput) (connectionEditPreview, error) {
	if err := ensureConnectionEditSchema(s); err != nil {
		return connectionEditPreview{}, err
	}
	candidate, err := normalizeConnectionEdit(in)
	if err != nil {
		return connectionEditPreview{}, err
	}
	s.writes.Lock()
	old, err := s.connection(candidate.ID)
	if err != nil {
		s.writes.Unlock()
		return connectionEditPreview{}, err
	}
	if old.Kind == "demo" || old.Kind != candidate.Kind {
		s.writes.Unlock()
		return connectionEditPreview{}, errors.New("连接类型不能更改")
	}
	if candidate.Kind == "webdav" && candidate.Username == "" {
		candidate.Username = old.Username
	}
	oldCreds, _, err := loadConnectionCredentials(s, candidate.ID)
	if err != nil {
		s.writes.Unlock()
		return connectionEditPreview{}, err
	}
	creds := connectionCredentialKey(in, oldCreds, candidate)
	oldFingerprint := editConfigFingerprint(old.Config)
	candidateFingerprint := editCandidateFingerprint(candidate, creds)
	scopeChanged := connectionStorageScopeChanged(old.Config, candidate)
	writeModeChanged := connectionWriteModeChanged(old.Config, candidate)
	recoveryChanged := connectionRecoveryChanged(old.Config, candidate)
	policyChanged := writeModeChanged || recoveryChanged
	impact, err := s.connectionEditReferences(candidate.ID, old.Config, scopeChanged)
	if err != nil {
		s.writes.Unlock()
		return connectionEditPreview{}, err
	}
	if len(impact.Blockers) > 0 {
		s.writes.Unlock()
		return connectionEditPreview{Connection: old.Config, Candidate: candidate, WriteModeChanged: writeModeChanged, RecoveryChanged: recoveryChanged, PolicyChanged: policyChanged, Impact: impact}, nil
	}
	// Connection validation can reach a local gateway served by this Service.
	// Do not hold the commit coordinator while issuing that network request.
	s.writes.Unlock()
	store, err := storage.New(candidate, creds)
	if err != nil {
		return connectionEditPreview{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	detection, err := detectConnectionCapabilities(probeCtx, store, candidate.WriteMode, true)
	if err != nil {
		return connectionEditPreview{}, err
	}
	caps := detection.Capabilities
	writeRestriction := detection.WriteRestriction
	s.writes.Lock()
	defer s.writes.Unlock()
	current, err := s.connection(candidate.ID)
	if err != nil {
		return connectionEditPreview{}, err
	}
	currentCreds, _, err := loadConnectionCredentials(s, candidate.ID)
	if err != nil {
		return connectionEditPreview{}, err
	}
	if editConfigFingerprint(current.Config) != oldFingerprint || editCandidateFingerprint(candidate, connectionCredentialKey(in, currentCreds, candidate)) != candidateFingerprint {
		return connectionEditPreview{}, storage.ErrConflict
	}
	// Remote validation has completed, so recheck local references and the exact
	// config/credential snapshot before issuing a token.
	impact, err = s.connectionEditReferences(candidate.ID, old.Config, scopeChanged)
	if err != nil {
		return connectionEditPreview{}, err
	}
	if len(impact.Blockers) > 0 {
		return connectionEditPreview{Connection: old.Config, Candidate: candidate, WriteModeChanged: writeModeChanged, RecoveryChanged: recoveryChanged, PolicyChanged: policyChanged, Impact: impact, Capabilities: caps, WriteRestriction: writeRestriction, CompatibilityProfile: detection.CompatibilityProfile, CapabilityVersion: detection.CapabilityVersion, CapabilitiesCheckedAt: detection.CapabilitiesCheckedAt}, nil
	}
	capRaw, _ := json.Marshal(detection)
	token := ID()
	expires := time.Now().Add(connectionEditPreviewTTL).UTC()
	_, err = s.db.Exec(`INSERT INTO connection_edit_previews(token,connection,old_config,candidate,scope_changed,capabilities,expires) VALUES(?,?,?,?,?,?,?)`, token, candidate.ID, editConfigFingerprint(old.Config), editCandidateFingerprint(candidate, creds), scopeChanged, string(capRaw), expires.Format(time.RFC3339Nano))
	if err != nil {
		return connectionEditPreview{}, err
	}
	_, _ = s.db.Exec(`DELETE FROM connection_edit_previews WHERE expires<?`, time.Now().UTC().Format(time.RFC3339Nano))
	return connectionEditPreview{Token: token, ExpiresAt: expires, Connection: old.Config, Candidate: candidate, WriteModeChanged: writeModeChanged, RecoveryChanged: recoveryChanged, PolicyChanged: policyChanged, Impact: impact, Capabilities: caps, WriteRestriction: writeRestriction, CompatibilityProfile: detection.CompatibilityProfile, CapabilityVersion: detection.CapabilityVersion, CapabilitiesCheckedAt: detection.CapabilitiesCheckedAt}, nil
}

func (s *Service) applyConnectionEdit(ctx context.Context, in ConnectionInput, token string) (Connection, error) {
	if token == "" {
		return Connection{}, errors.New("请先验证连接更改")
	}
	if err := ensureConnectionEditSchema(s); err != nil {
		return Connection{}, err
	}
	candidate, err := normalizeConnectionEdit(in)
	if err != nil {
		return Connection{}, err
	}
	s.mu.Lock()
	jobIDs := make([]string, 0)
	for _, j := range s.cfg.Jobs {
		if j.ConnectionID == candidate.ID {
			jobIDs = append(jobIDs, j.ID)
		}
	}
	s.mu.Unlock()
	sort.Strings(jobIDs)
	locks := make([]*sync.Mutex, 0, len(jobIDs))
	for _, id := range jobIDs {
		l := s.syncJobLock(id)
		l.Lock()
		locks = append(locks, l)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	var old Connection
	var oldRaw string
	var creds storage.Credentials
	var stored connectionEditStoredPreview
	var detection connectionDetection
	var scopeChanged bool
	var policyChanged bool
	var impact connectionEditImpact
	s.writes.Lock()
	prepareErr := func() error {
		var err error
		old, err = s.connection(candidate.ID)
		if err != nil {
			return err
		}
		if old.Kind == "demo" || old.Kind != candidate.Kind {
			return errors.New("连接类型不能更改")
		}
		if candidate.Kind == "webdav" && candidate.Username == "" {
			candidate.Username = old.Username
		}
		oldCreds, raw, err := loadConnectionCredentials(s, candidate.ID)
		if err != nil {
			return err
		}
		oldRaw = raw
		creds = connectionCredentialKey(in, oldCreds, candidate)
		err = s.db.QueryRow(`SELECT connection,old_config,candidate,scope_changed,capabilities,expires FROM connection_edit_previews WHERE token=?`, token).Scan(&stored.ConnectionID, &stored.OldConfig, &stored.Candidate, &stored.ScopeChanged, &stored.Capabilities, &stored.Expires)
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrConflict
		}
		if err != nil {
			return err
		}
		expires, parseErr := time.Parse(time.RFC3339Nano, stored.Expires)
		if parseErr != nil || time.Now().After(expires) || stored.ConnectionID != candidate.ID || stored.OldConfig != editConfigFingerprint(old.Config) || stored.Candidate != editCandidateFingerprint(candidate, creds) {
			return storage.ErrConflict
		}
		if err = json.Unmarshal([]byte(stored.Capabilities), &detection); err != nil || !detection.current() {
			return storage.ErrConflict
		}
		scopeChanged = connectionStorageScopeChanged(old.Config, candidate)
		policyChanged = connectionPolicyChanged(old.Config, candidate)
		if scopeChanged != stored.ScopeChanged {
			return storage.ErrConflict
		}
		impact, err = s.connectionEditReferences(candidate.ID, old.Config, scopeChanged)
		if err != nil {
			return err
		}
		if len(impact.Blockers) > 0 {
			return errors.New(strings.Join(impact.Blockers, "；"))
		}
		return nil
	}()
	s.writes.Unlock()
	if prepareErr != nil {
		return Connection{}, prepareErr
	}
	// Recheck list access before commit. Conditional capabilities come from the
	// preview token bound to this exact config and credential snapshot, avoiding
	// a second full probe while still confirming the endpoint is reachable.
	store, err := storage.New(candidate, creds)
	if err != nil {
		return Connection{}, err
	}
	commitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err = store.List(commitCtx, ""); err != nil {
		return Connection{}, fmt.Errorf("提交前连接复核失败：%w", err)
	}
	caps := detection.Capabilities
	writeRestriction := detection.WriteRestriction
	s.writes.Lock()
	defer s.writes.Unlock()
	current, err := s.connection(candidate.ID)
	if err != nil {
		return Connection{}, err
	}
	if current.Kind == "demo" || current.Kind != candidate.Kind || editConfigFingerprint(current.Config) != stored.OldConfig {
		return Connection{}, storage.ErrConflict
	}
	old = current
	oldCreds, oldCredentialRaw, err := loadConnectionCredentials(s, candidate.ID)
	if err != nil {
		return Connection{}, err
	}
	oldRaw = oldCredentialRaw
	creds = connectionCredentialKey(in, oldCreds, candidate)
	var storedAgain connectionEditStoredPreview
	err = s.db.QueryRow(`SELECT connection,old_config,candidate,scope_changed,capabilities,expires FROM connection_edit_previews WHERE token=?`, token).Scan(&storedAgain.ConnectionID, &storedAgain.OldConfig, &storedAgain.Candidate, &storedAgain.ScopeChanged, &storedAgain.Capabilities, &storedAgain.Expires)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Connection{}, storage.ErrConflict
		}
		return Connection{}, err
	}
	if storedAgain != stored || stored.Candidate != editCandidateFingerprint(candidate, creds) {
		return Connection{}, storage.ErrConflict
	}
	expires, err := time.Parse(time.RFC3339Nano, stored.Expires)
	if err != nil || time.Now().After(expires) {
		return Connection{}, storage.ErrConflict
	}
	scopeChanged = connectionStorageScopeChanged(old.Config, candidate)
	policyChanged = connectionPolicyChanged(old.Config, candidate)
	if scopeChanged != stored.ScopeChanged {
		return Connection{}, storage.ErrConflict
	}
	var detectionAgain connectionDetection
	if err = json.Unmarshal([]byte(storedAgain.Capabilities), &detectionAgain); err != nil || detectionAgain != detection || detectionAgain.Capabilities != caps || detectionAgain.WriteRestriction != writeRestriction {
		return Connection{}, storage.ErrConflict
	}
	impact, err = s.connectionEditReferences(candidate.ID, old.Config, scopeChanged)
	if err != nil {
		return Connection{}, err
	}
	if len(impact.Blockers) > 0 {
		return Connection{}, errors.New(strings.Join(impact.Blockers, "；"))
	}
	updated := old
	updated.Config = candidate
	applyConnectionDetection(&updated, detection)
	newRawBytes, err := json.Marshal(creds)
	if err != nil {
		return Connection{}, err
	}
	key := "connection:" + candidate.ID

	s.mu.Lock()
	if scopeChanged || policyChanged {
		latestJobIDs := make([]string, 0)
		for _, job := range s.cfg.Jobs {
			if job.ConnectionID != candidate.ID {
				continue
			}
			latestJobIDs = append(latestJobIDs, job.ID)
			if job.Status == "running" || scopeChanged && job.Enabled {
				s.mu.Unlock()
				return Connection{}, storage.ErrConflict
			}
		}
		sort.Strings(latestJobIDs)
		if len(latestJobIDs) != len(jobIDs) {
			s.mu.Unlock()
			return Connection{}, storage.ErrConflict
		}
		for i := range latestJobIDs {
			if latestJobIDs[i] != jobIDs[i] {
				s.mu.Unlock()
				return Connection{}, storage.ErrConflict
			}
		}
	}
	for _, gateway := range s.cfg.Gateways {
		if gateway.ConnectionID != candidate.ID {
			continue
		}
		if scopeChanged || gateway.Running || s.servers[gateway.ID] != nil {
			s.mu.Unlock()
			return Connection{}, storage.ErrConflict
		}
	}
	index := -1
	for i := range s.cfg.Connections {
		if s.cfg.Connections[i].ID == candidate.ID {
			index = i
			break
		}
	}
	if index < 0 || s.cfg.Connections[index].Config != old.Config {
		s.mu.Unlock()
		return Connection{}, storage.ErrConflict
	}
	oldConnection := s.cfg.Connections[index]
	oldStore, hadStore := s.stores[candidate.ID]
	oldJobs := append([]Job(nil), s.cfg.Jobs...)
	restoreMemory := func() {
		s.cfg.Connections[index] = oldConnection
		s.cfg.Jobs = oldJobs
		if hadStore {
			s.stores[candidate.ID] = oldStore
		} else {
			delete(s.stores, candidate.ID)
		}
	}
	s.cfg.Connections[index] = updated
	s.stores[candidate.ID] = store
	if scopeChanged || policyChanged {
		for i := range s.cfg.Jobs {
			if s.cfg.Jobs[i].ConnectionID == candidate.ID {
				s.cfg.Jobs[i].Enabled = false
				s.cfg.Jobs[i].Status = "paused"
				if scopeChanged {
					s.cfg.Jobs[i].Detail = "连接存储范围已更改，旧同步基线已重置；重新预览后再恢复"
				} else {
					s.cfg.Jobs[i].Detail = "连接写入策略或恢复副本设置已更改，旧预览计划已作废；重新预览后再启用"
				}
			}
		}
	}
	configBytes, marshalErr := s.durableJSONLocked()
	if marshalErr != nil {
		restoreMemory()
		s.mu.Unlock()
		return Connection{}, marshalErr
	}
	tx, txErr := s.db.Begin()
	if txErr == nil {
		_, txErr = tx.Exec(`INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, string(configBytes))
	}
	if txErr == nil && (scopeChanged || policyChanged) {
		for _, id := range jobIDs {
			tables := []string{"sync_queue", "sync_plans", "sync_dirty"}
			if scopeChanged {
				tables = append(tables, "baseline")
			}
			for _, table := range tables {
				if _, txErr = tx.Exec(`DELETE FROM `+table+` WHERE job=?`, id); txErr != nil {
					break
				}
			}
			if txErr != nil {
				break
			}
		}
		if txErr == nil && scopeChanged {
			_, txErr = tx.Exec(`DELETE FROM version_restore_previews WHERE connection=?`, candidate.ID)
		}
	}
	if txErr == nil {
		_, txErr = tx.Exec(`DELETE FROM connection_edit_previews WHERE token=?`, token)
	}
	credentialSetAttempted := false
	if txErr == nil {
		credentialSetAttempted = true
		txErr = s.vault.Set(key, string(newRawBytes))
	}
	if txErr == nil {
		txErr = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if txErr != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		restoreMemory()
		var rollbackErr error
		if credentialSetAttempted {
			rollbackErr = s.vault.Set(key, oldRaw)
		}
		if rollbackErr != nil {
			disabled := oldConnection
			invalidateConnectionDetection(&disabled, "凭据回滚失败，连接已禁用；请重新输入并测试凭据")
			s.cfg.Connections[index] = disabled
			s.stores[candidate.ID] = unavailableCredentialStore{}
			persistErr := s.saveLocked()
			s.mu.Unlock()
			return Connection{}, errors.Join(txErr, fmt.Errorf("恢复原凭据失败，连接已禁用：%w", rollbackErr), persistErr)
		}
		s.mu.Unlock()
		return Connection{}, txErr
	}
	s.mu.Unlock()
	if scopeChanged || policyChanged {
		s.mu.Lock()
		for tok, plan := range s.plans {
			for _, id := range jobIDs {
				if plan.JobID == id {
					delete(s.plans, tok)
					break
				}
			}
		}
		s.mu.Unlock()
	}
	s.activity("connection", "success", "已更新连接 "+candidate.Name, "")
	return updated, nil
}

func (s *Service) connectionEditCommand(r *http.Request) (any, error, bool) {
	switch r.URL.Path {
	case "/api/connections/edit-preview":
		var in ConnectionInput
		if err := decode(r, &in); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		v, err := s.previewConnectionEdit(ctx, in)
		return v, err, true
	case "/api/connections/edit":
		var req struct {
			ConnectionInput
			PreviewToken string `json:"previewToken"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		v, err := s.applyConnectionEdit(ctx, req.ConnectionInput, req.PreviewToken)
		return v, err, true
	default:
		return nil, nil, false
	}
}
