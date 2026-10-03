package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"time"
)

const (
	migrationMaxItems      = 10000
	migrationMaxJobs       = 500
	migrationMaxRunsPerJob = 10000
)

type MigrationJob struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	SourceConnection string   `json:"sourceConnection"`
	SourcePrefix     string   `json:"sourcePrefix"`
	TargetConnection string   `json:"targetConnection"`
	TargetPrefix     string   `json:"targetPrefix"`
	Exclude          []string `json:"exclude"`
	Overwrite        bool     `json:"overwrite"`
	Enabled          bool     `json:"enabled"`
	Deleted          bool     `json:"-"`
	Created          string   `json:"created,omitempty"`
}

type MigrationItem struct {
	SourcePath     string `json:"sourcePath"`
	TargetPath     string `json:"targetPath"`
	SourceETag     string `json:"sourceEtag"`
	SourceSize     int64  `json:"sourceSize"`
	SourceHash     string `json:"sourceHash,omitempty"`
	DestBeforeETag string `json:"destBeforeEtag,omitempty"`
	DestAbsent     bool   `json:"destAbsent"`
	DestETag       string `json:"destEtag,omitempty"`
	State          string `json:"state"`
	Error          string `json:"error,omitempty"`
}

type MigrationPreview struct {
	JobID       string          `json:"jobId"`
	Token       string          `json:"token"`
	Created     time.Time       `json:"created"`
	SourceFiles int             `json:"sourceFiles"`
	Conflicts   int             `json:"conflicts"`
	Bytes       int64           `json:"bytes"`
	Items       []MigrationItem `json:"items"`
}

type MigrationRun struct {
	ID        string `json:"id"`
	JobID     string `json:"jobId"`
	State     string `json:"state"`
	Started   string `json:"started"`
	Updated   string `json:"updated"`
	Total     int    `json:"filesTotal"`
	Done      int    `json:"filesDone"`
	Failed    int    `json:"filesFailed"`
	Cancelled bool   `json:"cancelled"`
	Detail    string `json:"detail,omitempty"`
}

type MigrationCleanupPreview struct {
	RunID   string          `json:"runId"`
	Token   string          `json:"token"`
	Created time.Time       `json:"created"`
	Items   []MigrationItem `json:"items"`
}

var migrationPlanLocks sync.Map     // map[service+job]*sync.Mutex
var migrationRunLocks sync.Map      // map[service+run]*sync.Mutex
var migrationRunCancels sync.Map    // map[service+run]context.CancelFunc
var migrationRunStateLocks sync.Map // map[service+run]*sync.Mutex

