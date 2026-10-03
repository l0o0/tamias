package core

import (
	"context"
	"os"
	"path/filepath"
	"tamiops/internal/systemstate"
	"testing"
	"time"
)

func TestValidateExecutionRules(t *testing.T) {
	valid := ExecutionRules{Enabled: true, Start: "22:15", End: "06:30", Weekdays: []int{0, 5}, Networks: []string{"en0"}, OnlyOnAC: true}
	if err := ValidateExecutionRules(valid); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	for _, invalid := range []ExecutionRules{
		{Enabled: true, Start: "8:00", End: "17:00"},
		{Enabled: true, Start: "12:00", End: "12:00"},
		{Enabled: true, Start: "24:00", End: "01:00"},
		{Enabled: true, Start: "12:00", End: "13:00", Weekdays: []int{7}},
		{Enabled: true, Start: "12:00", End: "13:00", Networks: []string{"en0", "en0"}},
	} {
		if err := ValidateExecutionRules(invalid); err == nil {
			t.Errorf("invalid rules accepted: %#v", invalid)
		}
	}
}

func TestExecutionRulesCrossMidnightUsesStartWeekday(t *testing.T) {
	zone := time.FixedZone("local", 8*60*60)
	rules := ExecutionRules{Enabled: true, Start: "22:00", End: "02:00", Weekdays: []int{int(time.Friday)}}
	fridayNight := time.Date(2025, time.January, 3, 23, 0, 0, 0, zone)
	saturdayEarly := time.Date(2025, time.January, 4, 1, 59, 0, 0, zone)
	saturdayEnd := time.Date(2025, time.January, 4, 2, 0, 0, 0, zone)
	saturdayNight := time.Date(2025, time.January, 4, 23, 0, 0, 0, zone)
	state := systemstate.Snapshot{}
	for _, now := range []time.Time{fridayNight, saturdayEarly} {
		if allowed, reason := executionAllowed(rules, now, state); !allowed {
			t.Errorf("start-day window rejected %s: %s", now, reason)
		}
	}
	for _, now := range []time.Time{saturdayEnd, saturdayNight} {
		if allowed, _ := executionAllowed(rules, now, state); allowed {
			t.Errorf("execution allowed outside Friday window at %s", now)
		}
	}
}

func TestAutomationAllowedUsesInjectedNetworkAndPowerState(t *testing.T) {
	s, _ := testService(t)
	prefs := s.Preferences()
	prefs.Rules = ExecutionRules{Enabled: true, Start: "09:00", End: "17:00", Weekdays: []int{int(time.Monday)}, Networks: []string{"en0"}, OnlyOnAC: true}
	if err := s.SetPreferences(prefs); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, time.January, 6, 10, 0, 0, 0, time.UTC) // Monday
	ctx := systemstate.WithSnapshot(context.Background(), systemstate.Snapshot{NetworkInterfaces: []string{"wlan0"}, OnAC: true, PowerKnown: true})
	if allowed, reason := s.AutomationAllowed(ctx, now); allowed || reason != "当前没有连接允许的活动网络" {
		t.Fatalf("network mismatch = %v, %q", allowed, reason)
	}
	ctx = systemstate.WithSnapshot(context.Background(), systemstate.Snapshot{NetworkInterfaces: []string{"en0"}})
	if allowed, reason := s.AutomationAllowed(ctx, now); allowed || reason != "无法确认当前电源状态" {
		t.Fatalf("unknown power state = %v, %q", allowed, reason)
	}
	ctx = systemstate.WithSnapshot(context.Background(), systemstate.Snapshot{NetworkInterfaces: []string{"en0"}, OnAC: false, PowerKnown: true})
	if allowed, reason := s.AutomationAllowed(ctx, now); allowed || reason != "当前未接入交流电源" {
		t.Fatalf("battery state = %v, %q", allowed, reason)
	}
	ctx = systemstate.WithSnapshot(context.Background(), systemstate.Snapshot{NetworkInterfaces: []string{"en0"}, OnAC: true, PowerKnown: true})
	if allowed, reason := s.AutomationAllowed(ctx, now); !allowed || reason != "当前系统状态符合自动执行规则" {
		t.Fatalf("matching state = %v, %q", allowed, reason)
	}
}

func TestRunDueBackupsObeysRulesButManualRunBypasses(t *testing.T) {
	s, id := testService(t)
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "file.txt"), []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateBackupJob(BackupJob{Name: "rule-gated", ConnectionID: id, LocalPath: local, RemotePrefix: "rule-gated", ScheduleMinutes: 1, Enabled: true, RetainRecent: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	start := now.Add(2 * time.Minute).Format("15:04")
	end := now.Add(3 * time.Minute).Format("15:04")
	if start == end {
		start = now.Add(4 * time.Minute).Format("15:04")
		end = now.Add(5 * time.Minute).Format("15:04")
	}
	prefs := s.Preferences()
	prefs.Rules = ExecutionRules{Enabled: true, Start: start, End: end}
	if err = s.SetPreferences(prefs); err != nil {
		t.Fatal(err)
	}
	ctx := systemstate.WithSnapshot(context.Background(), systemstate.Snapshot{PowerKnown: false})
	if snapshots, runErr := s.RunDueBackups(ctx); runErr != nil || len(snapshots) != 0 {
		t.Fatalf("scheduled backup ignored time rules: snapshots=%#v err=%v", snapshots, runErr)
	}
	waiting, err := s.backupJob(job.ID)
	if err != nil || waiting.LastRun != "" || waiting.Status != "waiting" {
		t.Fatalf("blocked schedule should remain due with waiting status: %#v err=%v", waiting, err)
	}
	plan, err := s.PreviewBackup(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RunBackup(context.Background(), job.ID, plan.Token)
	if err != nil || snapshot.Status != "complete" {
		t.Fatalf("manual backup was blocked by automatic rules: %#v err=%v", snapshot, err)
	}
}

func TestDisabledExecutionRulesAllowAutomaticWork(t *testing.T) {
	s, _ := testService(t)
	allowed, reason := s.AutomationAllowed(context.Background(), time.Date(2025, 1, 6, 2, 0, 0, 0, time.UTC))
	if !allowed || reason != "自动执行时间规则未启用" {
		t.Fatalf("disabled rules returned %v, %q", allowed, reason)
	}
	if err := ValidateExecutionRules(ExecutionRules{Enabled: true}); err != nil {
		t.Fatalf("an all-day rule should be valid: %v", err)
	}
	rules := ExecutionRules{Enabled: true, Weekdays: []int{int(time.Monday)}, OnlyOnAC: true}
	state := systemstate.Snapshot{OnAC: true, PowerKnown: true}
	if allowed, reason := executionAllowed(rules, time.Date(2025, 1, 6, 2, 0, 0, 0, time.UTC), state); !allowed {
		t.Fatalf("all-day weekday/power rule rejected: %s", reason)
	}
	if allowed, _ := executionAllowed(rules, time.Date(2025, 1, 5, 2, 0, 0, 0, time.UTC), state); allowed {
		t.Fatal("all-day rule ignored its weekday constraint")
	}
}
