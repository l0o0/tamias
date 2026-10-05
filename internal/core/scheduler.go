package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type scheduleObservation struct {
	localFingerprint        string
	changedAt               time.Time
	lastFullScan            time.Time
	lastTry                 time.Time
	localDirty              bool
	remoteToken             string
	pendingOperationBlocked bool
}

var schedulerObservations sync.Map // map[schedulerKey]scheduleObservation

type schedulerKey struct {
	service *Service
	job     string
}

const (
	schedulerPollInterval = 2 * time.Second
	schedulerRemotePoll   = 60 * time.Second
	schedulerDebounce     = 900 * time.Millisecond
	schedulerRetryBackoff = 20 * time.Second
)

// StartScheduler starts periodic scans and the polling watcher. It is safe to
// call more than once; the service owns exactly one scheduler loop.
func (s *Service) StartScheduler() {
	s.mu.Lock()
	if s.closing || s.schedulerCancel != nil {
		s.mu.Unlock()
		return
	}
	if s.schedulerWake == nil {
		s.schedulerWake = make(chan struct{}, 1)
	}
	if s.schedulerChanged == nil {
		s.schedulerChanged = make(chan struct{}, 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.schedulerCancel, s.schedulerDone = cancel, done
	s.mu.Unlock()
	s.startAutomaticConnectionDetection()
	go s.schedulerLoop(ctx, done)
}

// StopScheduler stops scans, cancels active transfers, and waits for every
// service-owned run to finish before returning. It is safe to call repeatedly.
func (s *Service) StopScheduler() {
	s.mu.Lock()
	cancel, done := s.schedulerCancel, s.schedulerDone
	active := make([]context.CancelFunc, 0, len(s.jobCancels))
	for _, stop := range s.jobCancels {
		active = append(active, stop)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, stop := range active {
		stop()
	}
	if done != nil {
		<-done
	}
	for {
		s.mu.Lock()
		remaining := len(s.jobCancels)
		s.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.mu.Lock()
	if s.schedulerDone == done {
		s.schedulerCancel = nil
		s.schedulerDone = nil
		// Services are long-lived, but stopped/closed services must not remain
		// strongly referenced by the process-wide observation map. Keep Start
		// from racing this cleanup by doing it under the service mutex.
		schedulerObservations.Range(func(key, _ any) bool {
			if k, ok := key.(schedulerKey); ok && k.service == s {
				schedulerObservations.Delete(key)
			}
			return true
		})
	}
	s.mu.Unlock()
}

// WakeScheduler asks the next loop iteration to perform a full reconciliation.
func (s *Service) WakeScheduler() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if s.schedulerWake == nil {
		s.schedulerWake = make(chan struct{}, 1)
	}
	wake, running := s.schedulerWake, s.schedulerCancel != nil
	s.mu.Unlock()
	if running {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (s *Service) schedulerLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	// Reconcile interrupted queue items on startup. Saved queue rows never
	// authorize stale writes: every retry gets a fresh Preview.
	s.requeueInterrupted()
	s.schedulerCycle(ctx, true)
	ticker := time.NewTicker(schedulerPollInterval)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		wake, changed := s.schedulerWake, s.schedulerChanged
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.schedulerCycle(ctx, false)
		case <-wake:
			s.schedulerCycle(ctx, true)
		case <-changed:
			s.schedulerCycle(ctx, false)
		}
	}
}

func (s *Service) requeueInterrupted() {
	now := time.Now().Format(time.RFC3339Nano)
	_, _ = s.db.Exec("UPDATE sync_queue SET state='pending',error='',updated=? WHERE state='running' OR (state='error' AND error='context canceled')", now)
	s.mu.Lock()
	changed := false
	for i := range s.cfg.Jobs {
		j := &s.cfg.Jobs[i]
		if j.Status == "running" || j.Status == "retrying" {
			j.Status = "error"
			j.Detail = "上次运行意外中断，正在重新核对未完成操作"
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
	s.mu.Unlock()
}

// localMetadataFingerprint intentionally reads only directory entries and
// metadata. It is cheap enough for a 2-second watch tick; content hashes and
// remote enumeration happen only after debounce or a full-poll trigger.
func localMetadataFingerprint(ctx context.Context, localPath string, excludes []string) (string, error) {
	if isManagedPath(filepath.ToSlash(localPath)) {
		return "", errors.New("本地同步目录位于备份任务管理的保留目录中")
	}
	root, err := os.OpenRoot(localPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	h := sha256.New()
	count := 0
	err = fs.WalkDir(root.FS(), ".", func(key string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if key == "." {
			return nil
		}
		if isManagedPath(filepath.ToSlash(key)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if excludedPath(excludes, filepath.ToSlash(key)) {
				return filepath.SkipDir
			}
		} else if syncPathExcluded(excludes, filepath.ToSlash(key)) {
			return nil
		}
		// Symlinks are deliberately outside the synchronization tree. In
		// particular, never stat their targets while polling for changes.
		if entry.Type()&os.ModeSymlink != 0 {
			fmt.Fprintf(h, "symlink:%s\n", filepath.ToSlash(key))
			return nil
		}
		count++
		if count > 10000 {
			return errors.New("原型每任务最多 10000 个本地条目")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\n", filepath.ToSlash(key), info.Size(), info.ModTime().UnixNano(), info.Mode())
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) schedulerCycle(ctx context.Context, force bool) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	jobs := append([]Job(nil), s.cfg.Jobs...)
	s.mu.Unlock()
	now := time.Now()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		detecting := s.connectionDetectionPending[j.ConnectionID]
		s.mu.Unlock()
		if detecting {
			continue
		}
		if !j.Enabled || j.LocalPath == "" || j.Status == "running" || j.Status == "retrying" || j.Status == "scanning" {
			continue
		}
		pending := s.hasPendingQueue(j.ID)
		watching := j.Watch
		dirtyToken := s.remoteChangeToken(j.ID)
		manual := !watching && j.ScheduleMinutes <= 0
		key := schedulerKey{s, j.ID}
		state := scheduleObservation{}
		if previous, ok := schedulerObservations.Load(key); ok {
			state, _ = previous.(scheduleObservation)
		}
		blockedResolved := false
		if state.pendingOperationBlocked {
			stillBlocked, err := s.hasJobPendingOperations(j)
			// Fail closed if the blocker query itself fails. The next cycle can
			// retry the local database check without repeating a remote sync.
			if err != nil || stillBlocked {
				continue
			}
			state.pendingOperationBlocked = false
			state.lastTry = time.Time{}
			blockedResolved = true
			schedulerObservations.Store(key, state)
		}
		if manual && dirtyToken == "" && !pending && !blockedResolved {
			continue
		}
		if !force && (dirtyToken == "" || dirtyToken == state.remoteToken) && !state.lastTry.IsZero() && now.Sub(state.lastTry) < schedulerRetryBackoff {
			continue
		}

		if watching {
			fingerprint, err := localMetadataFingerprint(ctx, j.LocalPath, j.Exclude)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				state.lastTry = now
				schedulerObservations.Store(key, state)
				s.recordSchedulerResult(j.ID, now, "error", "本地变更检查失败："+err.Error(), false)
				continue
			}
			if state.localFingerprint == "" {
				state.localFingerprint = fingerprint
				if !force {
					state.localDirty, state.changedAt = true, now
				}
			} else if state.localFingerprint != fingerprint {
				state.localFingerprint = fingerprint
				state.localDirty, state.changedAt = true, now
			}
		}

		interval := schedulerRemotePoll
		if j.ScheduleMinutes > 0 {
			scheduledInterval := time.Duration(j.ScheduleMinutes) * time.Minute
			if scheduledInterval > interval {
				interval = scheduledInterval
			}
		}
		scheduledDue := j.ScheduleMinutes > 0 && scheduleIsDue(j.LastRun, j.ScheduleMinutes, now)
		if !state.lastFullScan.IsZero() && j.ScheduleMinutes > 0 {
			scheduledDue = now.Sub(state.lastFullScan) >= time.Duration(j.ScheduleMinutes)*time.Minute
		}
		remotePollDue := state.lastFullScan.IsZero() || now.Sub(state.lastFullScan) >= interval
		localDebounced := state.localDirty && !state.changedAt.IsZero() && now.Sub(state.changedAt) >= schedulerDebounce
		shouldScan := force || blockedResolved || dirtyToken != "" || pending || scheduledDue || watching && (localDebounced || remotePollDue)
		if !shouldScan {
			schedulerObservations.Store(key, state)
			continue
		}
		state.remoteToken = dirtyToken
		allowed, reason := s.AutomationAllowed(ctx, now)
		if !allowed {
			state.lastTry = now
			schedulerObservations.Store(key, state)
			if reason == "" {
				reason = "当前系统状态不符合自动执行规则"
			}
			s.recordSchedulerResult(j.ID, now, "waiting", reason, false)
			continue
		}

		plan, err := s.Preview(ctx, j.ID)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			state.lastFullScan, state.lastTry = now, now
			if errors.Is(err, ErrPendingOperation) {
				state.pendingOperationBlocked = true
				state.lastTry = time.Time{}
				schedulerObservations.Store(key, state)
				s.recordSchedulerResult(j.ID, now, "needs_attention", err.Error(), false)
				continue
			}
			schedulerObservations.Store(key, state)
			s.recordSchedulerResult(j.ID, now, "error", "自动核对失败："+err.Error(), scheduledDue)
			continue
		}
		latest, latestErr := s.job(j.ID)
		if latestErr != nil || !latest.Enabled {
			continue
		}
		manual = !latest.Watch && latest.ScheduleMinutes <= 0
		if !manual {
			allowed, reason = s.AutomationAllowed(ctx, time.Now())
			if !allowed {
				state.lastTry = now
				schedulerObservations.Store(key, state)
				s.recordSchedulerResult(j.ID, now, "waiting", reason, false)
				continue
			}
		}
		if !s.acceptSchedulerScan(j.ID, dirtyToken, pending, now) {
			continue
		}
		state.lastFullScan = now
		state.lastTry = time.Time{}
		state.localDirty = false
		schedulerObservations.Store(key, state)
		actionable, conflicted := false, false
		for _, a := range plan.Actions {
			if a.Kind == "conflict" {
				conflicted = true
			}
			if a.Kind != "skip" || a.BaselineMerge || a.ForgetBaseline {
				actionable = true
			}
		}
		if !actionable {
			s.recordSchedulerResult(j.ID, now, "synced", "已核对，没有待同步的更改", scheduledDue)
			continue
		}
		if manual {
			detail := "远端内容发生变化，请检查预览后执行"
			if pending {
				detail = "发现未完成操作，请检查预览后执行"
			}
			s.recordSchedulerResult(j.ID, now, "needs_attention", detail, false)
			continue
		}
		if conflicted || plan.RequiresDeleteConfirmation {
			why := "自动核对发现冲突，请选择要保留的版本"
			if plan.RequiresDeleteConfirmation {
				why = fmt.Sprintf("待确认删除 %d 个文件，请检查预览", plan.DeleteCount)
			}
			state.lastTry = now
			schedulerObservations.Store(key, state)
			s.recordSchedulerResult(j.ID, now, "needs_attention", why, scheduledDue)
			continue
		}
		_, err = s.RunPlan(ctx, j.ID, plan.Token)
		if err != nil && !errors.Is(err, context.Canceled) {
			if errors.Is(err, ErrPendingOperation) {
				state.pendingOperationBlocked = true
				state.lastTry = time.Time{}
				schedulerObservations.Store(key, state)
				s.recordSchedulerResult(j.ID, now, "needs_attention", err.Error(), false)
				continue
			}
			state.lastTry = now
			schedulerObservations.Store(key, state)
			s.recordSchedulerResult(j.ID, now, "error", "自动执行失败："+err.Error(), scheduledDue)
			continue
		}
		if err == nil {
			state.lastFullScan = now
			state.lastTry = time.Time{}
			state.localDirty = false
			schedulerObservations.Store(key, state)
		}
	}
}

// Consume recheck signals only while the task is still enabled. CancelJob
// shares this mutex, so a pause during a scan keeps pending work discoverable.
func (s *Service) acceptSchedulerScan(id, token string, pending bool, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.cfg.Jobs {
		if j.ID != id {
			continue
		}
		if !j.Enabled || j.Status == "running" || j.Status == "scanning" {
			return false
		}
		tx, err := s.db.Begin()
		if err != nil {
			return false
		}
		defer tx.Rollback()
		if _, err = tx.Exec("DELETE FROM sync_dirty WHERE job=? AND token=?", id, token); err != nil {
			return false
		}
		if pending {
			if _, err = tx.Exec("UPDATE sync_queue SET state='done',error='',updated=? WHERE job=? AND state='pending'", now.Format(time.RFC3339Nano), id); err != nil {
				return false
			}
		}
		return tx.Commit() == nil
	}
	return false
}

func scheduleIsDue(last string, minutes int, now time.Time) bool {
	if minutes <= 0 {
		return false
	}
	if strings.TrimSpace(last) == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, last)
	return err != nil || now.Sub(t) >= time.Duration(minutes)*time.Minute
}

func (s *Service) hasPendingQueue(jobID string) bool {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM sync_queue WHERE job=? AND state='pending'", jobID).Scan(&n)
	return err == nil && n > 0
}

func (s *Service) recordSchedulerResult(id string, now time.Time, status, detail string, updateLastRun bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		j := &s.cfg.Jobs[i]
		if j.ID != id || !j.Enabled || j.Status == "running" || j.Status == "scanning" {
			continue
		}
		changed := false
		if j.Status != status {
			j.Status, changed = status, true
		}
		if j.Detail != detail {
			j.Detail, changed = detail, true
		}
		if updateLastRun {
			last := now.Format(time.RFC3339)
			if j.LastRun != last {
				j.LastRun, changed = last, true
			}
		}
		if changed {
			_ = s.saveLocked()
		}
		return
	}
}

func (s *Service) setJobAttention(id, detail, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Jobs {
		j := &s.cfg.Jobs[i]
		if j.ID == id && j.Status != "running" && j.Enabled {
			j.Status, j.Detail = status, detail
			_ = s.saveLocked()
			return
		}
	}
}

// A scheduled retry always performs a fresh preview and never replays stale
// queue content as authority for a write.
func (s *Service) RetryPending(ctx context.Context, id string) (Job, error) {
	if _, err := s.job(id); err != nil {
		return Job{}, err
	}
	p, err := s.Preview(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if p.RequiresDeleteConfirmation {
		return Job{}, errors.New("删除数量超过阈值，请先检查并确认预览")
	}
	return s.RunPlan(ctx, id, p.Token)
}