func (s *Service) ensureMigrationSchema() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS migration_jobs (
	 id TEXT PRIMARY KEY, name TEXT NOT NULL, source_connection TEXT NOT NULL, source_prefix TEXT NOT NULL,
	 target_connection TEXT NOT NULL, target_prefix TEXT NOT NULL, overwrite INTEGER NOT NULL,
	 created TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, deleted INTEGER NOT NULL DEFAULT 0,
	 exclude_json TEXT NOT NULL DEFAULT '[".tamiops-backup"]');
	 CREATE TABLE IF NOT EXISTS migration_runs (
	 id TEXT PRIMARY KEY, job_id TEXT NOT NULL, state TEXT NOT NULL, preview_token TEXT NOT NULL,
	 started TEXT NOT NULL, updated TEXT NOT NULL, files_total INTEGER NOT NULL, files_done INTEGER NOT NULL,
	 files_failed INTEGER NOT NULL, cancel_requested INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT '');
	 CREATE TABLE IF NOT EXISTS migration_items (
	 run_id TEXT NOT NULL, source_path TEXT NOT NULL, target_path TEXT NOT NULL, source_etag TEXT NOT NULL,
	 source_size INTEGER NOT NULL, source_hash TEXT NOT NULL DEFAULT '', dest_before_etag TEXT NOT NULL DEFAULT '',
	 dest_absent INTEGER NOT NULL DEFAULT 0, dest_etag TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
	 error TEXT NOT NULL DEFAULT '', PRIMARY KEY(run_id,source_path));
	 CREATE TABLE IF NOT EXISTS migration_plans (
	 job_id TEXT PRIMARY KEY, token TEXT NOT NULL, data TEXT NOT NULL, created TEXT NOT NULL);
	 CREATE TABLE IF NOT EXISTS migration_cleanup_plans (
	 run_id TEXT PRIMARY KEY, token TEXT NOT NULL, data TEXT NOT NULL, created TEXT NOT NULL);`)
	if err != nil {
		return err
	}
	if err = s.ensureMigrationEnabledColumn(); err != nil {
		return err
	}
	if err = s.ensureMigrationDeletedColumn(); err != nil {
		return err
	}
	if err = s.ensureMigrationDestAbsentColumn(); err != nil {
		return err
	}
	return s.ensureMigrationExcludeColumn()
}

func (s *Service) ensureMigrationExcludeColumn() error {
	rows, err := s.db.Query("PRAGMA table_info(migration_jobs)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "exclude_json" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !found {
		if _, err = s.db.Exec(`ALTER TABLE migration_jobs ADD COLUMN exclude_json TEXT NOT NULL DEFAULT '[".tamiops-backup"]'`); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`UPDATE migration_jobs SET exclude_json='[".tamiops-backup"]' WHERE exclude_json IS NULL OR exclude_json='' OR exclude_json='[]'`)
	return err
}

func defaultMigrationExcludes(patterns []string) []string {
	out := []string{".tamiops-backup"}
	seen := map[string]bool{".tamiops-backup": true}
	for _, raw := range patterns {
		pattern := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
		if pattern == "" || seen[pattern] {
			continue
		}
		seen[pattern] = true
		out = append(out, pattern)
	}
	return out
}

func equalMigrationExcludes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateMigrationLocations(sourceConn, targetConn Connection, j MigrationJob) error {
	if targetConn.Kind == "demo" && sourceConn.Kind != "demo" {
		return errors.New("真实来源不能迁移到退出后清空的演示空间")
	}
	nsSource, pSource := resource(sourceConn, j.SourcePrefix)
	nsTarget, pTarget := resource(targetConn, j.TargetPrefix)
	if isManagedPath(pSource) || isManagedPath(pTarget) {
		return errors.New("迁移路径不能直接指向备份保留目录")
	}
	if nsSource == nsTarget && pathsOverlap(pSource, pTarget) {
		return errors.New("源和目标连接指向同一命名空间且前缀重叠")
	}
	return nil
}

func (s *Service) validateMigrationJobLocations(j MigrationJob) error {
	sourceConn, err := s.connection(j.SourceConnection)
	if err != nil {
		return err
	}
	targetConn, err := s.connection(j.TargetConnection)
	if err != nil {
		return err
	}
	return validateMigrationLocations(sourceConn, targetConn, j)
}

func (s *Service) ensureMigrationDeletedColumn() error {
	rows, err := s.db.Query("PRAGMA table_info(migration_jobs)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "deleted" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || found {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE migration_jobs ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0")
	return err
}

func (s *Service) ensureMigrationEnabledColumn() error {
	rows, err := s.db.Query("PRAGMA table_info(migration_jobs)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "enabled" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || found {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE migration_jobs ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1")
	return err
}

func (s *Service) ensureMigrationDestAbsentColumn() error {
	rows, err := s.db.Query("PRAGMA table_info(migration_items)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "dest_absent" {
			found = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || found {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE migration_items ADD COLUMN dest_absent INTEGER NOT NULL DEFAULT 0")
	return err
}

func migrationKey(s *Service, id string) string { return fmt.Sprintf("%p:%s", s, id) }

func migrationJobLock(s *Service, id string) *sync.Mutex {
	lockVal, _ := migrationPlanLocks.LoadOrStore(migrationKey(s, id), &sync.Mutex{})
	return lockVal.(*sync.Mutex)
}

func migrationRunStateLock(s *Service, runID string) *sync.Mutex {
	lockVal, _ := migrationRunStateLocks.LoadOrStore(migrationKey(s, runID), &sync.Mutex{})
	return lockVal.(*sync.Mutex)
}

func (s *Service) CreateMigrationJob(j MigrationJob) (MigrationJob, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return j, err
	}
	if j.ID == "" {
		j.ID = ID()
	}
	jobLock := migrationJobLock(s, j.ID)
	jobLock.Lock()
	defer jobLock.Unlock()
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 {
		return j, errors.New("请填写迁移任务名称")
	}
	j.Exclude = defaultMigrationExcludes(j.Exclude)
	if err := validateExcludePatterns(j.Exclude); err != nil {
		return j, err
	}
	if _, err := s.connection(j.SourceConnection); err != nil {
		return j, err
	}
	targetConn, err := s.connection(j.TargetConnection)
	if err != nil {
		return j, err
	}
	if err = validKey(j.SourcePrefix); err != nil {
		return j, err
	}
	if err = validKey(j.TargetPrefix); err != nil {
		return j, err
	}
	if j.SourcePrefix != "" {
		if err = validSyncPath(j.SourcePrefix); err != nil {
			return j, err
		}
	}
	if j.TargetPrefix != "" {
		if err = validSyncPath(j.TargetPrefix); err != nil {
			return j, err
		}
	}
	if !targetConn.Capabilities.ConditionalWrite {
		return j, errors.New("目标连接尚未验证条件写入能力")
	}
	sourceConn, err := s.connection(j.SourceConnection)
	if err != nil {
		return j, err
	}
	if err = validateMigrationLocations(sourceConn, targetConn, j); err != nil {
		return j, err
	}
	if j.Created == "" {
		j.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	j.Enabled = true
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM migration_jobs WHERE deleted=0").Scan(&count); err != nil {
		return j, err
	}
	if count >= migrationMaxJobs {
		return j, errors.New("迁移任务数量达到上限")
	}
	excludeJSON, err := json.Marshal(j.Exclude)
	if err != nil {
		return j, err
	}
	_, err = s.db.Exec(`INSERT INTO migration_jobs(id,name,source_connection,source_prefix,target_connection,target_prefix,overwrite,created,enabled,exclude_json) VALUES(?,?,?,?,?,?,?,?,1,?)`, j.ID, j.Name, j.SourceConnection, j.SourcePrefix, j.TargetConnection, j.TargetPrefix, backupBoolInt(j.Overwrite), j.Created, string(excludeJSON))
	return j, err
}

func pathsOverlap(a, b string) bool {
	a, b = strings.Trim(a, "/"), strings.Trim(b, "/")
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (s *Service) migrationJob(id string) (MigrationJob, error) {
	var j MigrationJob
	var overwrite int
	var enabled int
	var deleted int
	var excludeJSON string
	err := s.db.QueryRow(`SELECT id,name,source_connection,source_prefix,target_connection,target_prefix,overwrite,created,enabled,deleted,exclude_json FROM migration_jobs WHERE id=?`, id).Scan(&j.ID, &j.Name, &j.SourceConnection, &j.SourcePrefix, &j.TargetConnection, &j.TargetPrefix, &overwrite, &j.Created, &enabled, &deleted, &excludeJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationJob{}, storage.ErrNotFound
	}
	j.Overwrite = overwrite != 0
	j.Enabled = enabled != 0
	j.Deleted = deleted != 0
	if err == nil {
		if unmarshalErr := json.Unmarshal([]byte(excludeJSON), &j.Exclude); unmarshalErr != nil {
			return MigrationJob{}, errors.New("迁移排除规则数据损坏")
		}
		j.Exclude = defaultMigrationExcludes(j.Exclude)
	}
	return j, err
}

func (s *Service) UpdateMigrationJob(j MigrationJob) (MigrationJob, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return j, err
	}
	jobLock := migrationJobLock(s, j.ID)
	jobLock.Lock()
	defer jobLock.Unlock()
	old, err := s.migrationJob(j.ID)
	if err != nil {
		return j, err
	}
	if old.Deleted {
		return j, storage.ErrNotFound
	}
	var runCount int
	if err = s.db.QueryRow("SELECT count(*) FROM migration_runs WHERE job_id=?", j.ID).Scan(&runCount); err != nil {
		return j, err
	}
	runs, err := s.ListMigrationRuns(j.ID)
	if err != nil {
		return j, err
	}
	for _, run := range runs {
		if run.State == "running" || run.State == "cancel_requested" {
			return j, errors.New("迁移任务正在运行，不能修改")
		}
	}
	if j.Name == "" {
		j.Name = old.Name
	}
	if j.SourceConnection == "" {
		j.SourceConnection = old.SourceConnection
	}
	if j.SourcePrefix == "" && old.SourcePrefix != "" {
		j.SourcePrefix = old.SourcePrefix
	}
	if j.TargetConnection == "" {
		j.TargetConnection = old.TargetConnection
	}
	if j.TargetPrefix == "" && old.TargetPrefix != "" {
		j.TargetPrefix = old.TargetPrefix
	}
	if j.Created == "" {
		j.Created = old.Created
	}
	if j.Exclude == nil {
		j.Exclude = append([]string(nil), old.Exclude...)
	} else {
		j.Exclude = defaultMigrationExcludes(j.Exclude)
	}
	if runCount > 0 && (j.SourceConnection != old.SourceConnection || j.SourcePrefix != old.SourcePrefix || j.TargetConnection != old.TargetConnection || j.TargetPrefix != old.TargetPrefix || j.Overwrite != old.Overwrite || !equalMigrationExcludes(j.Exclude, old.Exclude)) {
		return j, errors.New("任务已有运行记录，源、目标和覆盖策略不可变更，以保护断点续传与源清理")
	}
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 {
		return j, errors.New("迁移任务名称无效")
	}
	if _, err = s.connection(j.SourceConnection); err != nil {
		return j, err
	}
	conn, err := s.connection(j.TargetConnection)
	if err != nil {
		return j, err
	}
	if !conn.Capabilities.ConditionalWrite {
		return j, errors.New("目标连接尚未验证条件写入能力")
	}
	if err = validKey(j.SourcePrefix); err != nil {
		return j, err
	}
	if err = validKey(j.TargetPrefix); err != nil {
		return j, err
	}
	if err = validateExcludePatterns(j.Exclude); err != nil {
		return j, err
	}
	srcConn, _ := s.connection(j.SourceConnection)
	if err = validateMigrationLocations(srcConn, conn, j); err != nil {
		return j, err
	}
	excludeJSON, err := json.Marshal(j.Exclude)
	if err != nil {
		return j, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE migration_jobs SET name=?,source_connection=?,source_prefix=?,target_connection=?,target_prefix=?,overwrite=?,enabled=?,exclude_json=? WHERE id=?`, j.Name, j.SourceConnection, j.SourcePrefix, j.TargetConnection, j.TargetPrefix, backupBoolInt(j.Overwrite), backupBoolInt(j.Enabled), string(excludeJSON), j.ID); err != nil {
		return j, err
	}
	if _, err = tx.Exec("DELETE FROM migration_plans WHERE job_id=?", j.ID); err != nil {
		return j, err
	}
	if err = tx.Commit(); err != nil {
		return j, err
	}
	return j, nil
}

