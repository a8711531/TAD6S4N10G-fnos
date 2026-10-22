package powerguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 开关机计划：关机到点由守护进程自己触发 shutdown；开机靠主板 RTC 唤醒，
// 把下一次开机时刻写进 /sys/class/rtc/rtc0/wakealarm。功能参考群晖/威联通的
// 自动开关机：平日=周一至周五，假日=周六周日，每天=不限。
//
// 安全约束：
//   - 同一分钟只触发一次关机（powerScheduleFiredKey 去重），避免 30 秒 tick
//     在关机前的窗口里重复执行 shutdown。
//   - 触发关机前先把下一次开机时刻写进 wakealarm，保证「关机→定时开机」链路
//     不因关机动作本身丢掉唤醒设置。
//   - 测试一律走注入：时钟走 Manager.nowFn，shutdown 走 powerScheduleShutdownHook，
//     RTC 设备文件走 Manager.Root 映射，绝不真实关机或写真实 sysfs。

const powerScheduleMaxEntries = 32

var (
	powerScheduleTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
	// powerScheduleShutdownHook 测试注入的关机实现；默认调用系统 shutdown。
	powerScheduleShutdownHook = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "shutdown", "-h", "now").Run()
	}
)

// PowerScheduleEntry 一条开关机计划。
// Days: weekday(平日/周一至周五) | weekend(假日/周六周日) | daily(每天)；
// Time: 本地时间 HH:MM（24 小时制）；Action: poweron(开机) | poweroff(关机)。
type PowerScheduleEntry struct {
	Enabled bool   `json:"enabled"`
	Days    string `json:"days"`
	Time    string `json:"time"`
	Action  string `json:"action"`
}

type PowerScheduleConfig struct {
	Entries []PowerScheduleEntry `json:"entries,omitempty"`
}

// PowerScheduleStatus 下发给界面的完整状态：计划列表、统计、下次执行、
// RTC 当前时间/中断/已写入的唤醒时刻与「计划写入」是否同步。
type PowerScheduleStatus struct {
	Total        int                  `json:"total"`
	EnabledCount int                  `json:"enabled_count"`
	Entries      []PowerScheduleEntry `json:"entries"`
	NextTrigger  string               `json:"next_trigger"`  // 例：2026-10-23 01:00 关机；空 = 无启用计划
	RTCTime      string               `json:"rtc_time"`      // RTC 芯片当前时间（UTC），读不到为空
	RTCAlarmIRQ  string               `json:"rtc_alarm_irq"` // /proc/driver/rtc 的 alarm_IRQ（yes/no），读不到为空
	WakeAlarm    string               `json:"wakealarm"`     // 当前已写入的唤醒时刻（UTC），未设置为空
	Applied      bool                 `json:"applied"`       // wakealarm 与下次开机计划一致 = 计划写入成功
	Error        string               `json:"error"`
}

var (
	errPowerScheduleNoRTC = errors.New("未检测到 RTC 设备（/sys/class/rtc/rtc0），无法写入开机计划")
	errPowerScheduleNoOn  = errors.New("没有启用的开机计划，无需写入")
)

func powerScheduleActionLabel(action string) string {
	if action == "poweron" {
		return "开机"
	}
	return "关机"
}

// validatePowerSchedule 校验计划配置：条数上限、三字段枚举/格式。
func validatePowerSchedule(cfg PowerScheduleConfig) error {
	if len(cfg.Entries) > powerScheduleMaxEntries {
		return fmt.Errorf("计划条数超过 %d 条上限", powerScheduleMaxEntries)
	}
	for i, entry := range cfg.Entries {
		switch entry.Days {
		case "weekday", "weekend", "daily":
		default:
			return fmt.Errorf("第 %d 条计划的重复类型无效（%q）", i+1, entry.Days)
		}
		if !powerScheduleTimeRe.MatchString(entry.Time) {
			return fmt.Errorf("第 %d 条计划的时间格式无效（%q），应为 HH:MM", i+1, entry.Time)
		}
		switch entry.Action {
		case "poweron", "poweroff":
		default:
			return fmt.Errorf("第 %d 条计划的动作无效（%q）", i+1, entry.Action)
		}
	}
	return nil
}

