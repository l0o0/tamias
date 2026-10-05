package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"tamiops/internal/storage"
	"time"
)

const backupCleanupMaxObjects = maxBackupEntries + 1

type BackupCleanupObject struct {
	Path string `json:"path"`
	ETag string `json:"etag"`
	Size int64  `json:"size"`
	Kind string `json:"kind"`
}

type BackupCleanupPreview struct {
	JobID      string                `json:"jobId"`
	SnapshotID string                `json:"snapshotId"`
	Token      string                `json:"token"`
	Created    string                `json:"created"`
	Status     string                `json:"status"`
	Objects    []BackupCleanupObject `json:"objects"`
	Bytes      int64                 `json:"bytes"`
}

type BackupCleanupResult struct {
	JobID      string   `json:"jobId"`
	SnapshotID string   `json:"snapshotId"`
	Status     string   `json:"status"`
	Deleted    []string `json:"deleted"`
	Remaining  []string `json:"remaining"`
	Errors     []string `json:"errors"`
}

type backupCleanupReceipt struct {
	SnapshotID string `json:"snapshotId"`
	ObjectPath string `json:"objectPath"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
	Deleted    string `json:"deleted"`
}

type backupCleanupPlanData struct {
	Snapshot     BackupSnapshot         `json:"snapshot"`
	Files        []BackupFile           `json:"files"`
	Manifest     *backupManifest        `json:"manifest,omitempty"`
	ManifestETag string                 `json:"manifestEtag,omitempty"`
	Receipts     []backupCleanupReceipt `json:"receipts"`
	Objects      []BackupCleanupObject  `json:"objects"`
	Base         string                 `json:"base"`
	ConnectionID string                 `json:"connectionId"`
	RemotePrefix string                 `json:"remotePrefix"`
}

func (s *Service) backupCleanupCommand(r *http.Request) (any, error, bool) {
	switch r.URL.Path {
	case "/api/backups/cleanup-preview":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		v, err := s.PreviewBackupCleanup(ctx, in.ID)
		return v, err, true
	case "/api/backups/cleanup":
		var in struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		v, err := s.RunBackupCleanup(ctx, in.ID, in.Token)
		return v, err, true
	default:
		return nil, nil, false
	}
}

func (s *Service) ensureBackupCleanupSchema() error {
	if err := s.ensureBackupSchema(); err != nil {
		return err
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS backup_cleanup_plans (
	 snapshot_id TEXT PRIMARY KEY, token TEXT NOT NULL, data TEXT NOT NULL, created TEXT NOT NULL);
	 CREATE TABLE IF NOT EXISTS backup_cleanup_receipts (
	 snapshot_id TEXT NOT NULL, object_path TEXT NOT NULL, etag TEXT NOT NULL, size INTEGER NOT NULL, deleted TEXT NOT NULL,
	 PRIMARY KEY(snapshot_id,object_path));`)
	return err
}