func (s *Service) DeleteMigrationJob(id string) error {
	if err := s.ensureMigrationSchema(); err != nil {
		return err
	}
	jobLock := migrationJobLock(s, id)
	jobLock.Lock()
	defer jobLock.Unlock()
	rows, err := s.ListMigrationRuns(id)
	if err != nil {
		return err
	}
	for _, run := range rows {
		if run.State == "running" || run.State == "cancel_requested" {
			return errors.New("迁移任务正在运行，不能删除")
		}
	}
	_, err = s.db.Exec("UPDATE migration_jobs SET enabled=0,deleted=1 WHERE id=?", id)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("DELETE FROM migration_plans WHERE job_id=?", id)
	return err
}

func (s *Service) ListMigrationJobs() ([]MigrationJob, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT id FROM migration_jobs WHERE deleted=0 ORDER BY created,id")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]MigrationJob, 0, len(ids))
	for _, id := range ids {
		v, e := s.migrationJob(id)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) PreviewMigration(ctx context.Context, id string) (MigrationPreview, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return MigrationPreview{}, err
	}
	lock := migrationJobLock(s, id)
	lock.Lock()
	defer lock.Unlock()
	j, err := s.migrationJob(id)
	if err != nil {
		return MigrationPreview{}, err
	}
	plan, err := s.buildMigrationPreview(ctx, j)
	if err != nil {
		return MigrationPreview{}, err
	}
	plan.Token = migrationPreviewToken(j, plan.Items)
	raw, err := json.Marshal(plan)
	if err != nil {
		return MigrationPreview{}, err
	}
	_, err = s.db.Exec(`INSERT INTO migration_plans(job_id,token,data,created) VALUES(?,?,?,?) ON CONFLICT(job_id) DO UPDATE SET token=excluded.token,data=excluded.data,created=excluded.created`, id, plan.Token, string(raw), plan.Created.Format(time.RFC3339Nano))
	return plan, err
}

