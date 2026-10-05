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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"tamiops/internal/storage"
	"time"
)

const (
	backupManifestVersion = 1
	backupMaxManifestSize = 8 << 20
	backupMaxSnapshots    = 10000
	backupMaxJobs         = 500
)

func backupBoolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// BackupJob describes a local tree that is copied into a reserved, application-owned
// remote namespace. Existing snapshots are immutable and source files are never removed.
type BackupJob struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ConnectionID    string   `json:"connectionId"`
	LocalPath       string   `json:"localPath"`
	RemotePrefix    string   `json:"remotePrefix"`
	Exclude         []string `json:"exclude"`
	ScheduleMinutes int      `json:"scheduleMinutes"`
	RetainRecent    int      `json:"retainRecent"`
	RetainDaily     int      `json:"retainDaily"`
	RetainMonthly   int      `json:"retainMonthly"`
	Enabled         bool     `json:"enabled"`
	LastRun         string   `json:"lastRun,omitempty"`
	Status          string   `json:"status,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	Created         string   `json:"created,omitempty"`
}

type BackupFile struct {
	Path       string    `json:"path"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	RemoteETag string    `json:"remoteEtag,omitempty"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
	Modified   time.Time `json:"modified,omitempty"`
}

type BackupSnapshot struct {
	ID               string       `json:"id"`
	JobID            string       `json:"jobId"`
	RemoteConnection string       `json:"remoteConnection,omitempty"`
	RemotePrefix     string       `json:"remotePrefix,omitempty"`
	Started          string       `json:"started"`
	Completed        string       `json:"completed,omitempty"`
	Status           string       `json:"status"`
	FilesTotal       int          `json:"filesTotal"`
	FilesDone        int          `json:"filesDone"`
	FilesFailed      int          `json:"filesFailed"`
	BytesTotal       int64        `json:"bytesTotal"`
	Error            string       `json:"error,omitempty"`
	Files            []BackupFile `json:"files,omitempty"`
}

type BackupPreview struct {
	JobID    string       `json:"jobId"`
	Token    string       `json:"token"`
	Created  time.Time    `json:"created"`
	Files    []BackupFile `json:"files"`
	Bytes    int64        `json:"bytes"`
	Excluded int          `json:"excluded"`
}

type backupManifest struct {
	Version   int          `json:"version"`
	Snapshot  string       `json:"snapshot"`
	JobID     string       `json:"jobId"`
	Started   string       `json:"started"`
	Completed string       `json:"completed,omitempty"`
	Status    string       `json:"status"`
	Files     []BackupFile `json:"files"`
	Bytes     int64        `json:"bytes"`
	Error     string       `json:"error,omitempty"`
}

var backupJobLocks sync.Map // map[string]*sync.Mutex; keyed by service identity and job ID.

func (s *Service) ensureBackupSchema() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS backup_jobs (
	 id TEXT PRIMARY KEY, name TEXT NOT NULL, connection TEXT NOT NULL, local_path TEXT NOT NULL,
	 remote_prefix TEXT NOT NULL, exclude_json TEXT NOT NULL, schedule_minutes INTEGER NOT NULL,
	 retain_recent INTEGER NOT NULL, retain_daily INTEGER NOT NULL, retain_monthly INTEGER NOT NULL,
	 enabled INTEGER NOT NULL, last_run TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'ready',
	 detail TEXT NOT NULL DEFAULT '', created TEXT NOT NULL);
	 CREATE TABLE IF NOT EXISTS backup_snapshots (
	 id TEXT PRIMARY KEY, job_id TEXT NOT NULL, remote_connection TEXT NOT NULL DEFAULT '', remote_prefix TEXT NOT NULL DEFAULT '',
	 started TEXT NOT NULL, completed TEXT NOT NULL DEFAULT '',
	 status TEXT NOT NULL, files_total INTEGER NOT NULL, files_done INTEGER NOT NULL,
	 files_failed INTEGER NOT NULL, bytes_total INTEGER NOT NULL, manifest_version INTEGER NOT NULL DEFAULT 1,
	 error TEXT NOT NULL DEFAULT '');
	 CREATE TABLE IF NOT EXISTS backup_items (
	 snapshot_id TEXT NOT NULL, path TEXT NOT NULL, sha256 TEXT NOT NULL, size INTEGER NOT NULL,
	 remote_etag TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', modified TEXT NOT NULL DEFAULT '',
	 PRIMARY KEY(snapshot_id,path));
	 CREATE TABLE IF NOT EXISTS backup_plans (
	 job_id TEXT PRIMARY KEY, token TEXT NOT NULL, data TEXT NOT NULL, created TEXT NOT NULL);`)
	if err != nil {
		return err
	}
	return s.ensureBackupSnapshotLocationColumns()
}