// PreviewBackupCleanup creates a content and ETag bound plan for one failed or
// incomplete snapshot. Complete snapshots and snapshots currently running are
// never eligible.
func (s *Service) PreviewBackupCleanup(ctx context.Context, snapshotID string) (BackupCleanupPreview, error) {
	if !validRecoveryID(snapshotID) {
		return BackupCleanupPreview{}, errors.New("无效快照编号")
	}
	if err := s.ensureBackupCleanupSchema(); err != nil {
		return BackupCleanupPreview{}, err
	}
	first, job, err := s.backupSnapshot(snapshotID)
	if err != nil {
		return BackupCleanupPreview{}, err
	}
	if first.Status == "running" {
		return BackupCleanupPreview{}, errors.New("备份仍在执行，暂时不能清理")
	}
	unlock := s.lockBackupJob(job.ID)
	defer unlock()
	plan, err := s.buildBackupCleanupPlan(ctx, snapshotID)
	if err != nil {
		return BackupCleanupPreview{}, err
	}
	preview := BackupCleanupPreview{JobID: plan.data.Snapshot.JobID, SnapshotID: snapshotID, Created: time.Now().UTC().Format(time.RFC3339Nano), Status: "ready", Objects: append([]BackupCleanupObject{}, plan.data.Objects...), Bytes: backupCleanupObjectBytes(plan.data.Objects)}
	if len(preview.Objects) == 0 {
		preview.Status = "nothing_to_clean"
	}
	preview.Token = backupCleanupToken(plan.data)
	if len(preview.Objects) == 0 && backupCleanupFullyRecorded(plan.data) {
		if err = s.markBackupSnapshotCleaned(snapshotID); err != nil {
			return BackupCleanupPreview{}, err
		}
		preview.Status = "cleaned"
		preview.Token = ""
		_, err = s.db.Exec("DELETE FROM backup_cleanup_plans WHERE snapshot_id=?", snapshotID)
		return preview, err
	}
	raw, err := json.Marshal(plan.data)
	if err != nil {
		return BackupCleanupPreview{}, err
	}
	_, err = s.db.Exec(`INSERT INTO backup_cleanup_plans(snapshot_id,token,data,created) VALUES(?,?,?,?)
	 ON CONFLICT(snapshot_id) DO UPDATE SET token=excluded.token,data=excluded.data,created=excluded.created`, snapshotID, preview.Token, string(raw), preview.Created)
	return preview, err
}

type backupCleanupPlan struct {
	data backupCleanupPlanData
	job  BackupJob
	keys map[string]string // object path -> absolute remote key
}