func (s *Service) buildMigrationPreview(ctx context.Context, j MigrationJob) (MigrationPreview, error) {
	j.Exclude = defaultMigrationExcludes(j.Exclude)
	if err := validateExcludePatterns(j.Exclude); err != nil {
		return MigrationPreview{}, err
	}
	sourceConn, err := s.connection(j.SourceConnection)
	if err != nil {
		return MigrationPreview{}, err
	}
	targetConn, err := s.connection(j.TargetConnection)
	if err != nil {
		return MigrationPreview{}, err
	}
	if err = validateMigrationLocations(sourceConn, targetConn, j); err != nil {
		return MigrationPreview{}, err
	}
	sourceStore, err := s.store(j.SourceConnection)
	if err != nil {
		return MigrationPreview{}, err
	}
	targetStore, err := s.store(j.TargetConnection)
	if err != nil {
		return MigrationPreview{}, err
	}
	sourceFiles, _, err := scanRemoteFiltered(ctx, sourceStore, j.SourcePrefix, j.Exclude)
	if err != nil {
		return MigrationPreview{}, err
	}
	targetFiles, targetDirs, err := scanRemoteFiltered(ctx, targetStore, j.TargetPrefix, j.Exclude)
	if errors.Is(err, storage.ErrNotFound) {
		targetFiles, targetDirs, err = map[string]storage.Entry{}, map[string]bool{}, nil
	}
	if err != nil {
		return MigrationPreview{}, err
	}
	if len(sourceFiles) > migrationMaxItems {
		return MigrationPreview{}, errors.New("迁移清单超过单任务文件数量上限")
	}
	keys := make([]string, 0, len(sourceFiles))
	for rel := range sourceFiles {
		keys = append(keys, rel)
	}
	sort.Strings(keys)
	preview := MigrationPreview{JobID: j.ID, Created: time.Now().UTC(), SourceFiles: len(keys), Items: []MigrationItem{}}
	portable := map[string]string{}
	for _, rel := range keys {
		entry := sourceFiles[rel]
		identity := portablePathIdentity(rel)
		if old, ok := portable[identity]; ok && old != rel {
			return MigrationPreview{}, fmt.Errorf("源中存在跨平台重名路径：%s / %s", old, rel)
		}
		portable[identity] = rel
		if entry.IsDir || !strongTag(entry.ETag) || entry.Size < 0 || entry.Size > s.Preferences().MaxFileBytes {
			return MigrationPreview{}, fmt.Errorf("源文件没有可验证版本或超过额度：%s", path.Base(rel))
		}
		destRel := rel
		targetKey := joinPrefix(j.TargetPrefix, destRel)
		item := MigrationItem{SourcePath: entry.Path, TargetPath: targetKey, SourceETag: entry.ETag, SourceSize: entry.Size, State: "planned"}
		if before, exists := targetFiles[rel]; exists {
			if before.IsDir {
				item.State, item.Error = "conflict", "目标路径是目录"
			} else {
				item.DestAbsent, item.DestBeforeETag = false, before.ETag
				if !j.Overwrite {
					item.State, item.Error = "conflict", "目标文件已存在"
				} else if !strongTag(before.ETag) {
					item.State, item.Error = "conflict", "目标没有可靠版本标识，不能安全覆盖"
				}
			}
		} else if targetDirs[rel] {
			item.State, item.Error = "conflict", "目标路径是目录"
		} else {
			item.DestAbsent = true
		}
		if item.State == "conflict" {
			preview.Conflicts++
		}
		preview.Bytes += entry.Size
		preview.Items = append(preview.Items, item)
	}
	return preview, nil
}

func joinPrefix(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return path.Join(prefix, key)
}

