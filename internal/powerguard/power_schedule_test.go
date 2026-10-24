package powerguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var powerScheduleTestZone = time.FixedZone("CST", 8*3600)

// newPowerScheduleTestManager 构造带假 RTC 设备与空配置的 Manager。
// nowFn 指向返回的 now 变量，测试可随时拨时间。
func newPowerScheduleTestManager(t *testing.T, now *time.Time) *Manager {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "sys/class/rtc/rtc0/wakealarm"), "0\n")
	rtcEpoch := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC).Unix()
	writeTestFile(t, filepath.Join(root, "sys/class/rtc/rtc0/since_epoch"), strconv.FormatInt(rtcEpoch, 10)+"\n")
	writeTestFile(t, filepath.Join(root, "proc/driver/rtc"), "rtc_time\t: 00:00:00\nrtc_date\t: 2026-10-23\nalrm_time\t: 01:00:00\nalarm_IRQ\t: yes\n")
	configPath := filepath.Join(root, "etc/config.json")
	writeTestFile(t, configPath, "{}")
	return &Manager{
		Root:       root,
		ConfigPath: configPath,
		nowFn:      func() time.Time { return *now },
	}
}

func readWakeAlarmFile(t *testing.T, m *Manager) string {
	t.Helper()
	data, err := os.ReadFile(m.rtcWakealarmPath())
	if err != nil {
		t.Fatalf("读取 wakealarm 失败: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func TestValidatePowerSchedule(t *testing.T) {
	valid := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "10:00", Action: "poweron"},
		{Enabled: false, Days: "weekend", Time: "02:00", Action: "poweroff"},
		{Enabled: true, Days: "daily", Time: "23:59", Action: "poweroff"},
	}}
	if err := validatePowerSchedule(valid); err != nil {
		t.Fatalf("合法配置被误判: %v", err)
	}
	bad := []PowerScheduleEntry{
		{Enabled: true, Days: "weekly", Time: "10:00", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "25:00", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "10:60", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "1:00", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "10:00", Action: "reboot"},
	}
	for i, entry := range bad {
		if err := validatePowerSchedule(PowerScheduleConfig{Entries: []PowerScheduleEntry{entry}}); err == nil {
			t.Fatalf("非法第 %d 条未被拦截: %+v", i, entry)
		}
	}
	tooMany := PowerScheduleConfig{}
	for i := 0; i < powerScheduleMaxEntries+1; i++ {
		tooMany.Entries = append(tooMany.Entries, PowerScheduleEntry{Enabled: true, Days: "daily", Time: "10:00", Action: "poweroff"})
	}
	if err := validatePowerSchedule(tooMany); err == nil {
		t.Fatalf("超过 %d 条上限未被拦截", powerScheduleMaxEntries)
	}
}

func TestNextPowerScheduleTrigger(t *testing.T) {
	// 2026-10-23 是周五
	friday := time.Date(2026, 10, 23, 9, 0, 0, 0, powerScheduleTestZone)
	entries := []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "10:00", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
		{Enabled: true, Days: "weekend", Time: "02:00", Action: "poweroff"},
		{Enabled: true, Days: "daily", Time: "08:00", Action: "poweron"},
		{Enabled: false, Days: "daily", Time: "09:30", Action: "poweroff"},
	}
	at, entry, ok := nextPowerScheduleTrigger(entries, friday, "")
	if !ok || entry.Time != "10:00" || at.Hour() != 10 || at.Day() != 23 {
		t.Fatalf("最近触发应为周五 10:00 开机，实际 %v %+v", at, entry)
	}
	// 平日 01:00 当天已过 → 下周一 01:00
	at, entry, ok = nextPowerScheduleTrigger(entries[:2], friday, "poweroff")
	if !ok || at.Weekday() != time.Monday || at.Day() != 26 {
		t.Fatalf("平日关机应跳到周一 01:00，实际 %v", at)
	}
	// 假日 02:00 → 周六（明天）
	at, _, ok = nextPowerScheduleTrigger(entries[2:3], friday, "")
	if !ok || at.Weekday() != time.Saturday || at.Day() != 24 {
		t.Fatalf("假日关机应跳到周六 02:00，实际 %v", at)
	}
	// 每天 08:00 当天已过 → 明天 08:00
	at, _, ok = nextPowerScheduleTrigger(entries[3:4], friday, "")
	if !ok || at.Day() != 24 || at.Hour() != 8 {
		t.Fatalf("每天 08:00 应排到明天，实际 %v", at)
	}
	// 动作过滤：只看开机
	_, entry, ok = nextPowerScheduleTrigger(entries, friday, "poweron")
	if !ok || entry.Action != "poweron" {
		t.Fatalf("过滤开机动作错误: %+v", entry)
	}
	// 当前分钟整点已过的计划不能算「未来」
	exact := []PowerScheduleEntry{{Enabled: true, Days: "daily", Time: "09:00", Action: "poweroff"}}
	at, _, ok = nextPowerScheduleTrigger(exact, friday.Add(20*time.Second), "")
	if !ok || at.Day() != 24 {
		t.Fatalf("09:00:20 时 09:00 的计划必须排到明天，实际 %v", at)
	}
	// 全部禁用 → 无触发
	if _, _, ok = nextPowerScheduleTrigger(entries[4:], friday, ""); ok {
		t.Fatalf("禁用的计划不得产生触发")
	}
	// 手工改坏配置（时间非法）时不得 panic，直接跳过该条
	broken := []PowerScheduleEntry{
		{Enabled: true, Days: "daily", Time: "", Action: "poweron"},
		{Enabled: true, Days: "daily", Time: "1:00", Action: "poweron"},
		{Enabled: true, Days: "daily", Time: "25:00", Action: "poweron"},
		{Enabled: true, Days: "daily", Time: "10:60", Action: "poweron"},
	}
	if _, _, ok = nextPowerScheduleTrigger(broken, friday, ""); ok {
		t.Fatalf("非法时间格式的计划不得产生触发")
	}
}