func (s *Service) buildBackupCleanupPlan(ctx context.Context, snapshotID string) (backupCleanupPlan, error) {
	snapshot, job, err := s.backupSnapshot(snapshotID)
	if err != nil {
		return backupCleanupPlan{}, err
	}
	var active int
	if err = s.db.QueryRow(`SELECT count(*) FROM backup_snapshots WHERE job_id=? AND status='running'`, job.ID).Scan(&active); err != nil {
		return backupCleanupPlan{}, err
	}
	if active > 0 || job.Status == "running" {
		return backupCleanupPlan{}, errors.New("此备份任务仍在执行，暂时不能清理")
	}
	switch snapshot.Status {
	case "complete":
		return backupCleanupPlan{}, errors.New("完整快照会继续保留，不能清理")
	case "running":
		return backupCleanupPlan{}, errors.New("备份仍在执行，暂时不能清理")
	case "partial", "failed", "error", "paused", "cancelled":
	default:
		return backupCleanupPlan{}, errors.New("快照状态不明确，已停止清理")
	}
	if snapshot.RemoteConnection == "" || snapshot.RemotePrefix == "" || validSyncPath(snapshot.RemotePrefix) != nil {
		return backupCleanupPlan{}, errors.New("快照缺少可信的远端位置记录，已停止清理")
	}
	conn, err := s.connection(snapshot.RemoteConnection)
	if err != nil {
		return backupCleanupPlan{}, err
	}
	if !canDelete(conn) {
		return backupCleanupPlan{}, errors.New("远端未验证条件删除能力，已保留快照对象")
	}
	files, err := s.loadBackupFiles(snapshotID)
	if err != nil {
		return backupCleanupPlan{}, err
	}
	if len(files) > maxBackupEntries || len(files) != snapshot.FilesTotal {
		return backupCleanupPlan{}, errors.New("本地快照文件记录不完整，已停止清理")
	}
	var doneCount, failedCount, plannedCount int
	var fileBytes int64
	seen := map[string]bool{}
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return backupCleanupPlan{}, err
		}
		if validSyncPath(f.Path) != nil || seen[portablePathIdentity(f.Path)] || f.Size < 0 || f.SHA256 == "" {
			return backupCleanupPlan{}, errors.New("本地快照文件路径或内容记录无效，已停止清理")
		}
		seen[portablePathIdentity(f.Path)] = true
		fileBytes += f.Size
		switch f.State {
		case "done":
			doneCount++
			if !strongTag(f.RemoteETag) {
				return backupCleanupPlan{}, errors.New("已上传文件缺少可靠 ETag，已停止清理")
			}
		case "failed", "cancelled":
			failedCount++
		case "planned":
			plannedCount++
		default:
			return backupCleanupPlan{}, errors.New("快照含未决文件状态，已停止清理")
		}
	}
	if doneCount != snapshot.FilesDone || failedCount != snapshot.FilesFailed || doneCount+failedCount+plannedCount != snapshot.FilesTotal || fileBytes != snapshot.BytesTotal {
		return backupCleanupPlan{}, errors.New("本地快照汇总与文件记录不一致，已停止清理")
	}
	base := backupSnapshotBase(snapshot.RemotePrefix, snapshot.ID)
	manifestKey := path.Join(base, "manifest.json")
	if err = s.checkBackupCleanupPendingOperations(snapshot.RemoteConnection, base); err != nil {
		return backupCleanupPlan{}, err
	}
	backend := Backend{Service: s, ConnectionID: snapshot.RemoteConnection}
	receipts, err := s.backupCleanupReceipts(snapshotID)
	if err != nil {
		return backupCleanupPlan{}, err
	}
	receiptByPath := make(map[string]backupCleanupReceipt, len(receipts))
	for _, receipt := range receipts {
		if receipt.ObjectPath == "" || !strongTag(receipt.ETag) || receipt.Size < 0 {
			return backupCleanupPlan{}, errors.New("本地清理回执无效，已停止清理")
		}
		receiptByPath[receipt.ObjectPath] = receipt
	}
	manifest, manifestEntry, manifestErr := s.readBackupManifest(ctx, snapshot.RemoteConnection, manifestKey)
	if manifestErr != nil && !errors.Is(manifestErr, storage.ErrNotFound) {
		return backupCleanupPlan{}, errors.New("远端快照清单无法读取，已停止清理")
	}
	var manifestPtr *backupManifest
	manifestETag := ""
	if manifestErr == nil {
		if !strongTag(manifestEntry.ETag) {
			return backupCleanupPlan{}, errors.New("远端快照清单缺少可靠 ETag，已停止清理")
		}
		if err = validateCleanupManifest(manifest, snapshot, files); err != nil {
			return backupCleanupPlan{}, err
		}
		manifestPtr = &manifest
		manifestETag = manifestEntry.ETag
	}
	items, dirs, err := listBackupCleanupTree(ctx, s, snapshot.RemoteConnection, base)
	if errors.Is(err, storage.ErrNotFound) {
		items, dirs, err = map[string]storage.Entry{}, map[string]bool{}, nil
	}
	if err != nil {
		return backupCleanupPlan{}, errors.New("远端快照对象列表无法核对，已停止清理")
	}
	if len(items) > backupCleanupMaxObjects {
		return backupCleanupPlan{}, errors.New("远端快照对象数量超过清理上限")
	}
	for objectPath := range receiptByPath {
		if _, exists := items[objectPath]; exists {
			return backupCleanupPlan{}, errors.New("已清理对象重新出现，远端内容已变化")
		}
	}
	expectedDirs := cleanupExpectedDirectories(base, files)
	for dir := range dirs {
		if !expectedDirs[dir] {
			return backupCleanupPlan{}, errors.New("远端快照包含本地记录以外的目录，已停止清理")
		}
	}
	keys := make(map[string]string)
	objects := make([]BackupCleanupObject, 0, doneCount+plannedCount+1)
	expected := map[string]BackupCleanupObject{}
	if manifestErr == nil {
		expected["manifest.json"] = BackupCleanupObject{Path: "manifest.json", ETag: manifestETag, Size: manifestEntry.Size, Kind: "manifest"}
	}
	fileRecords := map[string]BackupFile{}
	for _, f := range files {
		rel := path.Join("files", f.Path)
		objectKey := path.Join(base, rel)
		if f.State == "done" {
			expected[rel] = BackupCleanupObject{Path: rel, ETag: f.RemoteETag, Size: f.Size, Kind: "file"}
			fileRecords[rel] = f
		} else {
			if entry, exists := items[objectKey]; exists {
				if entry.IsDir || entry.Size != f.Size || !strongTag(entry.ETag) {
					return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 的类型、大小或 ETag 无效，已停止清理", rel)
				}
				expected[rel] = BackupCleanupObject{Path: rel, ETag: entry.ETag, Size: f.Size, Kind: "file"}
				fileRecords[rel] = f
			} else if _, cleaned := receiptByPath[objectKey]; !cleaned {
				if journal, found, journalErr := s.committedBackupDelete(snapshot.RemoteConnection, objectKey, ""); journalErr != nil {
					return backupCleanupPlan{}, journalErr
				} else if found {
					if journal.Source.Size != f.Size {
						return backupCleanupPlan{}, errors.New("已提交删除记录与本地快照文件大小不一致")
					}
					receipt, receiptErr := s.saveRecoveredCleanupReceipt(snapshotID, objectKey, journal.Source.ETag, f.Size)
					if receiptErr != nil {
						return backupCleanupPlan{}, receiptErr
					}
					receipts = append(receipts, receipt)
					receiptByPath[objectKey] = receipt
				}
			}
		}
	}
	allowedReceipts := map[string]bool{manifestKey: true}
	for _, f := range files {
		allowedReceipts[path.Join(base, "files", f.Path)] = true
	}
	for objectPath := range receiptByPath {
		if !allowedReceipts[objectPath] {
			return backupCleanupPlan{}, errors.New("本地清理回执指向快照记录以外的对象，已停止清理")
		}
	}
	if manifestErr != nil && doneCount > 0 {
		if _, cleaned := receiptByPath[manifestKey]; !cleaned {
			if journal, found, journalErr := s.committedBackupDelete(snapshot.RemoteConnection, manifestKey, ""); journalErr != nil {
				return backupCleanupPlan{}, journalErr
			} else if found {
				receipt, receiptErr := s.saveRecoveredCleanupReceipt(snapshotID, manifestKey, journal.Source.ETag, journal.Source.Size)
				if receiptErr != nil {
					return backupCleanupPlan{}, receiptErr
				}
				receipts = append(receipts, receipt)
				receiptByPath[manifestKey] = receipt
			} else {
				return backupCleanupPlan{}, errors.New("快照含已上传文件但远端清单缺失且没有清理回执，提交状态不明确，已停止清理")
			}
		}
	}
	for rel, file := range fileRecords {
		objectKey := path.Join(base, rel)
		if receipt, cleaned := receiptByPath[objectKey]; cleaned {
			if receipt.Size != file.Size || (file.State == "done" && receipt.ETag != file.RemoteETag) {
				return backupCleanupPlan{}, errors.New("清理回执与本地快照文件不一致，已停止清理")
			}
			continue
		}
		if _, exists := items[objectKey]; exists {
			continue
		}
		if file.State != "done" {
			continue
		}
		journal, found, journalErr := s.committedBackupDelete(snapshot.RemoteConnection, objectKey, file.RemoteETag)
		if journalErr != nil {
			return backupCleanupPlan{}, journalErr
		}
		if !found {
			return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 已缺失且无清理回执或已提交删除记录", rel)
		}
		receipt, receiptErr := s.saveRecoveredCleanupReceipt(snapshotID, objectKey, journal.Source.ETag, file.Size)
		if receiptErr != nil {
			return backupCleanupPlan{}, receiptErr
		}
		receipts = append(receipts, receipt)
		receiptByPath[objectKey] = receipt
	}
	for rel, expectedObject := range expected {
		objectKey := path.Join(base, rel)
		actual, exists := items[objectKey]
		if receipt, cleaned := receiptByPath[objectKey]; cleaned {
			if exists || receipt.ETag != expectedObject.ETag && expectedObject.Kind != "manifest" || (expectedObject.Kind != "manifest" && receipt.Size != expectedObject.Size) {
				return backupCleanupPlan{}, errors.New("清理回执与本地快照记录不一致，已停止清理")
			}
			continue
		}
		if !exists {
			return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 已缺失且没有清理回执，可能被外部修改或提交结果不明", rel)
		}
		if actual.IsDir || actual.Size != expectedObject.Size || !strongTag(actual.ETag) {
			return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 的类型、大小或 ETag 已变化，已停止清理", rel)
		}
		if expectedObject.Kind == "file" {
			f := fileRecords[rel]
			verified, verifiedEntry, verifyErr := hashRemote(ctx, backend, objectKey, actual.ETag)
			if verifyErr != nil || verified.Hash != f.SHA256 || verified.Size != f.Size || verifiedEntry.ETag != actual.ETag {
				return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 内容与本地快照记录不一致，已停止清理", rel)
			}
		} else if actual.ETag != manifestEntry.ETag || actual.Size != manifestEntry.Size {
			return backupCleanupPlan{}, errors.New("远端清单在核对期间发生变化，已停止清理")
		}
		if expectedObject.Kind == "file" && expectedObject.ETag != "" && actual.ETag != expectedObject.ETag {
			return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 的 ETag 已变化，已停止清理", rel)
		}
		object := expectedObject
		if object.Kind == "file" && object.ETag == "" {
			object.ETag = actual.ETag
		}
		objects = append(objects, object)
		keys[rel] = objectKey
	}
	for key := range items {
		if key == manifestKey {
			continue
		}
		rel := strings.TrimPrefix(key, base+"/")
		if _, ok := expected[rel]; !ok {
			return backupCleanupPlan{}, fmt.Errorf("远端对象 %s 不在本地快照记录中，已停止清理", rel)
		}
	}
	if manifestErr != nil && len(objects) > 0 {
		if _, cleaned := receiptByPath[manifestKey]; !cleaned {
			return backupCleanupPlan{}, errors.New("远端文件存在但快照清单缺失，已停止清理")
		}
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Kind != objects[j].Kind {
			return objects[i].Kind == "file" // Keep the manifest until all data objects are removed.
		}
		return objects[i].Path < objects[j].Path
	})
	data := backupCleanupPlanData{Snapshot: snapshot, Files: files, Manifest: manifestPtr, ManifestETag: manifestETag, Receipts: receipts, Objects: objects, Base: base, ConnectionID: snapshot.RemoteConnection, RemotePrefix: snapshot.RemotePrefix}
	return backupCleanupPlan{data: data, job: job, keys: keys}, nil
}