// powerScheduleDaysMatch 判定触发日是否符合计划的重复类型（本地时区）。
func powerScheduleDaysMatch(day time.Time, days string) bool {
	switch days {
	case "daily":
		return true
	case "weekend":
		return day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
	default: // weekday
		return day.Weekday() >= time.Monday && day.Weekday() <= time.Friday
	}
}

// nextPowerScheduleTrigger 计算 from 之后最近一次触发时刻；actionFilter 非空时
// 只看该动作（"poweron"）。返回触发时刻与对应计划。
func nextPowerScheduleTrigger(entries []PowerScheduleEntry, from time.Time, actionFilter string) (time.Time, PowerScheduleEntry, bool) {
	var best time.Time
	var bestEntry PowerScheduleEntry
	for _, entry := range entries {
		if !entry.Enabled {
			continue
		}
		if actionFilter != "" && entry.Action != actionFilter {
			continue
		}
		// 配置文件可被手工编辑：非法时间格式在这里直接跳过，
		// 不能像保存路径那样假设已通过 validatePowerSchedule，否则切片越界 panic。
		if !powerScheduleTimeRe.MatchString(entry.Time) {
			continue
		}
		hour, _ := strconv.Atoi(entry.Time[:2])
		minute, _ := strconv.Atoi(entry.Time[3:])
		for offset := 0; offset < 8; offset++ {
			day := from.AddDate(0, 0, offset)
			candidate := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, from.Location())
			if !candidate.After(from) || !powerScheduleDaysMatch(candidate, entry.Days) {
				continue
			}
			if best.IsZero() || candidate.Before(best) {
				best = candidate
				bestEntry = entry
			}
			break // 该计划的最近一次已找到，不必再看更远的日期
		}
	}
	if best.IsZero() {
		return time.Time{}, PowerScheduleEntry{}, false
	}
	return best, bestEntry, true
}

// --- RTC 设备读写（全部走 rooted 映射，测试用临时目录替身）---

func (m *Manager) rtcWakealarmPath() string  { return m.rooted("/sys/class/rtc/rtc0/wakealarm") }
func (m *Manager) rtcSinceEpochPath() string { return m.rooted("/sys/class/rtc/rtc0/since_epoch") }
func (m *Manager) rtcProcPath() string       { return m.rooted("/proc/driver/rtc") }