func TestSavePowerScheduleConfigAppliesWakeAlarm(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, powerScheduleTestZone) // 周五
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "daily", Time: "10:00", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	want := time.Date(2026, 10, 23, 10, 0, 0, 0, powerScheduleTestZone).Unix()
	if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(want, 10) {
		t.Fatalf("保存后 wakealarm 应为周五 10:00（%d），实际 %q", want, got)
	}
	// 配置真的落盘了（LoadOrCreateConfig 依赖 /proc/cpuinfo，测试环境没有，
	// 直接走 loadConfigLocked 读盘校验）
	m.mu.Lock()
	stored, err := m.loadConfigLocked()
	m.mu.Unlock()
	if err != nil || len(stored.PowerSchedule.Entries) != 2 {
		t.Fatalf("配置未落盘: %v %+v", err, stored.PowerSchedule)
	}
	// 非法配置：拒绝且不得改动已保存内容
	if err := m.SavePowerScheduleConfig(PowerScheduleConfig{Entries: []PowerScheduleEntry{{Enabled: true, Days: "bad", Time: "10:00", Action: "poweron"}}}); err == nil {
		t.Fatalf("非法配置必须拒绝")
	}
	m.mu.Lock()
	stored, _ = m.loadConfigLocked()
	m.mu.Unlock()
	if len(stored.PowerSchedule.Entries) != 2 {
		t.Fatalf("校验失败不得改写配置: %+v", stored.PowerSchedule)
	}
}

func TestPowerScheduleStatus(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, powerScheduleTestZone)
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "daily", Time: "10:00", Action: "poweron"},
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
		{Enabled: false, Days: "weekend", Time: "02:00", Action: "poweroff"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	status := m.PowerScheduleStatus()
	if status.Total != 3 || status.EnabledCount != 2 {
		t.Fatalf("统计错误: %+v", status)
	}
	if status.NextTrigger != "2026-10-23 10:00 开机" {
		t.Fatalf("下次执行文案错误: %q", status.NextTrigger)
	}
	if status.RTCTime != "2026-10-23 00:00:00" {
		t.Fatalf("RTC 时间错误: %q", status.RTCTime)
	}
	if status.RTCAlarmIRQ != "yes" {
		t.Fatalf("alarm_IRQ 错误: %q", status.RTCAlarmIRQ)
	}
	if !status.Applied {
		t.Fatalf("保存后 wakealarm 应与计划同步: %+v", status)
	}
	// 无 RTC 设备：状态接口仍可用，只报错误不 panic
	m2 := &Manager{Root: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "config.json"), nowFn: func() time.Time { return now }}
	writeTestFile(t, m2.ConfigPath, "{}")
	status2 := m2.PowerScheduleStatus()
	if status2.Error == "" {
		t.Fatalf("缺 RTC 应在状态里报错: %+v", status2)
	}
}