func validateCleanupManifest(m backupManifest, snapshot BackupSnapshot, files []BackupFile) error {
	if m.Version != backupManifestVersion || m.Snapshot != snapshot.ID || m.JobID != snapshot.JobID || m.Started != snapshot.Started || m.Bytes != snapshot.BytesTotal || len(m.Files) != len(files) {
		return errors.New("远端快照清单与本地记录不一致，已停止清理")
	}
	if m.Status != snapshot.Status && !(m.Status == "running" && snapshot.Status == "partial" && m.Completed == "") {
		return errors.New("远端快照状态与本地记录不一致，已停止清理")
	}
	if m.Status != "running" && m.Completed != snapshot.Completed {
		return errors.New("远端快照完成时间与本地记录不一致，已停止清理")
	}
	for i := range files {
		remote, local := m.Files[i], files[i]
		if remote.Path != local.Path || remote.SHA256 != local.SHA256 || remote.Size != local.Size {
			return errors.New("远端清单文件记录与本地快照不一致，已停止清理")
		}
		if remote.State != "planned" && remote.State != local.State {
			return errors.New("远端清单文件状态与本地快照不一致，已停止清理")
		}
		if remote.State == "done" && remote.RemoteETag != local.RemoteETag {
			return errors.New("远端清单 ETag 与本地快照不一致，已停止清理")
		}
	}
	return nil
}