// readRTCNow 读 RTC 芯片当前时刻（UTC）。读不到返回零值。
func (m *Manager) readRTCNow() time.Time {
	data, err := os.ReadFile(m.rtcSinceEpochPath())
	if err != nil {
		return time.Time{}
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || epoch <= 0 {
		return time.Time{}
	}
	return time.Unix(epoch, 0).UTC()
}

// readRTCAlarmIRQ 从 /proc/driver/rtc 解析 alarm_IRQ（yes/no）。读不到返回空。
func (m *Manager) readRTCAlarmIRQ() string {
	data, err := os.ReadFile(m.rtcProcPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.SplitN(line, ":", 2)
		if len(fields) == 2 && strings.TrimSpace(fields[0]) == "alarm_IRQ" {
			return strings.TrimSpace(fields[1])
		}
	}
	return ""
}

// readWakeAlarm 读当前已写入 wakealarm 的触发时刻（UTC）；未设置/读不到返回零值。
func (m *Manager) readWakeAlarm() (time.Time, error) {
	data, err := os.ReadFile(m.rtcWakealarmPath())
	if err != nil {
		return time.Time{}, err
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || epoch <= 0 {
		return time.Time{}, nil
	}
	return time.Unix(epoch, 0).UTC(), nil
}

// writeWakeAlarm 把触发时刻写入 wakealarm：先清零再写新值（内核要求的两步）。
func (m *Manager) writeWakeAlarm(at time.Time) error {
	path := m.rtcWakealarmPath()
	if _, err := os.Stat(path); err != nil {
		return errPowerScheduleNoRTC
	}
	if err := os.WriteFile(path, []byte("0"), 0o600); err != nil {
		return fmt.Errorf("清空 RTC 唤醒失败: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(at.Unix(), 10)), 0o600); err != nil {
		return fmt.Errorf("写入 RTC 唤醒失败: %w", err)
	}
	return nil
}

// nextPowerOnAt 返回下一次开机计划时刻；没有启用的开机计划返回 false。
func (m *Manager) nextPowerOnAt(from time.Time) (time.Time, bool) {
	cfg, err := m.loadConfigLocked()
	if err != nil {
		return time.Time{}, false
	}
	at, _, ok := nextPowerScheduleTrigger(cfg.PowerSchedule.Entries, from, "poweron")
	return at, ok
}

// applyPowerScheduleLocked 把下一次开机计划写进 RTC；没有开机计划时清空唤醒。
// 返回写入的时刻（清空返回零值）。调用方须持 m.mu。
func (m *Manager) applyPowerScheduleLocked() (time.Time, error) {
	now := m.now()
	next, ok := m.nextPowerOnAt(now)
	if !ok {
		if err := os.WriteFile(m.rtcWakealarmPath(), []byte("0"), 0o600); err != nil {
			if _, statErr := os.Stat(m.rtcWakealarmPath()); statErr != nil {
				return time.Time{}, errPowerScheduleNoRTC
			}
			return time.Time{}, fmt.Errorf("清空 RTC 唤醒失败: %w", err)
		}
		m.logf("power schedule: RTC wake alarm cleared (no enabled power-on entry)")
		return time.Time{}, nil
	}
	if err := m.writeWakeAlarm(next); err != nil {
		return time.Time{}, err
	}
	m.logf("power schedule: RTC wake alarm set to %s", next.Format("2006-01-02 15:04:05"))
	return next, nil
}

// ApplyPowerSchedule 立即把开机计划写进 RTC（界面「应用计划」按钮）。
func (m *Manager) ApplyPowerSchedule() (PowerScheduleStatus, error) {
	m.mu.Lock()
	_, err := m.applyPowerScheduleLocked()
	m.mu.Unlock()
	status := m.PowerScheduleStatus()
	if err != nil {
		status.Error = err.Error()
		return status, err
	}
	return status, nil
}

// TestPowerSchedule 写入一个 minutes 分钟后的 RTC 唤醒用于验证主板定时开机：
// 用户手动关机后观察是否自动上电。不改配置，仅覆盖当前 wakealarm；
// 保护窗口（测试时刻+1 分钟）内 tick 不按计划同步覆盖它，过期后自动恢复同步。
func (m *Manager) TestPowerSchedule(minutes int) (time.Time, error) {
	if minutes < 1 || minutes > 30 {
		return time.Time{}, fmt.Errorf("测试提前量需在 1–30 分钟之间")
	}
	at := m.now().Add(time.Duration(minutes) * time.Minute)
	if err := m.writeWakeAlarm(at); err != nil {
		return time.Time{}, err
	}
	m.mu.Lock()
	m.powerScheduleTestUntil = at.Add(time.Minute)
	m.logf("power schedule: test wake alarm set to %s (%d 分钟后)", at.Format("2006-01-02 15:04:05"), minutes)
	m.mu.Unlock()
	return at, nil
}

// PowerScheduleStatus 汇总界面所需的全部状态。
func (m *Manager) PowerScheduleStatus() PowerScheduleStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := PowerScheduleStatus{}
	cfg, err := m.loadConfigLocked()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Entries = append([]PowerScheduleEntry(nil), cfg.PowerSchedule.Entries...)
	status.Total = len(status.Entries)
	for _, entry := range status.Entries {
		if entry.Enabled {
			status.EnabledCount++
		}
	}
	now := m.now()
	if at, entry, ok := nextPowerScheduleTrigger(status.Entries, now, ""); ok {
		status.NextTrigger = fmt.Sprintf("%s %s", at.Format("2006-01-02 15:04"), powerScheduleActionLabel(entry.Action))
	}
	if rtcNow := m.readRTCNow(); !rtcNow.IsZero() {
		status.RTCTime = rtcNow.Format("2006-01-02 15:04:05")
	}
	status.RTCAlarmIRQ = m.readRTCAlarmIRQ()
	wake, wakeErr := m.readWakeAlarm()
	if wakeErr != nil {
		// RTC 缺失不算致命：计划编辑仍可用，只是无法写入
		if _, statErr := os.Stat(m.rtcWakealarmPath()); statErr != nil {
			status.Error = errPowerScheduleNoRTC.Error()
		}
	} else if !wake.IsZero() {
		status.WakeAlarm = wake.Format("2006-01-02 15:04:05")
		if next, ok := m.nextPowerOnAt(now); ok && wake.Equal(next.UTC()) {
			status.Applied = true
		}
	}
	return status
}

// SavePowerScheduleConfig 校验并保存计划列表，随后立刻把开机计划写进 RTC。
func (m *Manager) SavePowerScheduleConfig(schedule PowerScheduleConfig) error {
	if err := validatePowerSchedule(schedule); err != nil {
		m.mu.Lock()
		m.lastError = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.lastError = err.Error()
		return err
	}
	cfg.PowerSchedule = schedule
	if err := m.saveConfigLocked(cfg); err != nil {
		m.lastError = err.Error()
		return err
	}
	// 保存即同步 RTC：RTC 缺失只记日志不阻塞保存（笔记本/虚拟机环境仍要能编辑计划）
	if _, err := m.applyPowerScheduleLocked(); err != nil {
		m.logf("power schedule: apply after save failed: %v", err)
	}
	m.lastError = ""
	return nil
}

// PowerScheduleTick 定时检查（主循环每 30 秒一次）：
//  1. 到点的关机计划触发 shutdown（同一分钟去重，触发前先写开机唤醒）；
//  2. wakealarm 与下次开机计划不一致时重新同步（覆盖测试唤醒/意外清空）。
func (m *Manager) PowerScheduleTick(ctx context.Context) {
	m.mu.Lock()
	cfg, err := m.loadConfigLocked()
	if err != nil {
		m.mu.Unlock()
		return
	}
	now := m.now()
	firedKey := ""
	var fired bool
	for _, entry := range cfg.PowerSchedule.Entries {
		if !entry.Enabled || entry.Action != "poweroff" {
			continue
		}
		if entry.Time != now.Format("15:04") || !powerScheduleDaysMatch(now, entry.Days) {
			continue
		}
		key := now.Format("2006-01-02 ") + entry.Time
		if m.powerScheduleFiredKey == key {
			continue
		}
		m.powerScheduleFiredKey = key
		firedKey = key
		fired = true
		break
	}
	if fired {
		// 触发关机前先把开机唤醒写好，保证关机后能按计划自动上电
		if _, err := m.applyPowerScheduleLocked(); err != nil {
			m.logf("power schedule: apply wake alarm before shutdown failed: %v", err)
		}
		m.logf("power schedule: 触发计划关机（%s）", firedKey)
	}
	m.mu.Unlock()

	if fired {
		if err := powerScheduleShutdownHook(ctx); err != nil {
			m.mu.Lock()
			m.logf("power schedule: shutdown failed: %v", err)
			m.mu.Unlock()
		}
		return // 正在关机，本 tick 不再做唤醒同步
	}

	// 唤醒同步：读当前 wakealarm，与下次开机计划不一致才写，避免每 tick 写 sysfs。
	// 测试唤醒保护窗口内跳过同步，保留 TestPowerSchedule 写入的临时闹钟。
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Before(m.powerScheduleTestUntil) {
		return
	}
	next, hasNext := m.nextPowerOnAt(now)
	wake, wakeErr := m.readWakeAlarm()
	if wakeErr != nil {
		return // RTC 缺失：静默跳过，状态接口会展示
	}
	switch {
	case hasNext && !wake.Equal(next.UTC()):
		if _, err := m.applyPowerScheduleLocked(); err != nil {
			m.logf("power schedule: sync wake alarm failed: %v", err)
		}
	case !hasNext && !wake.IsZero():
		if _, err := m.applyPowerScheduleLocked(); err != nil {
			m.logf("power schedule: clear wake alarm failed: %v", err)
		}
	}
}