func TestPowerScheduleTickFiresShutdownOnce(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 0, 20, 0, powerScheduleTestZone) // 周五 01:00:20
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
		{Enabled: true, Days: "daily", Time: "10:00", Action: "poweron"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	calls := 0
	old := powerScheduleShutdownHook
	powerScheduleShutdownHook = func(ctx context.Context) error { calls++; return nil }
	defer func() { powerScheduleShutdownHook = old }()

	m.PowerScheduleTick(context.Background())
	if calls != 1 {
		t.Fatalf("到点必须触发一次关机，实际 %d 次", calls)
	}
	// 触发关机前必须先把下次开机写进 wakealarm
	want := time.Date(2026, 10, 23, 10, 0, 0, 0, powerScheduleTestZone).Unix()
	if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(want, 10) {
		t.Fatalf("关机前 wakealarm 应为 10:00（%d），实际 %q", want, got)
	}
	// 同一分钟再 tick 不得重复触发
	m.PowerScheduleTick(context.Background())
	if calls != 1 {
		t.Fatalf("同一分钟不得重复关机，实际 %d 次", calls)
	}
	// 过了这一分钟不再触发
	now = now.Add(time.Minute)
	m.PowerScheduleTick(context.Background())
	if calls != 1 {
		t.Fatalf("时间窗口外不得关机，实际 %d 次", calls)
	}
	// 假日计划周五不触发
	now = time.Date(2026, 10, 24, 2, 0, 20, 0, powerScheduleTestZone) // 周六 02:00
	m.PowerScheduleTick(context.Background())
	if calls != 1 {
		t.Fatalf("周六不应触发平日关机，实际 %d 次", calls)
	}
}

func TestPowerScheduleTickSkipsShutdownDuringFirmwareJob(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 0, 20, 0, powerScheduleTestZone) // 周五 01:00:20
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	calls := 0
	old := powerScheduleShutdownHook
	powerScheduleShutdownHook = func(ctx context.Context) error { calls++; return nil }
	defer func() { powerScheduleShutdownHook = old }()

	// 刷写任务运行中（真实 beginFirmwareJob 路径）：计划关机必须让路，
	// 且不得占用同分钟去重键
	injectFakeInhibitor(m)
	if err := m.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatalf("启动刷写失败: %v", err)
	}
	m.PowerScheduleTick(context.Background())
	if calls != 0 {
		t.Fatalf("固件刷写期间不得关机，实际 %d 次", calls)
	}
	m.mu.Lock()
	firedKey := m.powerScheduleFiredKey
	m.mu.Unlock()
	if firedKey != "" {
		t.Fatalf("跳过关机时不得占用去重键，实际 %q", firedKey)
	}

	// 刷写结束，同一分钟内下一 tick 补触发一次
	m.firmwareFinish(nil, "")
	m.PowerScheduleTick(context.Background())
	if calls != 1 {
		t.Fatalf("刷写结束后同一分钟应补触发一次，实际 %d 次", calls)
	}
}

func TestPowerScheduleTickSkipsShutdownNearPowerOn(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 0, 20, 0, powerScheduleTestZone) // 周五 01:00:20
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
		{Enabled: true, Days: "daily", Time: "01:05", Action: "poweron"}, // 4 分 40 秒后开机
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	calls := 0
	old := powerScheduleShutdownHook
	powerScheduleShutdownHook = func(ctx context.Context) error { calls++; return nil }
	defer func() { powerScheduleShutdownHook = old }()

	m.PowerScheduleTick(context.Background())
	if calls != 0 {
		t.Fatalf("距下次开机不足 10 分钟时不得关机，实际 %d 次", calls)
	}
	m.mu.Lock()
	firedKey := m.powerScheduleFiredKey
	m.mu.Unlock()
	if firedKey == "" {
		t.Fatalf("跳过关机后应占用同分钟去重键，避免反复尝试")
	}
	// 同一分钟再 tick 不得重复尝试，更不能关机
	m.PowerScheduleTick(context.Background())
	if calls != 0 {
		t.Fatalf("保护窗口内的重复 tick 不得关机，实际 %d 次", calls)
	}

	// 开机计划离得远（30 分钟后）时，同一关机时刻应正常触发
	now2 := time.Date(2026, 10, 23, 1, 0, 20, 0, powerScheduleTestZone)
	m2 := newPowerScheduleTestManager(t, &now2)
	cfg2 := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
		{Enabled: true, Days: "daily", Time: "01:30", Action: "poweron"},
	}}
	if err := m2.SavePowerScheduleConfig(cfg2); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	farCalls := 0
	powerScheduleShutdownHook = func(ctx context.Context) error { farCalls++; return nil }
	m2.PowerScheduleTick(context.Background())
	if farCalls != 1 {
		t.Fatalf("距开机超过保护窗口应正常关机，实际 %d 次", farCalls)
	}
}