func migrationPreviewToken(j MigrationJob, items []MigrationItem) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00", j.ID, j.SourceConnection, j.SourcePrefix, j.TargetConnection, j.TargetPrefix, j.Overwrite)
	for _, exclude := range defaultMigrationExcludes(j.Exclude) {
		fmt.Fprintf(h, "exclude:%s\x00", exclude)
	}
	for _, item := range items {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%s\x00%t\x00%s\x00", item.SourcePath, item.TargetPath, item.SourceETag, item.SourceSize, item.DestBeforeETag, item.DestAbsent, item.State)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Service) StartMigration(ctx context.Context, id, token string) (MigrationRun, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return MigrationRun{}, err
	}
	jobLock := migrationJobLock(s, id)
	jobLock.Lock()
	defer jobLock.Unlock()
	var savedToken, data string
	if err := s.db.QueryRow("SELECT token,data FROM migration_plans WHERE job_id=?", id).Scan(&savedToken, &data); err != nil {
		return MigrationRun{}, err
	}
	if token == "" || token != savedToken {
		return MigrationRun{}, storage.ErrConflict
	}
	var planned MigrationPreview
	if err := json.Unmarshal([]byte(data), &planned); err != nil || planned.Token != token {
		return MigrationRun{}, errors.New("迁移预览已损坏，请重新预览")
	}
	job, err := s.migrationJob(id)
	if err != nil {
		return MigrationRun{}, err
	}
	if !job.Enabled || job.Deleted {
		return MigrationRun{}, errors.New("迁移任务已暂停或删除")
	}
	targetConnection, err := s.connection(job.TargetConnection)
	if err != nil {
		return MigrationRun{}, err
	}
	if !targetConnection.Capabilities.ConditionalWrite {
		return MigrationRun{}, errors.New("目标连接尚未重新验证条件写入能力")
	}
	var runCount int
	if err = s.db.QueryRow("SELECT count(*) FROM migration_runs WHERE job_id=?", id).Scan(&runCount); err != nil {
		return MigrationRun{}, err
	}
	if runCount >= migrationMaxRunsPerJob {
		return MigrationRun{}, errors.New("该任务的历史运行记录达到上限")
	}
	current, err := s.buildMigrationPreview(ctx, job)
	if err != nil {
		return MigrationRun{}, err
	}
	if migrationPreviewToken(job, current.Items) != token {
		return MigrationRun{}, storage.ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return MigrationRun{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	run := MigrationRun{ID: ID(), JobID: id, State: "queued", Started: now, Updated: now, Total: len(planned.Items)}
	tx, err := s.db.Begin()
	if err != nil {
		return MigrationRun{}, err
	}
	if _, err = tx.Exec(`INSERT INTO migration_runs(id,job_id,state,preview_token,started,updated,files_total,files_done,files_failed,cancel_requested,detail) VALUES(?,?,?, ?,?,?,?,0,0,0,'')`, run.ID, id, run.State, token, now, now, run.Total); err != nil {
		tx.Rollback()
		return MigrationRun{}, err
	}
	for _, item := range planned.Items {
		reason := item.Error
		if _, err = tx.Exec(`INSERT INTO migration_items(run_id,source_path,target_path,source_etag,source_size,source_hash,dest_before_etag,dest_absent,dest_etag,state,error) VALUES(?,?,?,?,?,'',?,?,?,'planned',?)`, run.ID, item.SourcePath, item.TargetPath, item.SourceETag, item.SourceSize, item.DestBeforeETag, backupBoolInt(item.DestAbsent), item.DestETag, reason); err != nil {
			tx.Rollback()
			return MigrationRun{}, err
		}
		if item.State == "conflict" {
			if _, err = tx.Exec(`UPDATE migration_items SET state='conflict' WHERE run_id=? AND source_path=?`, run.ID, item.SourcePath); err != nil {
				tx.Rollback()
				return MigrationRun{}, err
			}
			run.Failed++
		}
	}
	if err = tx.Commit(); err != nil {
		return MigrationRun{}, err
	}
	if err = s.launchMigration(run.ID); err != nil {
		run.State = "paused"
		run.Detail = "启动失败，可恢复重试"
		return run, err
	}
	run.State = "running"
	return run, nil
}

func (s *Service) launchMigration(runID string) error {
	key := migrationKey(s, runID)
	lockVal, _ := migrationRunLocks.LoadOrStore(key, &sync.Mutex{})
	lock := lockVal.(*sync.Mutex)
	if !lock.TryLock() {
		return errors.New("迁移任务已在运行")
	}
	taskCtx, finish, err := s.beginTask(context.Background())
	if err != nil {
		lock.Unlock()
		s.pauseMigrationLaunch(runID, err)
		return err
	}
	ctx, cancel := context.WithCancel(taskCtx)
	stateLock := migrationRunStateLock(s, runID)
	stateLock.Lock()
	migrationRunCancels.Store(key, cancel)
	_, err = s.db.Exec(`UPDATE migration_runs SET state='running',updated=?,cancel_requested=0 WHERE id=? AND state NOT IN ('cancelled','complete')`, time.Now().UTC().Format(time.RFC3339Nano), runID)
	if err == nil {
		// Check the persisted state before releasing the cancellation guard.
		var state string
		if queryErr := s.db.QueryRow("SELECT state FROM migration_runs WHERE id=?", runID).Scan(&state); queryErr != nil {
			err = queryErr
		} else if state != "running" {
			err = errors.New("迁移运行已取消或已结束")
		}
	}
	if err != nil {
		migrationRunCancels.Delete(key)
		stateLock.Unlock()
		cancel()
		lock.Unlock()
		finish()
		s.pauseMigrationLaunch(runID, err)
		return err
	}
	go func() {
		defer func() {
			stateLock.Lock()
			migrationRunCancels.Delete(key)
			stateLock.Unlock()
			cancel()
			finish()
			lock.Unlock()
		}()
		s.runMigration(ctx, runID)
	}()
	stateLock.Unlock()
	return nil
}

func (s *Service) pauseMigrationLaunch(runID string, cause error) {
	detail := "启动失败，可恢复重试"
	if cause != nil {
		detail += "：" + safeBackupFailure(cause)
	}
	_, _ = s.db.Exec(`UPDATE migration_runs SET state='paused',updated=?,detail=? WHERE id=? AND cancel_requested=0 AND state NOT IN ('complete','cancelled','uncertain')`, time.Now().UTC().Format(time.RFC3339Nano), detail, runID)
}

func (s *Service) runMigration(ctx context.Context, runID string) {
	_ = s.ensureMigrationSchema()
	run, job, err := s.migrationRunAndJob(runID)
	if err != nil {
		return
	}
	ctx = withTransferScope(ctx, "migration", job.ID)
	if err = s.validateMigrationJobLocations(job); err != nil {
		s.finishMigrationRun(runID, "partial", "迁移路径指向受保护备份目录，任务已停止")
		return
	}
	_, _ = s.db.Exec(`UPDATE migration_runs SET detail='' WHERE id=?`, runID)
	source := Backend{Service: s, ConnectionID: job.SourceConnection}
	target := Backend{Service: s, ConnectionID: job.TargetConnection}
	items, err := s.loadMigrationItems(runID)
	if err != nil {
		s.finishMigrationRun(runID, "partial", "无法读取迁移进度")
		return
	}
	for i := range items {
		item := &items[i]
		if item.State == "conflict" || item.State == "done" || item.State == "source_deleted" {
			continue
		}
		if isManagedPath(item.SourcePath) || isManagedPath(item.TargetPath) {
			item.State, item.Error = "failed", "文件路径指向受保护备份目录"
			_ = s.updateMigrationItem(runID, *item, item.State, item.Error)
			continue
		}
		if err = ctx.Err(); err != nil {
			_ = s.updateMigrationItem(runID, *item, "cancelled", "操作已取消")
			continue
		}
		if item.State == "uncertain" {
			resolvedItem, resolved, resolveErr := s.reconcileMigrationItem(ctx, source, target, *item)
			if resolveErr != nil || !resolved {
				_ = s.updateMigrationItem(runID, *item, "uncertain", "结果尚不能证明安全，请先核对 Core 操作日志")
				continue
			}
			*item = resolvedItem
			item.State = "done"
			_ = s.updateMigrationItem(runID, *item, "done", "")
			continue
		}
		transferred, e := s.transferMigrationItem(ctx, source, target, *item)
		*item = transferred
		if e != nil {
			state := "failed"
			uncertain := isUncertainMigrationError(s, job.TargetConnection, item.TargetPath, e)
			if uncertain {
				state = "uncertain"
			} else if item.SourceHash != "" {
				verifiedItem, verified, verifyErr := s.reconcileMigrationItem(ctx, source, target, *item)
				if verifyErr == nil && verified {
					*item = verifiedItem
					item.State = "done"
					_ = s.updateMigrationItem(runID, *item, "done", "")
					continue
				}
			}
			item.State = state
			item.Error = safeBackupFailure(e)
			_ = s.updateMigrationItem(runID, *item, state, item.Error)
			continue
		}
		item.State = "done"
		_ = s.updateMigrationItem(runID, *item, "done", "")
	}
	items, _ = s.loadMigrationItems(runID)
	state := "complete"
	done, failed := 0, 0
	for _, item := range items {
		if item.State == "done" || item.State == "source_deleted" {
			done++
		} else {
			failed++
			if item.State == "uncertain" {
				state = "uncertain"
			}
		}
	}
	if state != "uncertain" {
		if ctx.Err() != nil {
			state = "cancelled"
		} else if failed > 0 {
			state = "partial"
		}
	}
	_ = run
	detail := ""
	if failed > 0 {
		detail = "部分文件未迁移；源文件均予以保留"
	}
	_, _ = s.db.Exec(`UPDATE migration_runs SET state=?,updated=?,files_done=?,files_failed=?,detail=? WHERE id=?`, state, time.Now().UTC().Format(time.RFC3339Nano), done, failed, detail, runID)
}

func (s *Service) transferMigrationItem(ctx context.Context, source, target Backend, item MigrationItem) (MigrationItem, error) {
	if !strongTag(item.SourceETag) || item.SourceSize < 0 || item.SourceSize > s.Preferences().MaxFileBytes {
		return item, storage.ErrConflict
	}
	current, err := source.Stat(ctx, item.SourcePath)
	if err != nil {
		return item, err
	}
	if current.IsDir || current.ETag != item.SourceETag || current.Size != item.SourceSize {
		return item, storage.ErrConflict
	}
	input, opened, err := source.Open(ctx, item.SourcePath, item.SourceETag)
	if err != nil {
		return item, err
	}
	if opened.IsDir || opened.ETag != item.SourceETag || opened.Size != item.SourceSize {
		input.Close()
		return item, storage.ErrConflict
	}
	stage, err := s.spool(ctx, input, item.SourceSize)
	closeErr := input.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return item, err
	}
	defer func() { stage.Close(); osRemoveStage(stage.Name()) }()
	hash, size, err := fileSHA256(stage)
	if err != nil {
		return item, err
	}
	if size != item.SourceSize || item.SourceHash != "" && item.SourceHash != hash {
		return item, storage.ErrConflict
	}
	item.SourceHash = hash
	latest, err := source.Stat(ctx, item.SourcePath)
	if err != nil || latest.ETag != item.SourceETag || latest.Size != item.SourceSize {
		if err != nil {
			return item, err
		}
		return item, storage.ErrConflict
	}
	if err = ensureRemoteDirectory(ctx, target, path.Dir(item.TargetPath)); err != nil {
		return item, err
	}
	condition := storage.Condition{IfNoneMatch: true}
	if !item.DestAbsent {
		if !strongTag(item.DestBeforeETag) {
			return item, storage.ErrConflict
		}
		condition = storage.Condition{IfMatch: item.DestBeforeETag}
	}
	if _, err = stage.Seek(0, io.SeekStart); err != nil {
		return item, err
	}
	entry, err := target.put(ctx, item.TargetPath, stage, size, condition, hash)
	if err != nil {
		return item, err
	}
	if !strongTag(entry.ETag) {
		return item, errors.New("目标提交缺少可靠版本标识")
	}
	verified, actual, err := hashRemote(ctx, target, item.TargetPath, entry.ETag)
	if err != nil {
		return item, err
	}
	if verified.Hash != hash || verified.Size != size || actual.ETag != entry.ETag {
		return item, errors.New("目标副本未通过内容核验")
	}
	item.DestETag = entry.ETag
	return item, nil
}