func (s *Service) ensureBackupSnapshotLocationColumns() error {
	columns, err := backupTableColumns(s.db, "backup_snapshots")
	if err != nil {
		return err
	}
	if !columns["manifest_version"] {
		if _, err = s.db.Exec("ALTER TABLE backup_snapshots ADD COLUMN manifest_version INTEGER NOT NULL DEFAULT 1"); err != nil {
			return err
		}
	}
	if !columns["remote_connection"] {
		if _, err = s.db.Exec("ALTER TABLE backup_snapshots ADD COLUMN remote_connection TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if !columns["remote_prefix"] {
		if _, err = s.db.Exec("ALTER TABLE backup_snapshots ADD COLUMN remote_prefix TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`UPDATE backup_snapshots SET
	 remote_connection=COALESCE(NULLIF(remote_connection,''),(SELECT connection FROM backup_jobs WHERE backup_jobs.id=backup_snapshots.job_id),''),
	 remote_prefix=COALESCE(NULLIF(remote_prefix,''),(SELECT remote_prefix FROM backup_jobs WHERE backup_jobs.id=backup_snapshots.job_id),'')
	 WHERE remote_connection='' OR remote_prefix=''`)
	return err
}

func backupTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func backupLockKey(s *Service, id string) string { return fmt.Sprintf("%p:%s", s, id) }

func (s *Service) lockBackupJob(id string) func() {
	v, _ := backupJobLocks.LoadOrStore(backupLockKey(s, id), &sync.Mutex{})
	m := v.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

func (s *Service) CreateBackupJob(j BackupJob) (BackupJob, error) {
	if err := s.ensureBackupSchema(); err != nil {
		return j, err
	}
	if j.ID == "" {
		j.ID = ID()
	}
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 {
		return j, errors.New("请填写备份任务名称")
	}
	connection, err := s.connection(j.ConnectionID)
	if err != nil {
		return j, err
	}
	if !canWriteStrict(connection) {
		return j, errors.New("备份连接尚未验证条件写入能力")
	}
	if err := validateBackupLocation(j.LocalPath, j.RemotePrefix); err != nil {
		return j, err
	}
	if err := validateExcludePatterns(j.Exclude); err != nil {
		return j, err
	}
	if j.ScheduleMinutes < 0 || j.ScheduleMinutes > 525600 {
		return j, errors.New("备份间隔需为 0 至 525600 分钟")
	}
	if j.RetainRecent == 0 && j.RetainDaily == 0 && j.RetainMonthly == 0 {
		j.RetainRecent = 5
		j.RetainDaily = 7
		j.RetainMonthly = 3
	}
	if !validRetention(j.RetainRecent) || !validRetention(j.RetainDaily) || !validRetention(j.RetainMonthly) {
		return j, errors.New("保留数量需在 0 至 1000 之间")
	}
	if j.RetainRecent+j.RetainDaily+j.RetainMonthly == 0 {
		return j, errors.New("至少保留近期、每日或每月快照中的一类")
	}
	if j.Created == "" {
		j.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if j.Status == "" {
		j.Status = "ready"
	}
	// Preserve the schedule toggle supplied by the user; manual runs remain available.
	if err := s.insertBackupJob(j); err != nil {
		return j, err
	}
	return j, nil
}

func validRetention(n int) bool { return n >= 0 && n <= 1000 }

func validateBackupLocation(local, remotePrefix string) error {
	if strings.TrimSpace(local) == "" {
		return errors.New("请选择本地备份目录")
	}
	absolute, err := filepath.Abs(local)
	if err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return errors.New("本地备份路径必须是可访问目录")
	}
	if err = validKey(remotePrefix); err != nil || remotePrefix == "" {
		return errors.New("请指定非根目录的专用远端备份前缀")
	}
	if err = validSyncPath(remotePrefix); err != nil {
		return fmt.Errorf("远端前缀路径无效：%w", err)
	}
	return nil
}

func (s *Service) insertBackupJob(j BackupJob) error {
	if err := s.ensureBackupSchema(); err != nil {
		return err
	}
	exclude, _ := json.Marshal(j.Exclude)
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM backup_jobs WHERE status!='deleted'").Scan(&count); err != nil {
		return err
	}
	if count >= backupMaxJobs {
		return errors.New("备份任务数量达到上限")
	}
	_, err := s.db.Exec(`INSERT INTO backup_jobs(id,name,connection,local_path,remote_prefix,exclude_json,schedule_minutes,retain_recent,retain_daily,retain_monthly,enabled,last_run,status,detail,created)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.Name, j.ConnectionID, j.LocalPath, j.RemotePrefix, string(exclude), j.ScheduleMinutes, j.RetainRecent, j.RetainDaily, j.RetainMonthly, backupBoolInt(j.Enabled), j.LastRun, j.Status, j.Detail, j.Created)
	return err
}

func (s *Service) UpdateBackupJob(j BackupJob) (BackupJob, error) {
	unlock := s.lockBackupJob(j.ID)
	defer unlock()
	if err := s.ensureBackupSchema(); err != nil {
		return j, err
	}
	old, err := s.backupJob(j.ID)
	if err != nil {
		return j, err
	}
	if old.Status == "deleted" {
		return j, storage.ErrNotFound
	}
	if j.Name == "" {
		j.Name = old.Name
	}
	if j.ConnectionID == "" {
		j.ConnectionID = old.ConnectionID
	}
	if j.LocalPath == "" {
		j.LocalPath = old.LocalPath
	}
	if j.RemotePrefix == "" {
		j.RemotePrefix = old.RemotePrefix
	}
	if j.Exclude == nil {
		j.Exclude = old.Exclude
	}
	if j.LastRun == "" {
		j.LastRun = old.LastRun
	}
	if j.Created == "" {
		j.Created = old.Created
	}
	if j.Status == "" {
		j.Status = old.Status
	}
	if j.Detail == "" {
		j.Detail = old.Detail
	}
	if j.Enabled == false && old.Enabled && j.Name == old.Name && j.LocalPath == old.LocalPath && j.RemotePrefix == old.RemotePrefix {
		// false is a valid explicit pause; keep it.
	}
	if strings.TrimSpace(j.Name) == "" || len(j.Name) > 200 || j.ScheduleMinutes < 0 || j.ScheduleMinutes > 525600 || !validRetention(j.RetainRecent) || !validRetention(j.RetainDaily) || !validRetention(j.RetainMonthly) || j.RetainRecent+j.RetainDaily+j.RetainMonthly == 0 {
		return j, errors.New("备份任务配置无效")
	}
	connection, err := s.connection(j.ConnectionID)
	if err != nil {
		return j, err
	}
	if !canWriteStrict(connection) {
		return j, errors.New("备份连接尚未验证条件写入能力")
	}
	if err = validateBackupLocation(j.LocalPath, j.RemotePrefix); err != nil {
		return j, err
	}
	if err = validateExcludePatterns(j.Exclude); err != nil {
		return j, err
	}

	exclude, _ := json.Marshal(j.Exclude)
	tx, err := s.db.Begin()
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE backup_jobs SET name=?,connection=?,local_path=?,remote_prefix=?,exclude_json=?,schedule_minutes=?,retain_recent=?,retain_daily=?,retain_monthly=?,enabled=?,last_run=?,status=?,detail=? WHERE id=?`, j.Name, j.ConnectionID, j.LocalPath, j.RemotePrefix, string(exclude), j.ScheduleMinutes, j.RetainRecent, j.RetainDaily, j.RetainMonthly, backupBoolInt(j.Enabled), j.LastRun, j.Status, j.Detail, j.ID)
	if err != nil {
		return j, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return j, err
	}
	if n != 1 {
		return j, storage.ErrNotFound
	}
	if _, err = tx.Exec("DELETE FROM backup_plans WHERE job_id=?", j.ID); err != nil {
		return j, err
	}
	return j, tx.Commit()
}

func (s *Service) DeleteBackupJob(id string) error {
	unlock := s.lockBackupJob(id)
	defer unlock()
	if err := s.ensureBackupSchema(); err != nil {
		return err
	}

	_, err := s.db.Exec("UPDATE backup_jobs SET enabled=0,status='deleted',detail='任务已删除；历史快照仍保留' WHERE id=?", id)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("DELETE FROM backup_plans WHERE job_id=?", id)
	return err
}

func (s *Service) backupJob(id string) (BackupJob, error) {
	if err := s.ensureBackupSchema(); err != nil {
		return BackupJob{}, err
	}
	var j BackupJob
	var excludes string
	var enabled int
	err := s.db.QueryRow(`SELECT id,name,connection,local_path,remote_prefix,exclude_json,schedule_minutes,retain_recent,retain_daily,retain_monthly,enabled,last_run,status,detail,created FROM backup_jobs WHERE id=?`, id).Scan(&j.ID, &j.Name, &j.ConnectionID, &j.LocalPath, &j.RemotePrefix, &excludes, &j.ScheduleMinutes, &j.RetainRecent, &j.RetainDaily, &j.RetainMonthly, &enabled, &j.LastRun, &j.Status, &j.Detail, &j.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupJob{}, storage.ErrNotFound
	}
	if err != nil {
		return BackupJob{}, err
	}
	j.Enabled = enabled != 0
	if err = json.Unmarshal([]byte(excludes), &j.Exclude); err != nil {
		return BackupJob{}, err
	}
	return j, nil
}

func (s *Service) ListBackupJobs() ([]BackupJob, error) {
	if err := s.ensureBackupSchema(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT id FROM backup_jobs WHERE status!='deleted' ORDER BY created,id")
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
	out := make([]BackupJob, 0, len(ids))
	for _, id := range ids {
		j, e := s.backupJob(id)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, nil
}

func (s *Service) PreviewBackup(ctx context.Context, id string) (BackupPreview, error) {
	if err := s.ensureBackupSchema(); err != nil {
		return BackupPreview{}, err
	}
	j, err := s.backupJob(id)
	if err != nil {
		return BackupPreview{}, err
	}
	if j.Status == "deleted" {
		return BackupPreview{}, storage.ErrNotFound
	}
	connection, err := s.connection(j.ConnectionID)
	if err != nil {
		return BackupPreview{}, err
	}
	if !canWriteStrict(connection) {
		return BackupPreview{}, errors.New("备份连接尚未验证条件写入能力")
	}
	if err = validateExcludePatterns(j.Exclude); err != nil {
		return BackupPreview{}, err
	}
	root, err := os.OpenRoot(j.LocalPath)
	if err != nil {
		return BackupPreview{}, err
	}
	defer root.Close()
	files, _, protected, err := scanLocalFilteredProtected(ctx, root, s.Preferences().MaxFileBytes, j.Exclude)
	if err != nil {
		return BackupPreview{}, err
	}
	if len(protected) != 0 {
		return BackupPreview{}, errors.New("备份目录包含未排除的符号链接")
	}
	preview := BackupPreview{JobID: j.ID, Created: time.Now().UTC(), Files: []BackupFile{}}
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		f := files[key]
		preview.Files = append(preview.Files, BackupFile{Path: key, SHA256: f.Hash, Size: f.Size, State: "planned"})
		preview.Bytes += f.Size
	}
	preview.Excluded, err = countExcludedLocal(ctx, root, j.Exclude)
	if err != nil {
		return BackupPreview{}, err
	}
	if len(preview.Files) > maxBackupEntries {
		return BackupPreview{}, errors.New("备份清单超出单任务条目上限")
	}
	preview.Token = backupPreviewToken(j, preview.Files)
	raw, err := json.Marshal(preview)
	if err != nil {
		return BackupPreview{}, err
	}
	_, err = s.db.Exec(`INSERT INTO backup_plans(job_id,token,data,created) VALUES(?,?,?,?) ON CONFLICT(job_id) DO UPDATE SET token=excluded.token,data=excluded.data,created=excluded.created`, id, preview.Token, string(raw), preview.Created.Format(time.RFC3339Nano))
	return preview, err
}

const maxBackupEntries = 10000

func backupPreviewToken(j BackupJob, files []BackupFile) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", j.ID, j.ConnectionID, j.RemotePrefix, filepath.Clean(j.LocalPath))
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", f.Path, f.SHA256, f.Size)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func countExcludedLocal(ctx context.Context, root *os.Root, excludes []string) (int, error) {
	count := 0
	err := fs.WalkDir(root.FS(), ".", func(key string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if key == "." || !excludedPath(excludes, filepath.ToSlash(key)) {
			return nil
		}
		count++
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	return count, err
}

func (s *Service) RunBackup(ctx context.Context, id, token string) (BackupSnapshot, error) {
	ctx = withManagedBackup(ctx)
	ctx = withTransferScope(ctx, "backup", id)
	if err := s.ensureBackupSchema(); err != nil {
		return BackupSnapshot{}, err
	}
	unlock := s.lockBackupJob(id)
	defer unlock()
	j, err := s.backupJob(id)
	if err != nil {
		return BackupSnapshot{}, err
	}
	if j.Status == "deleted" {
		return BackupSnapshot{}, storage.ErrNotFound
	}
	connection, err := s.connection(j.ConnectionID)
	if err != nil {
		return BackupSnapshot{}, err
	}
	if !canWriteStrict(connection) {
		return BackupSnapshot{}, errors.New("备份连接尚未验证条件写入能力")
	}
	if token == "" {
		return BackupSnapshot{}, errors.New("请先预览备份内容")
	}
	var savedToken, planJSON string
	if err = s.db.QueryRow("SELECT token,data FROM backup_plans WHERE job_id=?", id).Scan(&savedToken, &planJSON); err != nil {
		return BackupSnapshot{}, err
	}
	if token != savedToken {
		return BackupSnapshot{}, storage.ErrConflict
	}
	var plan BackupPreview
	if err = json.Unmarshal([]byte(planJSON), &plan); err != nil || plan.JobID != id || plan.Token != token {
		return BackupSnapshot{}, errors.New("备份预览记录损坏，请重新预览")
	}
	root, err := os.OpenRoot(j.LocalPath)
	if err != nil {
		return BackupSnapshot{}, err
	}
	defer root.Close()
	current, _, protected, err := scanLocalFilteredProtected(ctx, root, s.Preferences().MaxFileBytes, j.Exclude)
	if err != nil {
		return BackupSnapshot{}, err
	}
	if len(protected) != 0 {
		return BackupSnapshot{}, errors.New("备份目录包含未排除的符号链接")
	}
	currentFiles := make([]BackupFile, 0, len(current))
	keys := make([]string, 0, len(current))
	for key := range current {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		currentFiles = append(currentFiles, BackupFile{Path: key, SHA256: current[key].Hash, Size: current[key].Size, State: "planned"})
	}
	if backupPreviewToken(j, currentFiles) != token {
		return BackupSnapshot{}, storage.ErrConflict
	}
	if len(plan.Files) != len(currentFiles) {
		return BackupSnapshot{}, storage.ErrConflict
	}
	for i := range currentFiles {
		if currentFiles[i].Path != plan.Files[i].Path || currentFiles[i].SHA256 != plan.Files[i].SHA256 || currentFiles[i].Size != plan.Files[i].Size {
			return BackupSnapshot{}, storage.ErrConflict
		}
	}

	snapshot := BackupSnapshot{ID: ID(), JobID: j.ID, RemoteConnection: j.ConnectionID, RemotePrefix: j.RemotePrefix, Started: time.Now().UTC().Format(time.RFC3339Nano), Status: "running", FilesTotal: len(plan.Files), BytesTotal: plan.Bytes, Files: append([]BackupFile(nil), plan.Files...)}
	manifest := backupManifest{Version: backupManifestVersion, Snapshot: snapshot.ID, JobID: j.ID, Started: snapshot.Started, Status: "running", Files: append([]BackupFile{}, snapshot.Files...), Bytes: snapshot.BytesTotal}
	if _, err = s.db.Exec(`INSERT INTO backup_snapshots(id,job_id,remote_connection,remote_prefix,started,status,files_total,files_done,files_failed,bytes_total,manifest_version) VALUES(?,?,?,?,?,'running',?,0,0,?,?)`, snapshot.ID, j.ID, j.ConnectionID, j.RemotePrefix, snapshot.Started, snapshot.FilesTotal, snapshot.BytesTotal, backupManifestVersion); err != nil {
		return BackupSnapshot{}, err
	}
	for _, f := range snapshot.Files {
		if _, err = s.db.Exec(`INSERT INTO backup_items(snapshot_id,path,sha256,size,state,modified) VALUES(?,?,?,?,'planned','')`, snapshot.ID, f.Path, f.SHA256, f.Size); err != nil {
			return snapshot, err
		}
	}
	base := backupSnapshotBase(j.RemotePrefix, snapshot.ID)
	backend := Backend{Service: s, ConnectionID: j.ConnectionID}
	if err = ensureRemoteDirectoryStrict(ctx, backend, base); err != nil {
		return s.finishBackupSnapshot(snapshot, fmt.Errorf("创建备份目录失败：%w", err))
	}
	manifestETag, err := saveBackupManifest(ctx, backend, path.Join(base, "manifest.json"), manifest, "")
	if err != nil {
		return s.finishBackupSnapshot(snapshot, fmt.Errorf("保存快照清单失败：%w", err))
	}

	for i := range snapshot.Files {
		f := &snapshot.Files[i]
		if err = ctx.Err(); err != nil {
			f.State, f.Error = "cancelled", "操作已取消"
			snapshot.FilesFailed++
			_ = s.updateBackupItem(snapshot.ID, *f)
			continue
		}
		actual, hashErr := s.hashLocal(root, f.Path)
		if hashErr != nil || actual.Hash != f.SHA256 || actual.Size != f.Size {
			f.State, f.Error = "failed", "源文件在预览后发生变化或不可读"
			snapshot.FilesFailed++
			_ = s.updateBackupItem(snapshot.ID, *f)
			continue
		}
		if err = ensureRemoteDirectoryStrict(ctx, backend, path.Join(base, "files", path.Dir(f.Path))); err != nil {
			f.State, f.Error = "failed", safeBackupFailure(err)
			snapshot.FilesFailed++
			_ = s.updateBackupItem(snapshot.ID, *f)
			continue
		}
		reader, openErr := root.Open(f.Path)
		if openErr != nil {
			f.State, f.Error = "failed", "读取源文件失败"
			snapshot.FilesFailed++
			_ = s.updateBackupItem(snapshot.ID, *f)
			continue
		}
		remoteKey := path.Join(base, "files", f.Path)
		entry, putErr := backend.putStrict(ctx, remoteKey, reader, f.Size, storage.Condition{IfNoneMatch: true}, f.SHA256)
		closeErr := reader.Close()
		if putErr == nil {
			putErr = closeErr
		}
		if putErr == nil {
			verifyCtx := ctx
			var verifyCancel context.CancelFunc
			if ctx.Err() != nil {
				verifyCtx, verifyCancel = context.WithTimeout(context.Background(), 90*time.Second)
			}
			got, actualEntry, verifyErr := hashRemote(verifyCtx, backend, remoteKey, entry.ETag)
			if verifyCancel != nil {
				verifyCancel()
			}
			if verifyErr != nil {
				putErr = verifyErr
			} else if got.Hash != f.SHA256 || got.Size != f.Size || actualEntry.ETag != entry.ETag {
				putErr = errors.New("远端副本未通过内容核验")
			}
		}
		if putErr != nil {
			f.State, f.Error = "failed", safeBackupFailure(putErr)
			snapshot.FilesFailed++
		} else {
			f.State, f.RemoteETag = "done", entry.ETag
			snapshot.FilesDone++
		}
		_ = s.updateBackupItem(snapshot.ID, *f)
	}
	for i := range snapshot.Files {
		if snapshot.Files[i].State != "planned" {
			continue
		}
		if ctx.Err() != nil {
			snapshot.Files[i].State = "cancelled"
			snapshot.Files[i].Error = "操作已取消"
		} else {
			snapshot.Files[i].State = "failed"
			snapshot.Files[i].Error = "任务提前停止，文件未上传"
		}
		snapshot.FilesFailed++
		_ = s.updateBackupItem(snapshot.ID, snapshot.Files[i])
	}
	if snapshot.FilesFailed > 0 {
		snapshot.Status = "partial"
	} else {
		snapshot.Status = "complete"
	}
	if snapshot.Error != "" {
		snapshot.Status = "partial"
	}
	snapshot.Completed = time.Now().UTC().Format(time.RFC3339Nano)
	manifest.Files = append(manifest.Files[:0], snapshot.Files...)
	manifest.Completed, manifest.Status, manifest.Error = snapshot.Completed, snapshot.Status, snapshot.Error
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 90*time.Second)
	finalCtx = withManagedBackup(finalCtx)
	defer finalCancel()
	if err = s.saveFinalBackupManifest(finalCtx, backend, base, manifest, manifestETag); err != nil {
		snapshot.Status = "partial"
		snapshot.Error = "最终快照清单保存失败"
		manifest.Status, manifest.Error = snapshot.Status, snapshot.Error
	}
	_, err = s.db.Exec(`UPDATE backup_snapshots SET completed=?,status=?,files_done=?,files_failed=?,error=? WHERE id=?`, snapshot.Completed, snapshot.Status, snapshot.FilesDone, snapshot.FilesFailed, snapshot.Error, snapshot.ID)
	if err == nil {
		_, err = s.db.Exec(`UPDATE backup_jobs SET last_run=?,status=?,detail=? WHERE id=?`, snapshot.Completed, snapshot.Status, snapshot.Error, j.ID)
	}
	if err != nil {
		return snapshot, err
	}
	if snapshot.Status == "complete" {
		if err = s.applyBackupRetention(ctx, j); err != nil {
			snapshot.Error = "快照已完成，但保留策略清理失败"
			_, _ = s.db.Exec("UPDATE backup_jobs SET detail=? WHERE id=?", snapshot.Error, j.ID)
		}
	}
	return snapshot, nil
}

func backupSnapshotBase(prefix, id string) string { return path.Join(prefix, ".tamiops-backup", id) }

func ensureRemoteDirectory(ctx context.Context, b Backend, key string) error {
	return ensureRemoteDirectoryWithPolicy(ctx, b, key, false)
}

func ensureRemoteDirectoryStrict(ctx context.Context, b Backend, key string) error {
	return ensureRemoteDirectoryWithPolicy(ctx, b, key, true)
}

func ensureRemoteDirectoryWithPolicy(ctx context.Context, b Backend, key string, strict bool) error {
	ctx = withManagedBackup(ctx)
	if key == "." || key == "" {
		return nil
	}
	parts := strings.Split(key, "/")
	current := ""
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = path.Join(current, part)
		entry, err := b.Stat(ctx, current)
		if err == nil {
			if !entry.IsDir {
				return errors.New("远端备份路径的父级不是目录")
			}
			continue
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if strict {
			err = b.mkdirStrict(ctx, current)
		} else {
			err = b.Mkdir(ctx, current)
		}
		if err != nil && !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	return nil
}

func saveBackupManifest(ctx context.Context, b Backend, key string, m backupManifest, etag string) (string, error) {
	ctx = withManagedBackup(ctx)
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	budget := int64(backupMaxManifestSize)
	prefs := b.Service.Preferences()
	if prefs.MaxFileBytes < budget {
		budget = prefs.MaxFileBytes
	}
	if prefs.StagingBytes < budget {
		budget = prefs.StagingBytes
	}
	if int64(len(data)) > budget {
		return "", errors.New("远端快照清单超过设置额度")
	}
	condition := storage.Condition{IfNoneMatch: true}
	if etag != "" {
		condition = storage.Condition{IfMatch: etag}
	}
	e, err := b.putStrict(ctx, key, strings.NewReader(string(data)), int64(len(data)), condition, "")
	if err != nil {
		return "", err
	}
	return e.ETag, nil
}

func (s *Service) saveFinalBackupManifest(ctx context.Context, b Backend, base string, m backupManifest, etag string) error {
	ctx = withManagedBackup(ctx)
	key := path.Join(base, "manifest.json")
	if !strongTag(etag) {
		return errors.New("初始快照清单缺少可靠版本，不能提交最终清单")
	}
	_, err := saveBackupManifest(ctx, b, key, m, etag)
	return err
}

func safeBackupFailure(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "资源已变化或目标已存在"
	case errors.Is(err, storage.ErrLocked):
		return "资源当前被 WebDAV 锁定"
	case errors.Is(err, context.Canceled):
		return "操作已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "操作超时"
	default:
		return "传输或核验失败"
	}
}

func (s *Service) updateBackupItem(snapshotID string, f BackupFile) error {
	modified := ""
	if !f.Modified.IsZero() {
		modified = f.Modified.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(`UPDATE backup_items SET remote_etag=?,state=?,error=?,modified=? WHERE snapshot_id=? AND path=?`, f.RemoteETag, f.State, f.Error, modified, snapshotID, f.Path)
	return err
}

func (s *Service) finishBackupSnapshot(snapshot BackupSnapshot, cause error) (BackupSnapshot, error) {
	snapshot.Status = "partial"
	snapshot.Completed = time.Now().UTC().Format(time.RFC3339Nano)
	snapshot.Error = safeBackupFailure(cause)
	for i := range snapshot.Files {
		if snapshot.Files[i].State == "planned" {
			snapshot.Files[i].State = "failed"
			snapshot.Files[i].Error = snapshot.Error
			_ = s.updateBackupItem(snapshot.ID, snapshot.Files[i])
			snapshot.FilesFailed++
		}
	}
	_, _ = s.db.Exec(`UPDATE backup_snapshots SET completed=?,status='partial',files_done=?,files_failed=?,error=? WHERE id=?`, snapshot.Completed, snapshot.FilesDone, snapshot.FilesFailed, snapshot.Error, snapshot.ID)
	_, _ = s.db.Exec(`UPDATE backup_jobs SET last_run=?,status='partial',detail=? WHERE id=?`, snapshot.Completed, snapshot.Error, snapshot.JobID)
	return snapshot, cause
}

func (s *Service) ListBackupSnapshots(jobID string) ([]BackupSnapshot, error) {
	if err := s.ensureBackupSchema(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,job_id,remote_connection,remote_prefix,started,completed,status,files_total,files_done,files_failed,bytes_total,error FROM backup_snapshots WHERE job_id=? ORDER BY started DESC LIMIT ?`, jobID, backupMaxSnapshots)
	if err != nil {
		return nil, err
	}
	out := []BackupSnapshot{}
	for rows.Next() {
		var v BackupSnapshot
		if err = rows.Scan(&v.ID, &v.JobID, &v.RemoteConnection, &v.RemotePrefix, &v.Started, &v.Completed, &v.Status, &v.FilesTotal, &v.FilesDone, &v.FilesFailed, &v.BytesTotal, &v.Error); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) backupSnapshot(id string) (BackupSnapshot, BackupJob, error) {
	var v BackupSnapshot
	err := s.db.QueryRow(`SELECT id,job_id,remote_connection,remote_prefix,started,completed,status,files_total,files_done,files_failed,bytes_total,error FROM backup_snapshots WHERE id=?`, id).Scan(&v.ID, &v.JobID, &v.RemoteConnection, &v.RemotePrefix, &v.Started, &v.Completed, &v.Status, &v.FilesTotal, &v.FilesDone, &v.FilesFailed, &v.BytesTotal, &v.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupSnapshot{}, BackupJob{}, storage.ErrNotFound
	}
	if err != nil {
		return BackupSnapshot{}, BackupJob{}, err
	}
	j, err := s.backupJob(v.JobID)
	if err != nil {
		return BackupSnapshot{}, BackupJob{}, err
	}
	if v.RemoteConnection == "" {
		v.RemoteConnection = j.ConnectionID
	}
	if v.RemotePrefix == "" {
		v.RemotePrefix = j.RemotePrefix
	}
	return v, j, err
}

func (s *Service) loadBackupFiles(snapshotID string) ([]BackupFile, error) {
	rows, err := s.db.Query(`SELECT path,sha256,size,remote_etag,state,error,modified FROM backup_items WHERE snapshot_id=? ORDER BY path`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []BackupFile{}
	for rows.Next() {
		var f BackupFile
		var modified string
		if err = rows.Scan(&f.Path, &f.SHA256, &f.Size, &f.RemoteETag, &f.State, &f.Error, &modified); err != nil {
			return nil, err
		}
		if modified != "" {
			f.Modified, _ = time.Parse(time.RFC3339Nano, modified)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Service) RestoreBackupSnapshot(ctx context.Context, snapshotID, destDir string) error {
	if !validRecoveryID(snapshotID) {
		return errors.New("无效快照编号")
	}
	if err := s.ensureBackupSchema(); err != nil {
		return err
	}
	snapshot, job, err := s.backupSnapshot(snapshotID)
	if err != nil {
		return err
	}
	if snapshot.Status != "complete" {
		return errors.New("只能恢复完整快照；部分快照的失败项需要先检查")
	}
	connectionID, remotePrefix := snapshot.RemoteConnection, snapshot.RemotePrefix
	if connectionID == "" {
		connectionID = job.ConnectionID
	}
	if remotePrefix == "" {
		remotePrefix = job.RemotePrefix
	}
	return s.restoreBackupRemote(ctx, connectionID, remotePrefix, snapshotID, destDir)
}

// ListRemoteBackupSnapshots allows recovery of snapshot history after the local database
// has been lost. The durable manifests live below the application-owned backup namespace.
func (s *Service) ListRemoteBackupSnapshots(ctx context.Context, connectionID, remotePrefix string) ([]BackupSnapshot, error) {
	if remotePrefix == "" || validSyncPath(remotePrefix) != nil {
		return nil, errors.New("远端备份前缀无效")
	}
	st, err := s.store(connectionID)
	if err != nil {
		return nil, err
	}
	root := path.Join(remotePrefix, ".tamiops-backup")
	entries, err := st.List(ctx, root)
	if err != nil {
		return nil, err
	}
	if len(entries) > backupMaxSnapshots {
		return nil, errors.New("远端备份快照目录超过扫描上限")
	}
	out := []BackupSnapshot{}
	for _, entry := range entries {
		if !entry.IsDir || path.Dir(entry.Path) != root {
			continue
		}
		if !validRecoveryID(path.Base(entry.Path)) {
			continue
		}
		m, _, readErr := s.readBackupManifest(ctx, connectionID, path.Join(entry.Path, "manifest.json"))
		if readErr != nil || m.Version != backupManifestVersion || m.Snapshot != path.Base(entry.Path) {
			continue
		}
		out = append(out, BackupSnapshot{ID: m.Snapshot, JobID: m.JobID, RemoteConnection: connectionID, RemotePrefix: remotePrefix, Started: m.Started, Completed: m.Completed, Status: m.Status, FilesTotal: len(m.Files), FilesDone: backupCountState(m.Files, "done"), FilesFailed: backupFailedCount(m.Files), BytesTotal: m.Bytes, Error: m.Error})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	if len(out) > backupMaxSnapshots {
		out = out[:backupMaxSnapshots]
	}
	return out, nil
}

func backupCountState(files []BackupFile, state string) int {
	n := 0
	for _, f := range files {
		if f.State == state {
			n++
		}
	}
	return n
}
func backupFailedCount(files []BackupFile) int {
	n := 0
	for _, f := range files {
		if f.State != "done" {
			n++
		}
	}
	return n
}

func (s *Service) RestoreRemoteBackupSnapshot(ctx context.Context, connectionID, remotePrefix, snapshotID, destDir string) error {
	if !validRecoveryID(snapshotID) || remotePrefix == "" || validSyncPath(remotePrefix) != nil {
		return errors.New("快照标识或远端前缀无效")
	}
	return s.restoreBackupRemote(ctx, connectionID, remotePrefix, snapshotID, destDir)
}

func (s *Service) restoreBackupRemote(ctx context.Context, connectionID, remotePrefix, snapshotID, destDir string) error {
	if !validRecoveryID(snapshotID) || remotePrefix == "" || validSyncPath(remotePrefix) != nil {
		return errors.New("快照标识或远端前缀无效")
	}
	if destDir == "" {
		return errors.New("请选择新的本地恢复目录")
	}
	st, err := s.store(connectionID)
	if err != nil {
		return err
	}
	b := Backend{Service: s, ConnectionID: connectionID}
	base := backupSnapshotBase(remotePrefix, snapshotID)
	m, manifestEntry, err := s.readBackupManifest(ctx, connectionID, path.Join(base, "manifest.json"))
	if err != nil {
		return err
	}
	if m.Version != backupManifestVersion || m.Snapshot != snapshotID || m.Status != "complete" {
		return errors.New("远端快照清单无效或快照不完整")
	}
	ctx = withTransferScope(ctx, "backup", m.JobID)
	if manifestEntry.Size < 0 || manifestEntry.Size > backupMaxManifestSize {
		return errors.New("快照清单大小无效")
	}
	if len(m.Files) > maxBackupEntries {
		return errors.New("快照文件数量超过恢复上限")
	}
	if m.Files == nil {
		m.Files = []BackupFile{}
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range m.Files {
		if f.State != "done" || validSyncPath(f.Path) != nil || seen[portablePathIdentity(f.Path)] {
			return errors.New("快照包含失败、无效或冲突路径，已停止恢复")
		}
		seen[portablePathIdentity(f.Path)] = true
		if f.Size < 0 || f.Size > s.Preferences().MaxFileBytes {
			return errors.New("快照文件超过当前单文件恢复额度")
		}
		total += f.Size
	}
	if total > freeBytes(s.dir)-(256<<20) {
		return errors.New("可用磁盘空间不足，无法恢复此快照")
	}
	if err = os.Mkdir(destDir, 0700); err != nil {
		return errors.New("恢复必须写入一个尚不存在的新目录")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destDir)
		}
	}()
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range m.Files {
		if err = ctx.Err(); err != nil {
			return err
		}
		remoteKey := path.Join(base, "files", f.Path)
		got, entry, hashErr := hashRemote(ctx, b, remoteKey, f.RemoteETag)
		if hashErr != nil || got.Hash != f.SHA256 || got.Size != f.Size || entry.ETag != f.RemoteETag {
			return fmt.Errorf("快照文件核验失败：%s", filepath.Base(f.Path))
		}
		parent := path.Dir(f.Path)
		if parent != "." {
			if err = root.MkdirAll(filepath.FromSlash(parent), 0700); err != nil {
				return err
			}
		}
		remoteReader, opened, openErr := b.Open(ctx, remoteKey, f.RemoteETag)
		if openErr != nil {
			return openErr
		}
		if opened.ETag != f.RemoteETag || opened.Size != f.Size || opened.IsDir {
			remoteReader.Close()
			return storage.ErrConflict
		}
		tmp := ".tami-restore-" + ID()
		dir := path.Dir(f.Path)
		if dir != "." {
			tmp = path.Join(dir, tmp)
		}
		out, openErr := root.OpenFile(filepath.FromSlash(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if openErr != nil {
			remoteReader.Close()
			return openErr
		}
		h := sha256.New()
		r := io.TeeReader(remoteReader, h)
		n, copyErr := io.Copy(out, io.LimitReader(r, s.Preferences().MaxFileBytes+1))
		if copyErr == nil && (n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256) {
			copyErr = errors.New("恢复内容校验失败")
		}
		if copyErr == nil {
			copyErr = out.Sync()
		}
		outErr := out.Close()
		remoteErr := remoteReader.Close()
		if copyErr == nil {
			copyErr = outErr
		}
		if copyErr == nil {
			copyErr = remoteErr
		}
		if copyErr == nil {
			copyErr = root.Link(filepath.FromSlash(tmp), filepath.FromSlash(f.Path))
		}
		_ = root.Remove(filepath.FromSlash(tmp))
		if copyErr != nil {
			return fmt.Errorf("恢复文件失败：%s：%w", filepath.Base(f.Path), copyErr)
		}
	}
	manifestAfter, statErr := st.Stat(ctx, path.Join(base, "manifest.json"))
	if statErr != nil || manifestAfter.ETag != manifestEntry.ETag {
		return errors.New("恢复期间快照清单发生变化")
	}
	complete = true
	return nil
}

func (s *Service) readBackupManifest(ctx context.Context, connectionID, key string) (backupManifest, storage.Entry, error) {
	b := Backend{Service: s, ConnectionID: connectionID}
	r, e, err := b.Open(ctx, key, "")
	if err != nil {
		return backupManifest{}, e, err
	}
	defer r.Close()
	if e.IsDir || e.Size < 0 || e.Size > backupMaxManifestSize {
		return backupManifest{}, e, errors.New("远端快照清单大小或类型无效")
	}
	data, err := io.ReadAll(io.LimitReader(r, backupMaxManifestSize+1))
	if err != nil {
		return backupManifest{}, e, err
	}
	if int64(len(data)) != e.Size || len(data) > backupMaxManifestSize {
		return backupManifest{}, e, errors.New("远端快照清单读取不完整")
	}
	var m backupManifest
	if err = json.Unmarshal(data, &m); err != nil {
		return backupManifest{}, e, errors.New("远端快照清单格式无效")
	}
	return m, e, nil
}

func (s *Service) applyBackupRetention(ctx context.Context, j BackupJob) error {
	ctx = withManagedBackup(ctx)
	snapshots, err := s.ListBackupSnapshots(j.ID)
	if err != nil {
		return err
	}
	complete := make([]BackupSnapshot, 0, len(snapshots))
	for _, v := range snapshots {
		if v.Status == "complete" {
			complete = append(complete, v)
		}
	}
	if len(complete) <= j.RetainRecent && j.RetainDaily == 0 && j.RetainMonthly == 0 {
		return nil
	}
	keep := map[string]bool{}
	for i, v := range complete {
		if i < j.RetainRecent {
			keep[v.ID] = true
		}
	}
	keepCalendar := func(n int, layout string) {
		if n <= 0 {
			return
		}
		seen := map[string]bool{}
		kept := 0
		for _, v := range complete {
			t, e := time.Parse(time.RFC3339Nano, v.Started)
			if e != nil {
				continue
			}
			bucket := t.UTC().Format(layout)
			if seen[bucket] {
				continue
			}
			seen[bucket] = true
			if kept < n {
				keep[v.ID] = true
				kept++
			}
		}
	}
	keepCalendar(j.RetainDaily, "2006-01-02")
	keepCalendar(j.RetainMonthly, "2006-01")
	for _, v := range complete {
		if keep[v.ID] {
			continue
		}
		if !validRecoveryID(v.ID) {
			return errors.New("保留策略遇到无效快照编号，已停止清理")
		}
		connectionID, remotePrefix := v.RemoteConnection, v.RemotePrefix
		if connectionID == "" {
			connectionID = j.ConnectionID
		}
		if remotePrefix == "" {
			remotePrefix = j.RemotePrefix
		}
		backend := Backend{Service: s, ConnectionID: connectionID}
		root := backupSnapshotBase(remotePrefix, v.ID)
		preview, e := backend.PreviewDeleteTree(ctx, root)
		if errors.Is(e, storage.ErrNotFound) {
			preview = TreePreview{Path: root}
		} else if e != nil {
			return e
		} else if e = backend.DeleteTreeWithPreview(ctx, root, preview.Token); e != nil {
			return e
		}
		if _, e = s.db.Exec("DELETE FROM backup_items WHERE snapshot_id=?", v.ID); e != nil {
			return e
		}
		if _, e = s.db.Exec("DELETE FROM backup_snapshots WHERE id=? AND status='complete'", v.ID); e != nil {
			return e
		}
	}
	return nil
}

func (s *Service) RunDueBackups(ctx context.Context) ([]BackupSnapshot, error) {
	jobs, err := s.ListBackupJobs()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []BackupSnapshot{}
	var failures []error
	for _, j := range jobs {
		if !j.Enabled || j.ScheduleMinutes <= 0 {
			continue
		}
		last, parseErr := time.Parse(time.RFC3339Nano, j.LastRun)
		if parseErr == nil && now.Sub(last) < time.Duration(j.ScheduleMinutes)*time.Minute {
			continue
		}
		if err = ctx.Err(); err != nil {
			return out, err
		}
		allowed, reason := s.AutomationAllowed(ctx, now)
		if !allowed {
			_, _ = s.db.Exec("UPDATE backup_jobs SET status='waiting',detail=? WHERE id=?", reason, j.ID)
			continue
		}
		connection, connectionErr := s.connection(j.ConnectionID)
		if connectionErr != nil || !canWriteStrict(connection) {
			detail := "备份连接不可用或未验证条件写入"
			s.recordBackupAttemptFailure(j, detail)
			continue
		}
		preview, e := s.PreviewBackup(ctx, j.ID)
		if e == nil {
			var snapshot BackupSnapshot
			snapshot, e = s.RunBackup(ctx, j.ID, preview.Token)
			if snapshot.ID != "" {
				out = append(out, snapshot)
			}
		}
		if e != nil {
			s.recordBackupAttemptFailure(j, safeBackupFailure(e))
			failures = append(failures, fmt.Errorf("%s: %w", j.Name, e))
		}
	}
	return out, errors.Join(failures...)
}

func (s *Service) recordBackupAttemptFailure(j BackupJob, detail string) {
	if detail == "" {
		detail = "定时备份未能完成"
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.Exec("UPDATE backup_jobs SET last_run=?,status='error',detail=? WHERE id=?", at, detail, j.ID)
	s.activity("backup", "error", "定时备份失败："+detail, j.ID)
}