func TestPowerScheduleTickSyncsWakeAlarm(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, powerScheduleTestZone)
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "daily", Time: "10:00", Action: "poweron"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 人为清空 wakealarm，tick 必须按计划重新写入
	if err := os.WriteFile(m.rtcWakealarmPath(), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.PowerScheduleTick(context.Background())
	want := time.Date(2026, 10, 23, 10, 0, 0, 0, powerScheduleTestZone).Unix()
	if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(want, 10) {
		t.Fatalf("tick 必须重写 wakealarm，实际 %q", got)
	}
	// 停用开机计划后，tick 必须清空 wakealarm
	cfg.Entries[0].Enabled = false
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if got := readWakeAlarmFile(t, m); got != "0" {
		t.Fatalf("无开机计划必须清空 wakealarm，实际 %q", got)
	}
}

// writeWakeAlarmFileRaw 模拟「别的软件」直接写 RTC 唤醒（绕过本模块的写入口，
// 不登记所有权）。用于验证无开机计划时的清理不会误伤外部闹钟。
func writeWakeAlarmFileRaw(t *testing.T, m *Manager, at time.Time) {
	t.Helper()
	if err := os.WriteFile(m.rtcWakealarmPath(), []byte(strconv.FormatInt(at.Unix(), 10)), 0o600); err != nil {
		t.Fatalf("模拟外部写入 wakealarm 失败: %v", err)
	}
}

// TestPowerScheduleWakeAlarmOwnership RTC 闹钟所有权：无开机计划时只能清理
// 本模块写入且当前仍匹配的闹钟，外部软件设置的唤醒必须原样保留。
func TestPowerScheduleWakeAlarmOwnership(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, powerScheduleTestZone) // 周五 09:00
	external := time.Date(2027, 1, 1, 8, 0, 0, 0, time.UTC)
	onCfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "daily", Time: "10:00", Action: "poweron"},
	}}
	planned := time.Date(2026, 10, 23, 10, 0, 0, 0, powerScheduleTestZone)

	t.Run("空配置且闹钟为空：同步无动作也不得报错", func(t *testing.T) {
		m := newPowerScheduleTestManager(t, &now)
		m.PowerScheduleTick(context.Background())
		if got := readWakeAlarmFile(t, m); got != "0" {
			t.Fatalf("空配置空闹钟应保持为空，实际 %q", got)
		}
		if _, err := m.ApplyPowerSchedule(); err != nil {
			t.Fatalf("无开机计划的应用不应报错: %v", err)
		}
		if got := readWakeAlarmFile(t, m); got != "0" {
			t.Fatalf("应用后仍应为空，实际 %q", got)
		}
	})

	t.Run("外部闹钟且无开机计划：不得清空", func(t *testing.T) {
		m := newPowerScheduleTestManager(t, &now)
		writeWakeAlarmFileRaw(t, m, external)
		m.PowerScheduleTick(context.Background())
		if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(external.Unix(), 10) {
			t.Fatalf("外部闹钟被误清空，实际 %q", got)
		}
		if _, err := m.ApplyPowerSchedule(); err != nil {
			t.Fatalf("应用失败: %v", err)
		}
		if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(external.Unix(), 10) {
			t.Fatalf("手动应用也不得清外部闹钟，实际 %q", got)
		}
	})

	t.Run("停用自己写的开机计划：清空自己的闹钟", func(t *testing.T) {
		m := newPowerScheduleTestManager(t, &now)
		if err := m.SavePowerScheduleConfig(onCfg); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(planned.Unix(), 10) {
			t.Fatalf("计划闹钟未写入，实际 %q", got)
		}
		if err := m.SavePowerScheduleConfig(PowerScheduleConfig{}); err != nil {
			t.Fatalf("停用计划失败: %v", err)
		}
		if got := readWakeAlarmFile(t, m); got != "0" {
			t.Fatalf("停用自己的计划应清空自己写入的闹钟，实际 %q", got)
		}
	})

	t.Run("自己的闹钟被外部改写后停用计划：不得清空", func(t *testing.T) {
		m := newPowerScheduleTestManager(t, &now)
		if err := m.SavePowerScheduleConfig(onCfg); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
		// 别的软件后来改写 wakealarm：所有权已失效，停用计划不得按旧值清空
		writeWakeAlarmFileRaw(t, m, external)
		if err := m.SavePowerScheduleConfig(PowerScheduleConfig{}); err != nil {
			t.Fatalf("停用计划失败: %v", err)
		}
		if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(external.Unix(), 10) {
			t.Fatalf("外部改写后的闹钟被误清空，实际 %q", got)
		}
		m.PowerScheduleTick(context.Background())
		if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(external.Unix(), 10) {
			t.Fatalf("tick 也不得清外部改写后的闹钟，实际 %q", got)
		}
	})
}