func isUncertainMigrationError(s *Service, connectionID, key string, err error) bool {
	if err == nil {
		return false
	}
	conn, e := s.connection(connectionID)
	if e != nil {
		return false
	}
	ns, resourcePath := resource(conn, key)
	var state string
	e = s.db.QueryRow(`SELECT state FROM operations WHERE state IN ('uncertain','committing') AND ((connection=? AND path=?) OR EXISTS(SELECT 1 FROM operation_resources r WHERE r.operation_id=operations.id AND r.connection=? AND r.path=?)) ORDER BY created DESC LIMIT 1`, ns, resourcePath, ns, resourcePath).Scan(&state)
	return e == nil && (state == "uncertain" || state == "committing")
}

func (s *Service) reconcileMigrationItem(ctx context.Context, source, target Backend, item MigrationItem) (MigrationItem, bool, error) {
	if item.SourceHash == "" {
		return item, false, nil
	}
	src, err := source.Stat(ctx, item.SourcePath)
	if err != nil || src.ETag != item.SourceETag || src.Size != item.SourceSize {
		if err != nil {
			return item, false, err
		}
		return item, false, storage.ErrConflict
	}
	dst, err := target.Stat(ctx, item.TargetPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) && item.DestAbsent {
			return item, false, nil
		}
		return item, false, err
	}
	if dst.IsDir || !strongTag(dst.ETag) {
		return item, false, storage.ErrConflict
	}
	hash, actual, err := hashRemote(ctx, target, item.TargetPath, dst.ETag)
	if err != nil {
		return item, false, err
	}
	if hash.Hash == item.SourceHash && hash.Size == item.SourceSize && actual.ETag == dst.ETag {
		item.DestETag = dst.ETag
		return item, true, nil
	}
	if item.DestAbsent || dst.ETag != item.DestBeforeETag {
		return item, false, storage.ErrConflict
	}
	return item, false, nil
}

func (s *Service) migrationRunAndJob(runID string) (MigrationRun, MigrationJob, error) {
	var run MigrationRun
	var cancel int
	err := s.db.QueryRow(`SELECT id,job_id,state,started,updated,files_total,files_done,files_failed,cancel_requested,detail FROM migration_runs WHERE id=?`, runID).Scan(&run.ID, &run.JobID, &run.State, &run.Started, &run.Updated, &run.Total, &run.Done, &run.Failed, &cancel, &run.Detail)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationRun{}, MigrationJob{}, storage.ErrNotFound
	}
	if err != nil {
		return MigrationRun{}, MigrationJob{}, err
	}
	run.Cancelled = cancel != 0
	job, err := s.migrationJob(run.JobID)
	return run, job, err
}

