package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"tamiops/internal/systemstate"
	"time"
)

// ExecutionRules limits automatic work. Weekdays use time.Weekday numbering:
// Sunday is 0. An empty weekday or network list means unrestricted.
type ExecutionRules struct {
	Enabled  bool     `json:"enabled"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Weekdays []int    `json:"weekdays"`
	Networks []string `json:"networks"`
	OnlyOnAC bool     `json:"onlyOnAC"`
}

type AutomationEnvironment struct {
	NetworkInterfaces []string `json:"networkInterfaces"`
	OnAC              bool     `json:"onAC"`
	PowerKnown        bool     `json:"powerKnown"`
}

func ValidateExecutionRules(r ExecutionRules) error {
	if (r.Start == "") != (r.End == "") {
		return errors.New("开始和结束时间必须同时填写")
	}
	if r.Start != "" {
		start, err := parseExecutionClock(r.Start)
		if err != nil {
			return fmt.Errorf("开始时间无效：%w", err)
		}
		end, err := parseExecutionClock(r.End)
		if err != nil {
			return fmt.Errorf("结束时间无效：%w", err)
		}
		if start == end {
			return errors.New("开始和结束时间不能相同")
		}
	}
	weekdays := map[int]bool{}
	for _, day := range r.Weekdays {
		if day < 0 || day > 6 {
			return fmt.Errorf("星期编号必须在 0 至 6 之间：%d", day)
		}
		if weekdays[day] {
			return fmt.Errorf("星期编号重复：%d", day)
		}
		weekdays[day] = true
	}
	networks := map[string]bool{}
	for _, name := range r.Networks {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 256 {
			return errors.New("网络名称不能为空且不能超过 256 个字符")
		}
		if networks[name] {
			return fmt.Errorf("网络名称重复：%s", name)
		}
		networks[name] = true
	}
	return nil
}

func parseExecutionClock(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, errors.New("请使用 HH:MM 格式")
	}
	for i := range value {
		if i == 2 {
			continue
		}
		if value[i] < '0' || value[i] > '9' {
			return 0, errors.New("请使用 HH:MM 格式")
		}
	}
	hour, errHour := strconv.Atoi(value[:2])
	minute, errMinute := strconv.Atoi(value[3:])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, errors.New("请使用有效的 24 小时时间")
	}
	return hour*60 + minute, nil
}

func (s *Service) AutomationAllowed(ctx context.Context, now time.Time) (bool, string) {
	rules := s.Preferences().Rules
	if err := ValidateExecutionRules(rules); err != nil {
		return false, err.Error()
	}
	if !rules.Enabled {
		return true, "自动执行时间规则未启用"
	}
	state := systemstate.Read(ctx)
	return executionAllowed(rules, now, state)
}

func executionAllowed(r ExecutionRules, now time.Time, state systemstate.Snapshot) (bool, string) {
	if !r.Enabled {
		return true, "自动执行时间规则未启用"
	}
	startDay := now.Weekday()
	within := true
	if r.Start != "" {
		start, _ := parseExecutionClock(r.Start)
		end, _ := parseExecutionClock(r.End)
		minute := now.Hour()*60 + now.Minute()
		within = false
		if start < end {
			within = minute >= start && minute < end
		} else if minute >= start {
			within = true
		} else if minute < end {
			within = true
			startDay = time.Date(now.Year(), now.Month(), now.Day()-1, 12, 0, 0, 0, now.Location()).Weekday()
		}
	}
	if !within || !executionWeekdayAllowed(r.Weekdays, startDay) {
		return false, "当前时间不在自动执行时段或星期范围内"
	}
	if len(r.Networks) != 0 {
		active := make(map[string]bool, len(state.NetworkInterfaces))
		for _, name := range state.NetworkInterfaces {
			active[name] = true
		}
		matched := false
		for _, name := range r.Networks {
			if active[strings.TrimSpace(name)] {
				matched = true
				break
			}
		}
		if !matched {
			return false, "当前没有连接允许的活动网络"
		}
	}
	if r.OnlyOnAC {
		if !state.PowerKnown {
			return false, "无法确认当前电源状态"
		}
		if !state.OnAC {
			return false, "当前未接入交流电源"
		}
	}
	return true, "当前系统状态符合自动执行规则"
}

func executionWeekdayAllowed(weekdays []int, day time.Weekday) bool {
	if len(weekdays) == 0 {
		return true
	}
	for _, allowed := range weekdays {
		if allowed == int(day) {
			return true
		}
	}
	return false
}

// Environment returns current host state for a user-facing status view. It
// reports power certainty explicitly because some systems cannot expose AC
// status through a supported interface.
func (s *Service) Environment() AutomationEnvironment {
	state := systemstate.Read(context.Background())
	return AutomationEnvironment{
		NetworkInterfaces: append([]string(nil), state.NetworkInterfaces...),
		OnAC:              state.OnAC,
		PowerKnown:        state.PowerKnown,
	}
}