func TestTestPowerSchedule(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, powerScheduleTestZone)
	m := newPowerScheduleTestManager(t, &now)
	// 有一条开机计划，正常情况下 tick 会把它同步进 wakealarm
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "daily", Time: "10:00", Action: "poweron"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	at, err := m.TestPowerSchedule(3)
	if err != nil {
		t.Fatalf("测试唤醒写入失败: %v", err)
	}
	if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(at.Unix(), 10) {
		t.Fatalf("测试闹钟未写入: %q", got)
	}
	// 保护窗口内 tick 不得用计划覆盖测试闹钟
	m.PowerScheduleTick(context.Background())
	if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(at.Unix(), 10) {
		t.Fatalf("保护窗口内测试闹钟被覆盖: %q", got)
	}
	// 拨过保护窗口，tick 恢复按计划同步
	now = at.Add(2 * time.Minute)
	m.PowerScheduleTick(context.Background())
	want := time.Date(2026, 10, 23, 10, 0, 0, 0, powerScheduleTestZone).Unix()
	if got := readWakeAlarmFile(t, m); got != strconv.FormatInt(want, 10) {
		t.Fatalf("保护窗口过后应恢复计划同步: %q", got)
	}
	// 非法提前量
	if _, err := m.TestPowerSchedule(0); err == nil {
		t.Fatalf("0 分钟必须拒绝")
	}
	if _, err := m.TestPowerSchedule(31); err == nil {
		t.Fatalf("31 分钟必须拒绝")
	}
}

// 关机/刷写原子互斥（barrier/channel 并发桩）：hook 卡住模拟「关机命令已发出、
// 系统尚未断电」窗口，窗口内及关机成功后请求刷写都必须被 shutdownPending
// 占位拒绝，二者绝不能同时获准执行。
func TestPowerScheduleShutdownBlocksFirmwareStart(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 0, 20, 0, powerScheduleTestZone) // 周五 01:00:20
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	injectFakeInhibitor(m)

	entered := make(chan struct{})
	release := make(chan struct{})
	old := powerScheduleShutdownHook
	powerScheduleShutdownHook = func(ctx context.Context) error {
		close(entered) // 关机已提交，卡在「命令已发出、尚未断电」窗口
		<-release
		return nil
	}
	defer func() { powerScheduleShutdownHook = old }()

	done := make(chan struct{})
	go func() {
		m.PowerScheduleTick(context.Background())
		close(done)
	}()
	<-entered // 等关机 hook 真正进入，确保占位已置位

	if err := m.beginFirmwareJob(firmwareJobKindBIOS); !errors.Is(err, errShutdownPending) {
		t.Fatalf("关机窗口内不得开始刷写，实际 %v", err)
	}
	close(release)
	<-done
	// 关机命令成功发出后占位保留：真实场景进程即将随系统退出，期间一律拒绝刷写
	if err := m.beginFirmwareJob(firmwareJobKindBIOS); !errors.Is(err, errShutdownPending) {
		t.Fatalf("关机成功后占位必须保留，实际 %v", err)
	}
}

// 关机命令失败：系统不会关机，必须释放占位恢复允许刷写，不得把刷写永久挡死。
func TestPowerScheduleShutdownFailureReleasesSlot(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 0, 20, 0, powerScheduleTestZone)
	m := newPowerScheduleTestManager(t, &now)
	cfg := PowerScheduleConfig{Entries: []PowerScheduleEntry{
		{Enabled: true, Days: "weekday", Time: "01:00", Action: "poweroff"},
	}}
	if err := m.SavePowerScheduleConfig(cfg); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	injectFakeInhibitor(m)
	old := powerScheduleShutdownHook
	powerScheduleShutdownHook = func(ctx context.Context) error { return errors.New("shutdown 命令失败") }
	defer func() { powerScheduleShutdownHook = old }()

	m.PowerScheduleTick(context.Background())
	if err := m.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatalf("关机失败后应恢复允许刷写: %v", err)
	}
	m.firmwareFinish(nil, "")
}