func (s *Service) backupCleanupReceipts(snapshotID string) ([]backupCleanupReceipt, error) {
	rows, err := s.db.Query(`SELECT snapshot_id,object_path,etag,size,deleted FROM backup_cleanup_receipts WHERE snapshot_id=? ORDER BY object_path`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []backupCleanupReceipt{}
	for rows.Next() {
		var v backupCleanupReceipt
		if err = rows.Scan(&v.SnapshotID, &v.ObjectPath, &v.ETag, &v.Size, &v.Deleted); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) checkBackupCleanupPendingOperations(connectionID, base string) error {
	conn, err := s.connection(connectionID)
	if err != nil {
		return err
	}
	ns, basePath := resource(conn, base)
	rows, err := s.db.Query(`SELECT connection,path FROM operations WHERE state IN ('committing','uncertain')
	 UNION SELECT r.connection,r.path FROM operation_resources r JOIN operations o ON o.id=r.operation_id
	 WHERE o.state IN ('committing','uncertain')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var operationNS, operationPath string
		if err = rows.Scan(&operationNS, &operationPath); err != nil {
			return err
		}
		if operationNS == ns && pathOverlaps(operationPath, basePath) {
			return errors.New("备份对象存在未决写入或删除，请先核对操作日志")
		}
	}
	return rows.Err()
}

// committedBackupDelete returns durable journal evidence only for an exact,
// committed delete. Uncertain operations are deliberately never accepted.
func (s *Service) committedBackupDelete(connectionID, key, expectedETag string) (operationReceipt, bool, error) {
	conn, err := s.connection(connectionID)
	if err != nil {
		return operationReceipt{}, false, err
	}
	ns, resourcePath := resource(conn, key)
	rows, err := s.db.Query(`SELECT receipt FROM operations WHERE state='committed' AND kind='delete' AND connection=? AND path=? ORDER BY created DESC`, ns, resourcePath)
	if err != nil {
		return operationReceipt{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return operationReceipt{}, false, err
		}
		var evidence operationReceipt
		if err = json.Unmarshal([]byte(raw), &evidence); err != nil {
			continue
		}
		if evidence.Kind != "delete" || evidence.SourceConnection != connectionID || evidence.SourcePath != key || !strongTag(evidence.Source.ETag) {
			continue
		}
		if expectedETag != "" && evidence.Source.ETag != expectedETag {
			continue
		}
		return evidence, true, nil
	}
	return operationReceipt{}, false, rows.Err()
}

func (s *Service) saveRecoveredCleanupReceipt(snapshotID, objectPath, etag string, size int64) (backupCleanupReceipt, error) {
	if !strongTag(etag) || size < 0 {
		return backupCleanupReceipt{}, errors.New("删除提交记录不完整，无法恢复清理回执")
	}
	receipt := backupCleanupReceipt{SnapshotID: snapshotID, ObjectPath: objectPath, ETag: etag, Size: size, Deleted: time.Now().UTC().Format(time.RFC3339Nano)}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO backup_cleanup_receipts(snapshot_id,object_path,etag,size,deleted) VALUES(?,?,?,?,?)`, receipt.SnapshotID, receipt.ObjectPath, receipt.ETag, receipt.Size, receipt.Deleted)
	if err != nil {
		return backupCleanupReceipt{}, err
	}
	var saved backupCleanupReceipt
	err = s.db.QueryRow(`SELECT snapshot_id,object_path,etag,size,deleted FROM backup_cleanup_receipts WHERE snapshot_id=? AND object_path=?`, snapshotID, objectPath).Scan(&saved.SnapshotID, &saved.ObjectPath, &saved.ETag, &saved.Size, &saved.Deleted)
	if err != nil {
		return backupCleanupReceipt{}, err
	}
	if saved.ETag != etag || saved.Size != size {
		return backupCleanupReceipt{}, errors.New("现有清理回执与已提交删除记录不一致")
	}
	return saved, nil
}

func (s *Service) markBackupSnapshotCleaned(snapshotID string) error {
	if _, err := s.db.Exec("UPDATE backup_snapshots SET status='cleaned' WHERE id=? AND status IN ('partial','failed','error','paused','cancelled')", snapshotID); err != nil {
		return err
	}
	var status string
	if err := s.db.QueryRow("SELECT status FROM backup_snapshots WHERE id=?", snapshotID).Scan(&status); err != nil {
		return err
	}
	if status != "cleaned" {
		return errors.New("快照状态已变化，无法标记清理完成")
	}
	return nil
}

func listBackupCleanupTree(ctx context.Context, s *Service, connectionID, root string) (map[string]storage.Entry, map[string]bool, error) {
	st, err := s.store(connectionID)
	if err != nil {
		return nil, nil, err
	}
	objects, directories := map[string]storage.Entry{}, map[string]bool{}
	queue := []string{root}
	count := 0
	for len(queue) > 0 {
		if err = ctx.Err(); err != nil {
			return nil, nil, err
		}
		current := queue[0]
		queue = queue[1:]
		entries, listErr := st.List(ctx, current)
		if listErr != nil {
			if current == root && errors.Is(listErr, storage.ErrNotFound) {
				return objects, directories, storage.ErrNotFound
			}
			return nil, nil, listErr
		}
		for _, entry := range entries {
			count++
			if count > backupCleanupMaxObjects*3 || entry.Path == "" || path.Dir(entry.Path) != current || !strings.HasPrefix(entry.Path, root+"/") {
				return nil, nil, errors.New("远端目录列表路径无效或超出上限")
			}
			if entry.IsDir {
				if _, exists := directories[entry.Path]; exists {
					continue
				}
				directories[entry.Path] = true
				queue = append(queue, entry.Path)
				continue
			}
			if _, exists := objects[entry.Path]; exists {
				return nil, nil, errors.New("远端目录返回重复对象")
			}
			objects[entry.Path] = entry
		}
	}
	return objects, directories, nil
}

func cleanupExpectedDirectories(base string, files []BackupFile) map[string]bool {
	dirs := map[string]bool{}
	for _, f := range files {
		for current := path.Dir(path.Join(base, "files", f.Path)); current != base && current != "."; current = path.Dir(current) {
			dirs[current] = true
		}
	}
	return dirs
}

func backupFileForPath(files []BackupFile, key string) BackupFile {
	for _, f := range files {
		if f.Path == key {
			return f
		}
	}
	return BackupFile{}
}

func backupCleanupObjectBytes(objects []BackupCleanupObject) int64 {
	var bytes int64
	for _, object := range objects {
		bytes += object.Size
	}
	return bytes
}

func backupCleanupToken(data backupCleanupPlanData) string {
	raw, _ := json.Marshal(data)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// RunBackupCleanup revalidates the saved token and every conditional ETag while
// holding the existing backup-job lock. Each deletion receives its own durable
// receipt so a later preview can continue after partial progress.
func (s *Service) RunBackupCleanup(ctx context.Context, snapshotID, token string) (BackupCleanupResult, error) {
	result := BackupCleanupResult{SnapshotID: snapshotID, Status: "rejected", Deleted: []string{}, Remaining: []string{}, Errors: []string{}}
	if !validRecoveryID(snapshotID) || token == "" {
		return result, errors.New("请先生成清理预览")
	}
	if err := s.ensureBackupCleanupSchema(); err != nil {
		return result, err
	}
	first, job, err := s.backupSnapshot(snapshotID)
	if err != nil {
		return result, err
	}
	result.JobID = job.ID
	if first.Status == "running" {
		return result, errors.New("备份仍在执行，暂时不能清理")
	}
	unlock := s.lockBackupJob(job.ID)
	defer unlock()
	var savedToken, savedCreated string
	if err = s.db.QueryRow("SELECT token,created FROM backup_cleanup_plans WHERE snapshot_id=?", snapshotID).Scan(&savedToken, &savedCreated); err != nil {
		return result, errors.New("清理预览已失效，请重新预览")
	}
	created, parseErr := time.Parse(time.RFC3339Nano, savedCreated)
	if parseErr != nil || time.Since(created) > 15*time.Minute || created.After(time.Now().Add(time.Minute)) {
		return result, errors.New("清理预览已过期，请重新预览")
	}
	if savedToken != token {
		return result, storage.ErrConflict
	}
	plan, err := s.buildBackupCleanupPlan(ctx, snapshotID)
	if err != nil {
		return result, err
	}
	if backupCleanupToken(plan.data) != token {
		return result, storage.ErrConflict
	}
	if len(plan.data.Objects) == 0 {
		if backupCleanupFullyRecorded(plan.data) {
			if err = s.markBackupSnapshotCleaned(snapshotID); err != nil {
				return result, err
			}
			result.Status = "complete"
		} else {
			result.Status = "nothing_to_clean"
		}
		return result, nil
	}
	backend := Backend{Service: s, ConnectionID: plan.data.ConnectionID}
	cleanupCtx := withManagedBackup(withTransferScope(ctx, "backup-cleanup", snapshotID))
	for index, object := range plan.data.Objects {
		if err = cleanupCtx.Err(); err != nil {
			result.Status = "partial"
			result.Errors = append(result.Errors, "操作已取消")
			for _, remaining := range plan.data.Objects[index:] {
				result.Remaining = append(result.Remaining, remaining.Path)
			}
			return result, nil
		}
		key := plan.keys[object.Path]
		if key == "" {
			result.Status = "partial"
			result.Errors = append(result.Errors, "清理清单损坏，已停止")
			for _, remaining := range plan.data.Objects[index:] {
				result.Remaining = append(result.Remaining, remaining.Path)
			}
			return result, nil
		}
		if err = backend.Delete(cleanupCtx, key, storage.Condition{IfMatch: object.ETag}); err != nil {
			result.Status = "partial"
			result.Errors = append(result.Errors, object.Path+"："+safeBackupFailure(err))
			for _, remaining := range plan.data.Objects[index:] {
				result.Remaining = append(result.Remaining, remaining.Path)
			}
			return result, nil
		}
		_, receiptErr := s.db.Exec(`INSERT INTO backup_cleanup_receipts(snapshot_id,object_path,etag,size,deleted) VALUES(?,?,?,?,?)`, snapshotID, key, object.ETag, object.Size, time.Now().UTC().Format(time.RFC3339Nano))
		if receiptErr != nil {
			result.Status = "uncertain"
			result.Errors = append(result.Errors, object.Path+"：对象已删除但本地回执未保存，请先重新预览核对")
			for _, remaining := range plan.data.Objects[index+1:] {
				result.Remaining = append(result.Remaining, remaining.Path)
			}
			result.Remaining = append(result.Remaining, object.Path)
			return result, nil
		}
		result.Deleted = append(result.Deleted, object.Path)
	}
	remainingPlan, verifyErr := s.buildBackupCleanupPlan(ctx, snapshotID)
	if verifyErr != nil {
		result.Status = "uncertain"
		result.Errors = append(result.Errors, "删除后远端状态无法核对，请重新预览："+safeCleanupError(verifyErr))
		return result, nil
	}
	if len(remainingPlan.data.Objects) != 0 {
		result.Status = "partial"
		for _, object := range remainingPlan.data.Objects {
			result.Remaining = append(result.Remaining, object.Path)
		}
		result.Errors = append(result.Errors, "仍有对象未清理，请重新预览")
		return result, nil
	}
	if backupCleanupFullyRecorded(remainingPlan.data) {
		if err = s.markBackupSnapshotCleaned(snapshotID); err != nil {
			result.Status = "partial"
			result.Errors = append(result.Errors, "远端对象已清理，但本地状态未更新")
			return result, nil
		}
	}
	result.Status = "complete"
	return result, nil
}

func backupCleanupFullyRecorded(data backupCleanupPlanData) bool {
	seen := map[string]bool{}
	for _, receipt := range data.Receipts {
		seen[receipt.ObjectPath] = true
	}
	if !seen[path.Join(data.Base, "manifest.json")] {
		return false
	}
	for _, file := range data.Files {
		if file.State == "done" && !seen[path.Join(data.Base, "files", file.Path)] {
			return false
		}
	}
	return true
}

func safeCleanupError(err error) string {
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "远端对象已变化"
	case errors.Is(err, storage.ErrNotFound):
		return "远端对象缺失"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "操作已取消或超时"
	default:
		return "远端状态不明确"
	}
}