func (s *Service) loadMigrationItems(runID string) ([]MigrationItem, error) {
	rows, err := s.db.Query(`SELECT source_path,target_path,source_etag,source_size,source_hash,dest_before_etag,dest_absent,dest_etag,state,error FROM migration_items WHERE run_id=? ORDER BY source_path`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []MigrationItem{}
	for rows.Next() {
		var item MigrationItem
		var absent int
		if err = rows.Scan(&item.SourcePath, &item.TargetPath, &item.SourceETag, &item.SourceSize, &item.SourceHash, &item.DestBeforeETag, &absent, &item.DestETag, &item.State, &item.Error); err != nil {
			return nil, err
		}
		item.DestAbsent = absent != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) updateMigrationItem(runID string, item MigrationItem, state, message string) error {
	_, err := s.db.Exec(`UPDATE migration_items SET source_hash=?,dest_etag=?,state=?,error=? WHERE run_id=? AND source_path=?`, item.SourceHash, item.DestETag, state, message, runID, item.SourcePath)
	_, runErr := s.db.Exec(`UPDATE migration_runs SET updated=?,files_done=(SELECT count(*) FROM migration_items WHERE run_id=? AND state IN ('done','source_deleted')),files_failed=(SELECT count(*) FROM migration_items WHERE run_id=? AND state NOT IN ('planned','done','source_deleted')) WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), runID, runID, runID)
	if err != nil {
		return err
	}
	return runErr
}

func (s *Service) finishMigrationRun(runID, state, detail string) {
	_, _ = s.db.Exec(`UPDATE migration_runs SET state=?,updated=?,detail=? WHERE id=?`, state, time.Now().UTC().Format(time.RFC3339Nano), detail, runID)
}

func (s *Service) ListMigrationRuns(jobID string) ([]MigrationRun, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,job_id,state,started,updated,files_total,files_done,files_failed,cancel_requested,detail FROM migration_runs WHERE job_id=? ORDER BY started DESC LIMIT ?`, jobID, backupMaxSnapshots)
	if err != nil {
		return nil, err
	}
	out := []MigrationRun{}
	for rows.Next() {
		var r MigrationRun
		var cancel int
		if err = rows.Scan(&r.ID, &r.JobID, &r.State, &r.Started, &r.Updated, &r.Total, &r.Done, &r.Failed, &cancel, &r.Detail); err != nil {
			rows.Close()
			return nil, err
		}
		r.Cancelled = cancel != 0
		out = append(out, r)
	}
	err = rows.Err()
	rows.Close()
	return out, err
}

func (s *Service) ResumeMigration(ctx context.Context, runID string) (MigrationRun, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return MigrationRun{}, err
	}
	run, job, err := s.migrationRunAndJob(runID)
	if err != nil {
		return MigrationRun{}, err
	}
	jobLock := migrationJobLock(s, job.ID)
	jobLock.Lock()
	defer jobLock.Unlock()
	run, job, err = s.migrationRunAndJob(runID)
	if err != nil {
		return MigrationRun{}, err
	}
	if ctx.Err() != nil {
		return run, ctx.Err()
	}
	if job.Deleted || !job.Enabled {
		return run, errors.New("迁移任务已暂停或删除")
	}
	if run.State == "complete" {
		return run, errors.New("已完成的迁移不能恢复")
	}
	if run.State == "cancelled" {
		return run, errors.New("已取消的迁移不能恢复")
	}
	if run.State == "uncertain" {
		items, e := s.loadMigrationItems(runID)
		if e != nil {
			return run, e
		}
		for _, item := range items {
			if item.State == "uncertain" && isUncertainMigrationError(s, job.TargetConnection, item.TargetPath, errors.New("uncertain")) {
				return run, errors.New("请先在待核对操作中确认远端提交结果")
			}
		}
	}
	if err = s.launchMigration(runID); err != nil {
		run.State = "paused"
		run.Detail = "启动失败，可恢复重试"
		return run, err
	}
	run.State = "running"
	return run, nil
}

func (s *Service) CancelMigration(runID string) error {
	if err := s.ensureMigrationSchema(); err != nil {
		return err
	}
	if _, _, err := s.migrationRunAndJob(runID); err != nil {
		return err
	}
	stateLock := migrationRunStateLock(s, runID)
	stateLock.Lock()
	defer stateLock.Unlock()
	key := migrationKey(s, runID)
	_, live := migrationRunCancels.Load(key)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var err error
	if live {
		_, err = s.db.Exec(`UPDATE migration_runs SET cancel_requested=1,state='cancel_requested',updated=? WHERE id=? AND state IN ('queued','running','paused','cancel_requested')`, now, runID)
		if v, ok := migrationRunCancels.Load(key); ok {
			v.(context.CancelFunc)()
		}
	} else {
		_, err = s.db.Exec(`UPDATE migration_runs SET cancel_requested=1,state='cancelled',updated=?,detail='已取消，未启动的运行不会自动执行' WHERE id=? AND state IN ('queued','paused','cancel_requested','running')`, now, runID)
	}
	return err
}

func (s *Service) PreviewMigrationCleanup(ctx context.Context, runID string) (MigrationCleanupPreview, error) {
	if err := s.ensureMigrationSchema(); err != nil {
		return MigrationCleanupPreview{}, err
	}
	run, job, err := s.migrationRunAndJob(runID)
	if err != nil {
		return MigrationCleanupPreview{}, err
	}
	jobLock := migrationJobLock(s, job.ID)
	jobLock.Lock()
	defer jobLock.Unlock()
	run, job, err = s.migrationRunAndJob(runID)
	if err != nil {
		return MigrationCleanupPreview{}, err
	}
	ctx = withTransferScope(ctx, "migration", job.ID)
	if run.State != "complete" && run.State != "partial" {
		return MigrationCleanupPreview{}, errors.New("迁移尚未完成，不能预览源清理")
	}
	if err = s.validateMigrationJobLocations(job); err != nil {
		return MigrationCleanupPreview{}, err
	}
	sourceConnection, err := s.connection(job.SourceConnection)
	if err != nil {
		return MigrationCleanupPreview{}, err
	}
	if !sourceConnection.Capabilities.ConditionalDelete {
		return MigrationCleanupPreview{}, errors.New("源连接尚未验证条件删除能力，已保留所有源文件")
	}
	source := Backend{Service: s, ConnectionID: job.SourceConnection}
	target := Backend{Service: s, ConnectionID: job.TargetConnection}
	items, err := s.loadMigrationItems(runID)
	if err != nil {
		return MigrationCleanupPreview{}, err
	}
	preview := MigrationCleanupPreview{RunID: runID, Created: time.Now().UTC(), Items: []MigrationItem{}}
	for _, item := range items {
		if item.State != "done" && item.State != "cleanup_failed" && item.State != "cleanup_uncertain" {
			continue
		}
		if isManagedPath(item.SourcePath) || isManagedPath(item.TargetPath) {
			return MigrationCleanupPreview{}, errors.New("迁移记录包含受保护备份目录中的路径，已停止源清理")
		}
		src, sourceErr := source.Stat(ctx, item.SourcePath)
		if sourceErr != nil && !errors.Is(sourceErr, storage.ErrNotFound) {
			return MigrationCleanupPreview{}, fmt.Errorf("源文件无法验证：%s", path.Base(item.SourcePath))
		}
		if sourceErr == nil {
			if src.IsDir || src.ETag != item.SourceETag || src.Size != item.SourceSize {
				return MigrationCleanupPreview{}, fmt.Errorf("源文件已变化，不能清理：%s", path.Base(item.SourcePath))
			}
			if isUncertainMigrationError(s, job.SourceConnection, item.SourcePath, errors.New("uncertain")) {
				return MigrationCleanupPreview{}, errors.New("源删除结果仍待核对，不能重复清理")
			}
			hash, _, hashErr := hashRemote(ctx, source, item.SourcePath, item.SourceETag)
			if hashErr != nil || item.SourceHash == "" || hash.Hash != item.SourceHash || hash.Size != item.SourceSize {
				return MigrationCleanupPreview{}, fmt.Errorf("源文件内容与已迁移版本不同：%s", path.Base(item.SourcePath))
			}
		}
		dst, destErr := target.Stat(ctx, item.TargetPath)
		if destErr != nil || dst.IsDir || !strongTag(item.DestETag) || dst.ETag != item.DestETag {
			return MigrationCleanupPreview{}, fmt.Errorf("目标文件已变化，不能清理源文件：%s", path.Base(item.TargetPath))
		}
		destHash, _, hashErr := hashRemote(ctx, target, item.TargetPath, item.DestETag)
		if hashErr != nil || destHash.Hash != item.SourceHash || destHash.Size != item.SourceSize {
			return MigrationCleanupPreview{}, fmt.Errorf("目标文件内容核验失败：%s", path.Base(item.TargetPath))
		}
		if errors.Is(sourceErr, storage.ErrNotFound) {
			_ = s.updateMigrationItem(runID, item, "source_deleted", "")
			continue
		}
		preview.Items = append(preview.Items, item)
	}
	preview.Token = migrationCleanupToken(runID, job, preview.Items)
	raw, err := json.Marshal(preview)
	if err != nil {
		return MigrationCleanupPreview{}, err
	}
	_, err = s.db.Exec(`INSERT INTO migration_cleanup_plans(run_id,token,data,created) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET token=excluded.token,data=excluded.data,created=excluded.created`, runID, preview.Token, string(raw), preview.Created.Format(time.RFC3339Nano))
	return preview, err
}

func migrationCleanupToken(runID string, job MigrationJob, items []MigrationItem) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", runID, job.SourceConnection, job.SourcePrefix, job.TargetConnection, job.TargetPrefix)
	for _, exclude := range defaultMigrationExcludes(job.Exclude) {
		fmt.Fprintf(h, "exclude:%s\x00", exclude)
	}
	for _, item := range items {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", item.SourcePath, item.SourceETag, item.SourceHash, item.TargetPath, item.DestETag)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Service) CleanupMigrationSource(ctx context.Context, runID, token string) error {
	if err := s.ensureMigrationSchema(); err != nil {
		return err
	}
	_, initialJob, err := s.migrationRunAndJob(runID)
	if err != nil {
		return err
	}
	jobLock := migrationJobLock(s, initialJob.ID)
	jobLock.Lock()
	defer jobLock.Unlock()
	var savedToken, data string
	if err := s.db.QueryRow("SELECT token,data FROM migration_cleanup_plans WHERE run_id=?", runID).Scan(&savedToken, &data); err != nil {
		return err
	}
	if token == "" || token != savedToken {
		return storage.ErrConflict
	}
	var planned MigrationCleanupPreview
	if err := json.Unmarshal([]byte(data), &planned); err != nil || planned.RunID != runID || planned.Token != token {
		return errors.New("源清理预览损坏，请重新预览")
	}
	run, job, err := s.migrationRunAndJob(runID)
	if err != nil {
		return err
	}
	ctx = withTransferScope(ctx, "migration", job.ID)
	if run.State != "complete" && run.State != "partial" {
		return errors.New("迁移状态不允许清理源文件")
	}
	if err = s.validateMigrationJobLocations(job); err != nil {
		return err
	}
	if migrationCleanupToken(runID, job, planned.Items) != token {
		return storage.ErrConflict
	}
	source := Backend{Service: s, ConnectionID: job.SourceConnection}
	target := Backend{Service: s, ConnectionID: job.TargetConnection}
	for _, item := range planned.Items {
		if isManagedPath(item.SourcePath) || isManagedPath(item.TargetPath) {
			return errors.New("迁移记录包含受保护备份目录中的路径，已停止源清理")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		s.writes.Lock()
		err = s.revalidateAndDeleteMigrationSource(ctx, source, target, item)
		s.writes.Unlock()
		if err != nil {
			state := "cleanup_failed"
			if isUncertainMigrationError(s, job.SourceConnection, item.SourcePath, err) {
				state = "cleanup_uncertain"
			}
			_ = s.updateMigrationItem(runID, item, state, safeBackupFailure(err))
			return fmt.Errorf("源清理在 %s 处停止；剩余源文件予以保留", path.Base(item.SourcePath))
		}
		_ = s.updateMigrationItem(runID, item, "source_deleted", "")
	}
	_, _ = s.db.Exec(`UPDATE migration_runs SET updated=?,detail='目标已核验；预览中的源文件已清理' WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), runID)
	return nil
}

func (s *Service) revalidateAndDeleteMigrationSource(ctx context.Context, source, target Backend, item MigrationItem) error {
	srcStore, sourceConn, err := s.writable(source.ConnectionID, item.SourcePath, true)
	if err != nil {
		return err
	}
	if err = s.checkDAVLocks(ctx, sourceConn.ID, item.SourcePath); err != nil {
		return err
	}
	targetStore, targetConn, err := s.writable(target.ConnectionID, item.TargetPath, false)
	if err != nil {
		return err
	}
	if err = s.checkDAVLocks(ctx, targetConn.ID, item.TargetPath); err != nil {
		return err
	}
	src, err := srcStore.Stat(ctx, item.SourcePath)
	if err != nil {
		return err
	}
	if src.IsDir || src.ETag != item.SourceETag || src.Size != item.SourceSize {
		return storage.ErrConflict
	}
	sourceHash, _, err := hashRemote(ctx, source, item.SourcePath, item.SourceETag)
	if err != nil || sourceHash.Hash != item.SourceHash || sourceHash.Size != item.SourceSize {
		if err != nil {
			return err
		}
		return storage.ErrConflict
	}
	dst, err := targetStore.Stat(ctx, item.TargetPath)
	if err != nil {
		return err
	}
	if dst.IsDir || dst.ETag != item.DestETag || !strongTag(dst.ETag) {
		return storage.ErrConflict
	}
	destHash, _, err := hashRemote(ctx, target, item.TargetPath, item.DestETag)
	if err != nil || destHash.Hash != item.SourceHash || destHash.Size != item.SourceSize {
		if err != nil {
			return err
		}
		return storage.ErrConflict
	}
	return source.deleteLocked(ctx, srcStore, sourceConn, item.SourcePath, storage.Condition{IfMatch: item.SourceETag})
}
