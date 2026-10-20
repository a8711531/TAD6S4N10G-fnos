package powerguard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFanStateCaptureRejectsDifferentCPU(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "cpuinfo"), []byte("model name : Intel(R) Processor N100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root, StatePath: filepath.Join(root, "original-state.json")}
	state := OriginalState{Version: stateVersion, CPUModel: "Intel(R) Core(TM) i3-N305"}
	if err := writeJSONAtomic(manager.StatePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	err := manager.captureOriginalFanLocked(FanDevice{ID: "fan1", PWM: 128, Mode: 2})
	if !errors.Is(err, ErrOriginalStateCPUMismatch) {
		t.Fatalf("capture error=%v, want %v", err, ErrOriginalStateCPUMismatch)
	}
	if _, err := os.Stat(manager.fanStatePath()); !os.IsNotExist(err) {
		t.Fatalf("fan state was written for mismatched hardware: %v", err)
	}
}

func TestDefaultFanConfigIsSafeAndDisabled(t *testing.T) {
	cfg := DefaultFanConfig()
	if cfg.Enabled {
		t.Fatal("fan control must be disabled by default")
	}
	if cfg.MinPWMPercent != 40 || cfg.EmergencyTempC != 85 || cfg.PollSeconds != 2 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	// 出厂默认＝系统集成的「平衡」档（8 点 CPU / 8 点 HDD / 4 点 NVMe），
	// 直接和 builtinFanCurves 对齐，改参数时这里不用跟着改数字。
	balanced := builtinFanCurves["balanced"]
	if !fanPointsEqual(cfg.Curve, clampCurvePWM(balanced.CPU, cfg.MinPWMPercent)) {
		t.Fatalf("unexpected default CPU curve: %+v", cfg.Curve)
	}
	if !fanPointsEqual(cfg.HDDCurve, clampCurvePWM(balanced.HDD, cfg.MinPWMPercent)) {
		t.Fatalf("unexpected default HDD curve: %+v", cfg.HDDCurve)
	}
	if !fanPointsEqual(cfg.NVMeCurve, clampCurvePWM(balanced.NVMe, cfg.MinPWMPercent)) {
		t.Fatalf("unexpected default NVMe curve: %+v", cfg.NVMeCurve)
	}
	if len(cfg.Curve) != 8 || len(cfg.HDDCurve) != 8 || len(cfg.NVMeCurve) != 4 {
		t.Fatalf("default curve shape changed: CPU=%d HDD=%d NVMe=%d", len(cfg.Curve), len(cfg.HDDCurve), len(cfg.NVMeCurve))
	}
	if cfg.Curve[0].PWMPercent != 40 || cfg.Curve[len(cfg.Curve)-1].PWMPercent != 100 {
		t.Fatalf("default CPU curve must start at minimum and end at 100%%: %+v", cfg.Curve)
	}
	if len(cfg.HDDSlotIDs) != 6 || len(cfg.NVMeSlotIDs) != 4 {
		t.Fatalf("all storage slots must participate by default: HDD=%v NVMe=%v", cfg.HDDSlotIDs, cfg.NVMeSlotIDs)
	}
}

func TestInterpolatePWMPercent(t *testing.T) {
	cfg := DefaultFanConfig()
	curve := cfg.Curve
	// 出厂默认＝平衡档：20/42/44/60/70/75/80/85 °C → 40/40/47/47/55/65/85/100 %
	tests := []struct {
		temp float64
		want int
	}{
		{30, 40},
		{42, 40},
		{47.5, 47},
		{55, 47},
		{70, 55},
		{75, 65},
		{80, 85},
		{85, 100},
	}
	for _, test := range tests {
		if got := interpolatePWMPercent(curve, test.temp, cfg.MinPWMPercent, cfg.EmergencyTempC); got != test.want {
			t.Errorf("temperature %.1f: got %d%%, want %d%%", test.temp, got, test.want)
		}
	}
}

func TestDiscoverFansRequiresMatchingRPMAndPWMNodes(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon7")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	for name, value := range map[string]string{
		"fan2_input": "0", "pwm2": "26", "pwm2_enable": "2",
		"fan3_input": "1662", "pwm3": "80", "pwm3_enable": "2",
		"fan4_input": "900",
	} {
		writeTestValue(t, filepath.Join(hwmon, name), value)
	}

	fans, err := (&Manager{Root: root}).DiscoverFans()
	if err != nil {
		t.Fatal(err)
	}
	if len(fans) != 2 {
		t.Fatalf("got %d complete fan channels, want 2: %+v", len(fans), fans)
	}
	if !(&Manager{Root: root}).IT87DriverDetected() {
		t.Fatal("IT87 hwmon node was not detected")
	}
	if fans[1].ID != "it8613:hwmon7:fan3" || fans[1].RPM != 1662 || fans[1].Channel != 3 {
		t.Fatalf("unexpected spinning fan: %+v", fans[1])
	}
}

func TestSetFanPWMAndRestore(t *testing.T) {
	dir := t.TempDir()
	pwm := filepath.Join(dir, "pwm3")
	enable := filepath.Join(dir, "pwm3_enable")
	writeTestValue(t, pwm, "80")
	writeTestValue(t, enable, "2")
	fan := FanDevice{ID: "it8613:test:fan3", Channel: 3, PWMPath: pwm, EnablePath: enable}

	if err := setFanPWM(fan, 50); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm); got != 128 {
		t.Fatalf("pwm=%d, want 128", got)
	}
	if got, _ := readInt(enable); got != 1 {
		t.Fatalf("mode=%d, want manual mode 1", got)
	}
	if err := restoreFan(fan, originalFan{ID: fan.ID, PWM: 80, Mode: 2}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm); got != 80 {
		t.Fatalf("restored pwm=%d, want 80", got)
	}
	if got, _ := readInt(enable); got != 2 {
		t.Fatalf("restored mode=%d, want automatic mode 2", got)
	}
}

func TestSetFanPWMLeavesDriverFullSpeedMode(t *testing.T) {
	dir := t.TempDir()
	pwm := filepath.Join(dir, "pwm3")
	enable := filepath.Join(dir, "pwm3_enable")
	writeTestValue(t, pwm, "255")
	writeTestValue(t, enable, "0")
	fan := FanDevice{ID: "it8613:test:fan3", Channel: 3, PWMPath: pwm, EnablePath: enable}

	if err := setFanPWM(fan, 70); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm); got != 179 {
		t.Fatalf("pwm=%d, want 179", got)
	}
	if got, _ := readInt(enable); got != 1 {
		t.Fatalf("mode=%d, want manual mode 1", got)
	}

	writeTestValue(t, pwm, "255")
	writeTestValue(t, enable, "0")
	if err := setFanPWM(fan, 100); err != nil {
		t.Fatalf("already-full fan should be accepted: %v", err)
	}
}

func TestNormalizeConfigMigratesFanDefaults(t *testing.T) {
	cfg := Config{Enabled: true, PL1W: 15, PL2W: 15, ReapplySeconds: 30}
	if !normalizeConfig(&cfg) {
		t.Fatal("legacy config was not migrated")
	}
	if cfg.Fan.Enabled || cfg.Fan.MinPWMPercent != 40 || len(cfg.Fan.Curve) != 8 || len(cfg.Fan.HDDCurve) != 8 || len(cfg.Fan.NVMeCurve) != 4 || len(cfg.Fan.HDDSlotIDs) != 6 || len(cfg.Fan.NVMeSlotIDs) != 4 {
		t.Fatalf("unexpected migrated fan config: %+v", cfg.Fan)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("normalized config was migrated a second time")
	}
}

// 旧出厂默认（最低转速 60% + 三条曲线都从 60% 起）在升级时整体升到新默认；
// 只要用户动过最低转速或任意一条曲线，就一个字都不改。
func TestNormalizeConfigUpgradesUntouchedLegacyFanDefaults(t *testing.T) {
	legacy := func() FanConfig {
		return FanConfig{
			Enabled:        false,
			MinPWMPercent:  60,
			EmergencyTempC: 85,
			PollSeconds:    2,
			Curve: []FanPoint{
				{TempC: 40, PWMPercent: 60}, {TempC: 55, PWMPercent: 70},
				{TempC: 70, PWMPercent: 85}, {TempC: 80, PWMPercent: 100},
			},
			HDDCurve:    legacyDefaultStorageCurve(),
			NVMeCurve:   legacyDefaultStorageCurve(),
			HDDSlotIDs:  storageSlotIDs("front"),
			NVMeSlotIDs: storageSlotIDs("m2"),
		}
	}
	// 先把 history/log 等无关段归一化，之后只看风扇段是否被动过
	settle := func(fan FanConfig) Config {
		cfg := Config{GPIO: DefaultGPIOConfig()}
		normalizeConfig(&cfg)
		cfg.Fan = fan
		return cfg
	}

	cfg := settle(legacy())
	if !normalizeConfig(&cfg) {
		t.Fatal("未改动的旧默认风扇配置没有被升级")
	}
	if cfg.Fan.MinPWMPercent != 40 || cfg.Fan.Curve[0].PWMPercent != 40 || cfg.Fan.HDDCurve[0].PWMPercent != 40 || cfg.Fan.NVMeCurve[0].PWMPercent != 40 {
		t.Fatalf("旧默认没有整体升到新默认: %+v", cfg.Fan)
	}
	// 升级后就是新的出厂默认（平衡档），逐点对齐
	want := DefaultFanConfig()
	if !fanPointsEqual(cfg.Fan.Curve, want.Curve) || !fanPointsEqual(cfg.Fan.HDDCurve, want.HDDCurve) || !fanPointsEqual(cfg.Fan.NVMeCurve, want.NVMeCurve) {
		t.Fatalf("升级改动了曲线形态: %+v", cfg.Fan)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("升级后二次归一化不该再改")
	}

	// 只改过最低转速 → 曲线原样保留（不能被升级覆盖）
	modified := legacy()
	modified.MinPWMPercent = 55
	cfg = settle(modified)
	if normalizeConfig(&cfg) {
		t.Fatal("改过最低转速的配置不该被升级")
	}
	if cfg.Fan.MinPWMPercent != 55 || cfg.Fan.Curve[0].PWMPercent != 60 {
		t.Fatalf("用户配置被改动: %+v", cfg.Fan)
	}

	// 只改过一条曲线 → 最低转速也保持原样
	modified = legacy()
	modified.NVMeCurve = []FanPoint{{TempC: 30, PWMPercent: 60}, {TempC: 70, PWMPercent: 100}}
	cfg = settle(modified)
	if normalizeConfig(&cfg) {
		t.Fatal("改过曲线的配置不该被升级")
	}
	if cfg.Fan.MinPWMPercent != 60 || cfg.Fan.Curve[0].PWMPercent != 60 {
		t.Fatalf("用户配置被改动: %+v", cfg.Fan)
	}
}

func TestNormalizeConfigMigratesDiskCurveWithoutReplacingCPUCurve(t *testing.T) {
	cfg := Config{Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig()}
	cfg.Fan.MinPWMPercent = 30
	cfg.Fan.Curve = []FanPoint{{TempC: 40, PWMPercent: 30}, {TempC: 70, PWMPercent: 90}}
	cfg.Fan.DiskCurve = []FanPoint{{TempC: 28, PWMPercent: 30}, {TempC: 48, PWMPercent: 100}}
	cfg.Fan.HDDCurve = nil
	cfg.Fan.NVMeCurve = nil
	if !normalizeConfig(&cfg) {
		t.Fatal("v0.5 config was not migrated")
	}
	if len(cfg.Fan.Curve) != 2 || cfg.Fan.Curve[0].PWMPercent != 30 {
		t.Fatalf("CPU curve was replaced during migration: %+v", cfg.Fan.Curve)
	}
	if len(cfg.Fan.HDDCurve) != 2 || cfg.Fan.HDDCurve[0].TempC != 28 || cfg.Fan.HDDCurve[1].PWMPercent != 100 {
		t.Fatalf("legacy disk curve was not copied to HDD: %+v", cfg.Fan.HDDCurve)
	}
	if !fanPointsEqual(cfg.Fan.NVMeCurve, defaultNVMeFanCurve(30)) {
		t.Fatalf("NVMe curve did not fall back to the balanced default: %+v", cfg.Fan.NVMeCurve)
	}
	if cfg.Fan.DiskCurve != nil {
		t.Fatalf("legacy disk curve was not cleared: %+v", cfg.Fan.DiskCurve)
	}
	if normalizeConfig(&cfg) {
		t.Fatal("migrated config changed a second time")
	}
}

func TestNormalizeConfigPreservesExplicitlyDisabledStorageMonitoring(t *testing.T) {
	cfg := Config{Fan: DefaultFanConfig(), GPIO: DefaultGPIOConfig(), Log: DefaultLogConfig()}
	// 已经归一化过的配置：传感器默认显示名早已补过，这里不该再被改
	cfg.SensorNameDefaultsApplied = true
	cfg.Fan.HDDSlotIDs = []string{}
	cfg.Fan.NVMeSlotIDs = []string{}
	cfg.History = HistoryConfig{Enabled: false, MaxSizeMB: 32, RetentionDays: historyDefaultRetentionDays}
	if normalizeConfig(&cfg) {
		t.Fatal("normalized config unexpectedly changed")
	}
	if len(cfg.Fan.HDDSlotIDs) != 0 || len(cfg.Fan.NVMeSlotIDs) != 0 {
		t.Fatalf("disabled storage monitoring was re-enabled: HDD=%v NVMe=%v", cfg.Fan.HDDSlotIDs, cfg.Fan.NVMeSlotIDs)
	}
	if cfg.History != (HistoryConfig{Enabled: false, MaxSizeMB: 32, RetentionDays: historyDefaultRetentionDays}) {
		t.Fatalf("explicit history settings were overwritten: %+v", cfg.History)
	}
}

func TestFanTargetsUseHigherCurve(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.MinPWMPercent = 30
	cfg.Curve = []FanPoint{{TempC: 40, PWMPercent: 30}, {TempC: 80, PWMPercent: 100}}
	cfg.HDDCurve = defaultStorageFanCurve(cfg.MinPWMPercent)
	cfg.NVMeCurve = []FanPoint{{TempC: 30, PWMPercent: 30}, {TempC: 70, PWMPercent: 90}}

	cpu, hdd, nvme, target := fanTargets(cfg, 60, 25, true, 30, true)
	if cpu != 65 || hdd != 40 || nvme != 30 || target != 65 {
		t.Fatalf("CPU should control cool storage: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	cpu, hdd, nvme, target = fanTargets(cfg, 45, 35, true, 30, true)
	if cpu != 39 || hdd != 47 || nvme != 30 || target != 47 {
		t.Fatalf("HDD should control: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	cpu, hdd, nvme, target = fanTargets(cfg, 45, 25, true, 70, true)
	if cpu != 39 || hdd != 40 || nvme != 90 || target != 90 {
		t.Fatalf("NVMe should control: cpu=%d hdd=%d nvme=%d target=%d", cpu, hdd, nvme, target)
	}
	_, hdd, nvme, target = fanTargets(cfg, 45, 0, false, 0, false)
	if hdd != 0 || nvme != 0 || target != 39 {
		t.Fatalf("missing storage temperature should not affect target: hdd=%d nvme=%d target=%d", hdd, nvme, target)
	}
}

func TestMaximumStorageTemperatureByKind(t *testing.T) {
	storage := StorageStatus{Slots: []StorageSlot{
		{ID: "front-1", TemperatureC: 43},
		{ID: "front-2", Kind: "front", TemperatureC: 44},
		{ID: "m2-1", Kind: "m2", TemperatureC: 58},
		{ID: "m2-2", Kind: "m2", TemperatureC: 64},
	}}
	storage.Slots[0].Kind = "front"
	if temperature, available := maximumStorageTemperature(storage, "front", []string{"front-1", "front-2"}); !available || temperature != 44 {
		t.Fatalf("HDD temperature=%.1f available=%v, want 44 true", temperature, available)
	}
	if temperature, available := maximumStorageTemperature(storage, "m2", []string{"m2-1", "m2-2"}); !available || temperature != 64 {
		t.Fatalf("NVMe temperature=%.1f available=%v, want 64 true", temperature, available)
	}
	if temperature, available := maximumStorageTemperature(storage, "m2", []string{"m2-1"}); !available || temperature != 58 {
		t.Fatalf("filtered NVMe temperature=%.1f available=%v, want 58 true", temperature, available)
	}
	if temperature, available := maximumStorageTemperature(storage, "m2", []string{}); available || temperature != 0 {
		t.Fatalf("disabled NVMe slots returned temperature=%.1f available=%v", temperature, available)
	}
}

func TestFanControlSourceReportsAllTiedSources(t *testing.T) {
	if source := fanControlSource(85, 85, 70); source != "cpu+hdd" {
		t.Fatalf("source=%q, want cpu+hdd", source)
	}
	if source := fanControlSource(100, 100, 100); source != "cpu+hdd+nvme" {
		t.Fatalf("source=%q, want cpu+hdd+nvme", source)
	}
}

func TestFanValidationRejectsDecreasingSpeed(t *testing.T) {
	cfg := DefaultFanConfig()
	// 默认 CPU 曲线第 3、4 点都是 47%：把第 4 点压到 40% 就构成递减
	cfg.Curve[3].PWMPercent = 40
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("decreasing PWM curve was accepted")
	}
}

func TestFanValidationRejectsDecreasingStorageSpeed(t *testing.T) {
	cfg := DefaultFanConfig()
	// 默认 NVMe 曲线是 40/40/60/70：末点压到 50 就构成递减
	cfg.NVMeCurve[3].PWMPercent = 50
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("decreasing NVMe PWM curve was accepted")
	}
}

func TestFanValidationRejectsInvalidOrDuplicateStorageSlots(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.HDDSlotIDs = []string{"front-1", "front-1"}
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("duplicate HDD slot was accepted")
	}
	cfg = DefaultFanConfig()
	cfg.NVMeSlotIDs = []string{"front-1"}
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("HDD slot was accepted as NVMe slot")
	}
	cfg = DefaultFanConfig()
	cfg.HDDSlotIDs = []string{}
	cfg.NVMeSlotIDs = []string{}
	if err := (&Manager{}).validateFanLocked(&cfg); err != nil {
		t.Fatalf("explicitly disabling storage monitoring was rejected: %v", err)
	}
}

func writeTestValue(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---- 风扇满转基准自动标定 ----

// 全速读数稳态后按中位数更新满转基准并落盘;非全速/坏读数/未稳态不标定;
// 重启(新 Manager 惰性载入配置)后基准保留;换算与单位上限按新基准走。
func TestFanRPMCalibrationLearnsFullSpeed(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8792:it8792:fan2"
	// 非全速读数不标定
	m.processFanRPMSample(id, 4500, 200)
	if base := m.fanDebugRPMBaseLocked(id); base != fanRPMBaseDefault {
		t.Fatalf("sub-full-speed sample must not calibrate, base=%d", base)
	}
	// 稳态全速窗口(极差 ≤5%):中位数 4500
	for _, rpm := range []int{4480, 4520, 4500, 4490, 4510} {
		m.processFanRPMSample(id, rpm, 255)
	}
	if base := m.fanDebugRPMBaseLocked(id); base != 4500 {
		t.Fatalf("steady full-speed window should calibrate to the median, base=%d", base)
	}
	// 已落盘
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.FanRPMBase[id] != 4500 {
		t.Fatalf("calibration must persist to config, got %+v", persisted.FanRPMBase)
	}
	// 窗口混入毛刺(极差超限):不标定,基准不被单点读数带偏
	m.processFanRPMSample(id, 9000, 255)
	m.processFanRPMSample(id, 4495, 255)
	if base := m.fanDebugRPMBaseLocked(id); base != 4500 {
		t.Fatalf("non-steady window must not recalibrate, base=%d", base)
	}
	// 重启语义:新 Manager 从配置惰性载入基准
	restarted := &Manager{ConfigPath: configPath}
	if base := restarted.fanDebugRPMBaseLocked(id); base != 4500 {
		t.Fatalf("restart should load the calibrated base, base=%d", base)
	}
	// 换算与上限按基准:rpm 2250 → 50% → PWM 128;上限钳到 4500
	if got := fanDebugRaw("rpm", 2250, restarted.fanDebugRPMBaseLocked(id)); got != percentToPWM(50) {
		t.Fatalf("rpm conversion must use the calibrated base, got %d", got)
	}
	if got := restarted.fanDebugUnitMaxFor(id, "rpm"); got != 4500 {
		t.Fatalf("rpm unit max must follow the calibrated base, got %d", got)
	}
	// 名义基准(未标定风扇)与旧公式等价:2000 RPM → 100% PWM
	if got := fanDebugRaw("rpm", 2000, fanRPMBaseDefault); got != 255 {
		t.Fatalf("default base must keep 2000 RPM=100%%, got %d", got)
	}
}

// ---- RPM 特性表学习与闭环微调 ----

// 中低档位稳态读数入特性表并落盘;重启(新 Manager)后表保留;
// 设定目标转速时初值查表插值,表外区间用满转基准外推,空表退化为线性。
func TestFanRPMMapLearnsAndInterpolates(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{ConfigPath: configPath}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8792:it8792:fan2"
	// 两档稳态:PWM 80 → 1500,PWM 160 → 2500(每档 3 个读数)
	for _, rpm := range []int{1490, 1510, 1500} {
		m.processFanRPMSample(id, rpm, 80)
	}
	m.fanRPMLastSave = time.Time{} // 第二档落盘不受首写节流限制
	for _, rpm := range []int{2490, 2510, 2500} {
		m.processFanRPMSample(id, rpm, 160)
	}
	// 升速中(极差超限)不入表
	m.processFanRPMSample(id, 1800, 120)
	m.processFanRPMSample(id, 2200, 120)
	m.processFanRPMSample(id, 1650, 120)
	// 落盘
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	table := persisted.FanRPMMap[id]
	if table == nil || table[80] != 1500 || table[160] != 2500 {
		t.Fatalf("steady slots must be learned, got %+v", persisted.FanRPMMap)
	}
	if _, learned := table[112]; learned {
		t.Fatal("non-steady slot must not be learned")
	}
	// 重启语义:新 Manager 载入表后插值
	restarted := &Manager{ConfigPath: configPath}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 目标 2000:80+ (2000-1500)*(160-80)/(2500-1500) = 120
	if got := restarted.rpmToPWMEstimateLocked(id, 2000); got != 120 {
		t.Fatalf("interpolation: got %d, want 120", got)
	}
	// 表内低段:目标 1500 → 80;低于最小转速也停在 80(最低已知档)
	if got := restarted.rpmToPWMEstimateLocked(id, 1500); got != 80 {
		t.Fatalf("clamp to lowest known slot: got %d, want 80", got)
	}
	// 表外高转速:基准线性外推(2000 默认基准,目标 4000 → 255)
	if got := restarted.rpmToPWMEstimateLocked(id, 4000); got != 255 {
		t.Fatalf("extrapolation above table: got %d, want 255", got)
	}
	// 空表风扇:退化为基准线性(等价旧公式)
	if got := restarted.rpmToPWMEstimateLocked("other:fan9", 1000); got != percentToPWM(50) {
		t.Fatalf("empty table must fall back to linear base, got %d", got)
	}
}

// 闭环单步:超出容差按差值微调 PWM(限幅 ±20),达标静默并标记 locked,
// 连续不可达 15 次暂停,percent 单位不参与,无活跃目标时报告退出。
func TestFanRPMCloseLoopStep(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon7")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	for name, value := range map[string]string{
		"fan2_input": "2000", "pwm2": "128", "pwm2_enable": "1",
		"fan3_input": "1800", "pwm3": "100", "pwm3_enable": "1",
	} {
		writeTestValue(t, filepath.Join(hwmon, name), value)
	}
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning)
	const idA, idB = "it8613:hwmon7:fan2", "it8613:hwmon7:fan3"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{idA: 3000, idB: 50}
	m.fanDebugUnits = map[string]string{idA: "rpm", idB: "percent"}
	m.mu.Unlock()

	fan2Input := filepath.Join(hwmon, "fan2_input")
	pwm2 := filepath.Join(hwmon, "pwm2")
	// 第一步:实测 2000 vs 目标 3000,容差 max(90,60)=90 → adj=(1000)*255/2000=127→钳20 → 148
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("rpm target should keep the loop active")
	}
	if got, _ := readInt(pwm2); got != 148 {
		t.Fatalf("first step should write pwm=148, got %d", got)
	}
	if m.fanRPMLoopLocked[idA] {
		t.Fatal("far from target must not be locked")
	}
	// percent 风扇不参与
	if got, _ := readInt(filepath.Join(hwmon, "pwm3")); got != 100 {
		t.Fatalf("percent unit must not be touched, pwm3=%d", got)
	}
	// 连续不可达:推到 15 次上限后暂停写入（写入后隔轮观察使调整轮减半,
	// 循环按 miss 达标退出,带上限防死循环）
	writeTestValue(t, fan2Input, "2000")
	for i := 0; m.fanRPMLoopMiss[idA] < fanRPMCloseLoopMaxMiss && i < fanRPMCloseLoopMaxMiss*4; i++ {
		m.stepFanRPMCloseLoop()
	}
	missBefore := m.fanRPMLoopMiss[idA]
	if missBefore != fanRPMCloseLoopMaxMiss {
		t.Fatalf("miss should cap at %d, got %d", fanRPMCloseLoopMaxMiss, missBefore)
	}
	writeTestValue(t, pwm2, "148") // 复位,暂停后不得再写
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("paused fan is still an active target")
	}
	if got, _ := readInt(pwm2); got != 148 {
		t.Fatalf("paused fan must not be adjusted, pwm=%d", got)
	}
	// 转速进入容差(3000±90):达标,locked,miss 清零
	writeTestValue(t, fan2Input, "3010")
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("still active after locking")
	}
	if !m.fanRPMLoopLocked[idA] || m.fanRPMLoopMiss[idA] != 0 {
		t.Fatalf("in-tolerance reading should lock, locked=%v miss=%d", m.fanRPMLoopLocked[idA], m.fanRPMLoopMiss[idA])
	}
	// 取消接管:无活跃目标,报告退出
	m.mu.Lock()
	delete(m.fanDebugTakenOver, idA)
	delete(m.fanDebugTakenOver, idB)
	m.mu.Unlock()
	if m.stepFanRPMCloseLoop() {
		t.Fatal("no rpm targets should deactivate the loop")
	}
}

// ---- 主动标定满转基准 ----

// 标定按钮端到端:接管前未接管的风扇 → 全速写 255 → 恒定稳态读数入特性表
// 240 档 + 更新满转基准并落盘 → 恢复未接管状态(交还曲线,PWM 回到标定前值)。
// 闭环挂起:标定进行中该风扇不被闭环微调。
func TestCalibrateFanRPMFullSpeed(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon9")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "4490")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "128")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, ConfigPath: configPath, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning) // DiscoverFans 的学习 goroutine 会往 TempDir 写盘,须先于目录清理退出
	const id = "it8613:hwmon9:fan2"
	pwm2 := filepath.Join(hwmon, "pwm2")

	base, err := m.CalibrateFanRPM(id)
	if err != nil {
		t.Fatal(err)
	}
	if base != 4490 {
		t.Fatalf("steady reading 4490 should become the base, got %d", base)
	}
	// 特性表 240 档入表,基准落盘
	m.mu.Lock()
	table := m.fanRPMLearned[id]
	slot240 := table[240]
	m.mu.Unlock()
	if slot240 != 4490 {
		t.Fatalf("calibration must record the top slot, got %d", slot240)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "4490") {
		t.Fatalf("calibration must persist to config: %s", raw)
	}
	// 恢复:未接管,交还曲线,PWM 回到标定前值
	if _, taken := m.fanDebugTakenOver[id]; taken {
		t.Fatal("fan was not taken over before calibration, must be released")
	}
	if m.fanRPMSuspend[id] {
		t.Fatal("suspend flag must be cleared after calibration")
	}
	if got, _ := readInt(pwm2); got != 128 {
		t.Fatalf("original PWM must be restored, got %d", got)
	}
	// 全速确实被写入过(pwm2 在标定中被写成 255,上面已验证恢复)——
	// 直接断言闭环挂起语义:挂起中的风扇不参与闭环,唯一目标被挂起时闭环退出
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 3000}
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.fanRPMSuspend[id] = true
	m.mu.Unlock()
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	if m.stepFanRPMCloseLoop() {
		t.Fatal("suspended-only fan should deactivate the loop")
	}
	if got, _ := readInt(pwm2); got != 128 {
		t.Fatalf("suspended fan must not be adjusted by the close loop, pwm=%d", got)
	}
}

// ---- 回归:取消接管立即恢复转速;递增间隔不设上限 ----

// 释放接管必须立即把 PWM 写回接管时的存档值:被调试风扇不在曲线列表时
// 没有任何后台路径会再碰它,不写回就永久保持调试转速(用户实测)。
func TestFanDebugTakeoverReleaseRestoresPWM(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "100")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "2") // 接管前是 BIOS 自动温控
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: root, ConfigPath: configPath, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning) // DiscoverFans 的学习 goroutine 会往 TempDir 写盘,须先于目录清理退出
	const id = "it8613:hwmon8:fan2"
	pwm2 := filepath.Join(hwmon, "pwm2")
	pwm2Enable := filepath.Join(hwmon, "pwm2_enable")

	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugValue(id, 0, "percent"); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm2); got != 0 {
		t.Fatalf("debug value should drive the fan to pwm 0, got %d", got)
	}
	// 切到 rpm 单位再释放：单位与闭环状态必须清干净——残留的 rpm 会让下次
	// 接管把百分比当转速解释（闭环降速到近停转）
	if err := m.SetFanDebugValue(id, 1000, "rpm"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugTakeover(id, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm2); got != 100 {
		t.Fatalf("release must restore the captured PWM immediately, got %d", got)
	}
	if got, _ := readInt(pwm2Enable); got != 2 {
		t.Fatalf("release must restore the original pwm_enable mode (BIOS auto), got %d", got)
	}
	if _, taken := m.fanDebugTakenOver[id]; taken {
		t.Fatal("release must clear the takeover state")
	}
	if _, unitLeft := m.fanDebugUnits[id]; unitLeft {
		t.Fatalf("release must clear the unit, got %q", m.fanDebugUnits[id])
	}
	if _, missLeft := m.fanRPMLoopMiss[id]; missLeft {
		t.Fatal("release must clear the close-loop miss counter")
	}
	// 重新接管：单位必须重置为 percent（接管值的语义就是百分比）
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	if m.fanDebugUnits[id] != "percent" {
		t.Fatalf("takeover must reset the unit to percent, got %q", m.fanDebugUnits[id])
	}
	if err := m.SetFanDebugTakeover(id, false); err != nil {
		t.Fatal(err)
	}
}

// 紧急满速兜底期间 RPM 闭环必须让位：不得把 100% 拉回目标转速（闭环保持
// 存活，兜底解除后自动恢复监控）。
func TestFanRPMLoopYieldsToEmergency(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000") // 实测远超目标,闭环本想降 PWM
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "255")        // 兜底刚写完满速
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8613:hwmon8:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 1000}
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.fanDebugEmergency = true
	m.mu.Unlock()
	pwm2 := filepath.Join(hwmon, "pwm2")
	if alive := m.stepFanRPMCloseLoop(); !alive {
		t.Fatal("close loop must stay alive during emergency (targets still active)")
	}
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("emergency full-speed must not be pulled back by the close loop, got %d", got)
	}
}

// 兜底解除（CPU 温度回落）时把被接管风扇按各自调试值写回——percent/pwm
// 单位的风扇在兜底期间停在 100%，不写回会与调试显示脱节。
func TestFanDebugEmergencyReleaseRestoresDebugValue(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	cpu := filepath.Join(root, "sys", "class", "hwmon", "hwmon9")
	if err := os.MkdirAll(cpu, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(cpu, "name"), "coretemp")
	writeTestValue(t, filepath.Join(cpu, "temp1_input"), "50000") // 50°C < 85 阈值 → 解除兜底
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 50} // percent
	m.fanDebugUnits = map[string]string{id: "percent"}
	m.fanDebugEmergency = true
	m.mu.Unlock()
	pwm2 := filepath.Join(hwmon, "pwm2")
	writeTestValue(t, pwm2, "255") // 兜底满速的最后写入
	if err := m.applyFanDebugEmergencyLocked(FanConfig{EmergencyTempC: 85}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readInt(pwm2); got != int64(percentToPWM(50)) {
		t.Fatalf("emergency release must rewrite the debug value, want %d got %d", percentToPWM(50), got)
	}
	if m.fanDebugEmergency {
		t.Fatal("emergency flag must clear when temperature drops below the threshold")
	}
}

// RPM 闭环防震荡三件套的行为锁定：写入后隔轮观察（风扇转速惯性 2–5 秒
// 大于 tick 2 秒,基于过渡态连环调整是正负来回震荡的根源）、近端小步
// 细逼近、反向步长减半（过调回摆逐次收敛）。
func TestFanRPMLoopAntiOscillation(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon7")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "128")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	t.Cleanup(m.stopFanRPMLearning)
	const id = "it8613:hwmon7:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 1600}
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.mu.Unlock()
	fan2Input := filepath.Join(hwmon, "fan2_input")
	pwm2 := filepath.Join(hwmon, "pwm2")

	// 远端第一步：目标 1600 实测 2000（容差 max(48,60)=60）→ adjust=-51 钳
	// -20 → 108,写入并进入隔轮观察
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 108 {
		t.Fatalf("first step should write pwm=108, got %d", got)
	}
	if !m.fanRPMLoopSettling[id] {
		t.Fatal("adjustment must arm the settling round")
	}
	// 第二步（转速还没跟上,读数仍是 2000）：观察轮,不得再写
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 108 {
		t.Fatalf("settling round must not adjust, pwm=%d", got)
	}
	if m.fanRPMLoopSettling[id] {
		t.Fatal("settling flag must clear after the observation round")
	}
	// 第三步（转速部分回落 1700,误差 100 < 2×容差 120）：近端小步,仅 -2
	writeTestValue(t, fan2Input, "1700")
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 106 {
		t.Fatalf("near-target step must be capped at -2 (pwm 106), got %d", got)
	}
	// 第四步（转速进容差 1550）：达标锁定,miss 清零
	writeTestValue(t, fan2Input, "1550")
	m.stepFanRPMCloseLoop()
	if !m.fanRPMLoopLocked[id] || m.fanRPMLoopMiss[id] != 0 {
		t.Fatalf("within tolerance must lock and reset miss, locked=%v miss=%d", m.fanRPMLoopLocked[id], m.fanRPMLoopMiss[id])
	}

	// 反向减半：上一轮方向为 -1,现在读数骤降到 1000（大幅欠速）→ adjust
	// +76 钳 +20,与上轮反向 → 减半为 +10
	writeTestValue(t, fan2Input, "1000")
	writeTestValue(t, pwm2, "106")
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 116 {
		t.Fatalf("reverse direction must halve the step (+10 → 116), got %d", got)
	}
	if m.fanRPMLoopLastDir[id] != 1 {
		t.Fatalf("direction bookkeeping should record +1, got %d", m.fanRPMLoopLastDir[id])
	}
}

// 递增间隔不设上限(旧实现限 1–120):>120 接受,0 报错,荒谬大值钳到
// 溢出护栏(百年)防 Duration 回绕。
func TestFanDebugAutoIntervalUnbounded(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "100")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	// 3600 秒(1 小时)合法
	if err := m.SetFanDebugAuto(id, true, 5, 3600, "percent"); err != nil {
		t.Fatalf("interval beyond the old 120s cap must be accepted: %v", err)
	}
	m.mu.Lock()
	got := m.fanDebugAuto.entries[id].Interval
	m.mu.Unlock()
	if got != 3600 {
		t.Fatalf("interval should be stored verbatim, got %d", got)
	}
	// 0 报错
	if err := m.SetFanDebugAuto(id, true, 5, 0, "percent"); err == nil {
		t.Fatal("interval 0 must be rejected")
	}
	// 荒谬大值钳到溢出护栏
	if err := m.SetFanDebugAuto(id, true, 5, 9999999999, "percent"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	got = m.fanDebugAuto.entries[id].Interval
	m.mu.Unlock()
	if got != fanAutoIntervalMax {
		t.Fatalf("absurd interval must clamp to the overflow guard, got %d", got)
	}
}

// 自动测试运行中锁定参数:调试值与主动标定都拒绝(前端禁用控件之外的
// 后端兜底——直连 API 的改动会破坏递增进程)。
func TestFanDebugAutoRunningLocksParams(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "100")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugAuto(id, true, 5, 10, "percent"); err != nil {
		t.Fatal(err)
	}
	// 标定必然破坏递增(写 255 全速):运行中拒绝
	if _, err := m.CalibrateFanRPM(id); err == nil || !strings.Contains(err.Error(), "自动测试进行中") {
		t.Fatalf("calibration must be rejected while auto test runs, got %v", err)
	}
	// 调试值接口运行中不拒:单位切换的等比换算是无损操作(见
	// TestFanDebugUnitSwitchMidRamp),锁定由前端禁用控件承担
	if err := m.SetFanDebugValue(id, 50, "percent"); err != nil {
		t.Fatalf("unit-resync path must stay open mid-ramp, got %v", err)
	}
}

// ---- 回归:闭环顶格冻结(目标超出风扇能力时不再回撤试探) ----

// 目标转速高于实际可达:PWM 顶到 255 后闭环必须冻结(保持全速、miss 拉满),
// 不得因读数噪声"降档再试"形成 98↔90 振荡;读数偶发进容差可解冻;
// 真超速(读数>目标)仍允许下调。
func TestFanRPMCloseLoopStopsAtFullScale(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2600")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "255")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	m.mu.Lock()
	m.fanDebugTakenOver = map[string]int{id: 3000} // 目标 3000,实际全速 ~2600,容差 90
	m.fanDebugUnits = map[string]string{id: "rpm"}
	m.mu.Unlock()
	pwm2 := filepath.Join(hwmon, "pwm2")
	fan2 := filepath.Join(hwmon, "fan2_input")

	// 顶格欠速:一步进入冻结,PWM 保持 255
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("frozen fan is still an active target")
	}
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("full-scale PWM must be held, got %d", got)
	}
	m.mu.Lock()
	miss, locked := m.fanRPMLoopMiss[id], m.fanRPMLoopLocked[id]
	m.mu.Unlock()
	if miss != fanRPMCloseLoopMaxMiss || !locked {
		t.Fatalf("full-scale undershoot must freeze immediately, miss=%d locked=%v", miss, locked)
	}
	// 冻结中读数继续欠速:不写不动
	writeTestValue(t, fan2, "2580")
	m.stepFanRPMCloseLoop()
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("frozen loop must not touch PWM, got %d", got)
	}
	// 读数偶发进容差:解冻(miss 清零)
	writeTestValue(t, fan2, "3010")
	m.stepFanRPMCloseLoop()
	m.mu.Lock()
	miss = m.fanRPMLoopMiss[id]
	m.mu.Unlock()
	if miss != 0 {
		t.Fatalf("in-tolerance reading should unfreeze, miss=%d", miss)
	}
	// 真超速(读数 > 目标+容差):允许下调,不冻结
	writeTestValue(t, fan2, "3400")
	if !m.stepFanRPMCloseLoop() {
		t.Fatal("overshoot should keep the loop active for correction")
	}
	if got, _ := readInt(pwm2); got != 235 {
		t.Fatalf("overshoot must step PWM down by max adjustment, got %d", got)
	}
}

// 自动递增到单位上限:Done 且 PWM 顶格(既有行为,防回归)。
func TestFanDebugAutoRampDoneAtFullScale(t *testing.T) {
	root := t.TempDir()
	hwmon := filepath.Join(root, "sys", "class", "hwmon", "hwmon8")
	if err := os.MkdirAll(hwmon, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, filepath.Join(hwmon, "name"), "it8613")
	writeTestValue(t, filepath.Join(hwmon, "fan2_input"), "2000")
	writeTestValue(t, filepath.Join(hwmon, "pwm2"), "90")
	writeTestValue(t, filepath.Join(hwmon, "pwm2_enable"), "1")
	m := &Manager{Root: root, StatePath: filepath.Join(root, "state.json")}
	const id = "it8613:hwmon8:fan2"
	if err := m.SetFanDebugTakeover(id, true); err != nil {
		t.Fatal(err)
	}
	// 从 95 起步、步进 10、间隔 1 秒:一波即到 100 上限
	if err := m.SetFanDebugValue(id, 95, "percent"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugAuto(id, true, 10, 1, "percent"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var entry fanDebugAutoEntry
	for {
		time.Sleep(100 * time.Millisecond)
		m.mu.Lock()
		entry = m.fanDebugAuto.entries[id]
		m.mu.Unlock()
		if entry.Done || time.Now().After(deadline) {
			break
		}
	}
	pwm2 := filepath.Join(hwmon, "pwm2")
	if got, _ := readInt(pwm2); got != 255 {
		t.Fatalf("ramp to 100%% should write full-scale PWM, got %d", got)
	}
	if !entry.Done || entry.Running {
		t.Fatalf("ramp at unit max should be done, got %+v", entry)
	}
}

// 卸载 / 重装：release-fans 必须把风扇写回接管前的 BIOS 自动模式，
// 同时把配置里的风扇控制关掉——否则重装后服务一启动又会抢回控制权。
func TestReleaseFansHandsControlBackToBIOS(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	enable1 := filepath.Join(hwmon, "pwm1_enable")

	// 接管前存档：BIOS 自动模式(2) + PWM 102
	state := originalFanState{
		Version:    fanStateVersion,
		CapturedAt: time.Now(),
		Fans:       []originalFan{{ID: "it8613:hwmon3:fan1", PWM: 102, Mode: 2}},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.fanStatePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// 当前是模块接管中：手动模式 + 自定义 PWM
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatal(err)
	}
	writeTestValue(t, enable1, "1")
	writeTestValue(t, pwm1, "200")

	if err := manager.ReleaseFans(); err != nil {
		t.Fatalf("release fans: %v", err)
	}

	// 硬件：pwm_enable 回到 BIOS 自动档，PWM 回到接管前的值
	if got, _ := readInt(enable1); got != 2 {
		t.Fatalf("pwm1_enable=%d, want 2 (BIOS auto)", got)
	}
	if got, _ := readInt(pwm1); got != 102 {
		t.Fatalf("pwm1=%d, want 102 (captured original)", got)
	}

	// 配置：风扇控制落盘为关闭，重装后不会自动接管
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Fan.Enabled {
		t.Fatal("fan control must be disabled on disk after release")
	}
	if len(saved.Fan.CPUFanIDs) != 1 || saved.Fan.CPUFanIDs[0] != "it8613:hwmon3:fan1" {
		t.Fatalf("release must not throw away fan bindings: %+v", saved.Fan.CPUFanIDs)
	}
}

// 没有接管过（没有原始状态存档）时 release 也要安全：配置关掉、不报错。
func TestReleaseFansWithoutCapturedState(t *testing.T) {
	manager := newFanDebugTestManager(t)
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseFans(); err != nil {
		t.Fatalf("release without captured state should be a no-op, got %v", err)
	}
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Fan.Enabled {
		t.Fatal("fan control must be disabled even without a captured state")
	}
}

// 全新安装：三条曲线都要绑到所有有转速反馈的风扇、打开风扇控制、用平衡档。
func TestBindAllFansDefaultEnablesEveryCurve(t *testing.T) {
	manager := newFanDebugTestManager(t)
	if err := manager.BindAllFansDefault(); err != nil {
		t.Fatalf("bind all fans: %v", err)
	}
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Fan.Enabled {
		t.Fatal("fresh install should enable fan control by default")
	}
	// fixture 里 fan1 有 1200 RPM、fan2 是 0 RPM（未接），只绑有反馈的那个
	for name, ids := range map[string][]string{"cpu": saved.Fan.CPUFanIDs, "hdd": saved.Fan.HDDFanIDs, "nvme": saved.Fan.NVMeFanIDs} {
		if len(ids) != 1 || ids[0] != "it8613:hwmon3:fan1" {
			t.Fatalf("%s fan ids = %v, want [it8613:hwmon3:fan1]", name, ids)
		}
	}
	if len(saved.Fan.HDDSlotIDs) != 6 || len(saved.Fan.NVMeSlotIDs) != 4 {
		t.Fatalf("all storage slots must be bound: HDD=%v NVMe=%v", saved.Fan.HDDSlotIDs, saved.Fan.NVMeSlotIDs)
	}
	balanced := builtinFanCurves["balanced"]
	if !fanPointsEqual(saved.Fan.Curve, clampCurvePWM(balanced.CPU, saved.Fan.MinPWMPercent)) {
		t.Fatalf("fresh install must use the balanced CPU curve: %+v", saved.Fan.Curve)
	}
}

// 关停档：曲线允许整条 0%（最低转速 40% 让位），下发时确实写 0。
func TestOffModeAllowsZeroPWMAndWritesZero(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("关停档的 0%% 曲线必须被接受: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档应写 0，实际 pwm1=%d", got)
	}

	// 落盘的 off_mode 要在，重启后仍按关停跑
	raw, err := os.ReadFile(manager.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Fan.OffMode {
		t.Fatal("off_mode 必须落盘，否则重启就退出关停档")
	}
}

// 不开关停档时，同样一条 0% 曲线仍要被最低转速拦下。
func TestZeroPWMWithoutOffModeRejected(t *testing.T) {
	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	if err := (&Manager{}).validateFanLocked(&cfg); err == nil {
		t.Fatal("非关停档的 0% 曲线必须被拒绝")
	}
}

// 关停优先级高于最低转速，但压不过紧急温度：CPU 到 85°C 仍要满速兜底。
func TestOffModeStillFallsBackToFullSpeedOnEmergency(t *testing.T) {
	manager := newFanDebugTestManager(t)
	coretemp := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon9")
	writeTestValue(t, filepath.Join(coretemp, "temp1_input"), "90000")
	pwm1 := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3", "pwm1")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, _ := readInt(pwm1); got != 255 {
		t.Fatalf("超过紧急温度必须满速，实际 pwm1=%d", got)
	}
}

// 关停档真的把风扇停住之后，转速反馈变 0 是预期结果：不能再被「无有效转速」判死。
// 否则周期性应用会走 fail-safe 把风扇拉满，表现就是「关停之后隔一会儿又起转」。
func TestOffModeKeepsWorkingWhenFanReportsZeroRPM(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "0") // 停转后的真实反馈

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.OffMode = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range cfg.Curve {
		cfg.Curve[i].PWMPercent = 0
	}
	for i := range cfg.HDDCurve {
		cfg.HDDCurve[i].PWMPercent = 0
	}
	for i := range cfg.NVMeCurve {
		cfg.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(cfg); err != nil {
		t.Fatalf("关停档不该因为 0 转被判死: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("关停档应写 0，实际 pwm1=%d", got)
	}
	// 周期性应用同样不能失败：失败会触发 fail-safe 把风扇拉满
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("周期性应用不能失败（会 fail-safe 拉满）: %v", err)
	}
	if got, _ := readInt(pwm1); got != 0 {
		t.Fatalf("周期性应用之后仍应保持 0，实际 pwm1=%d", got)
	}

	status := manager.fanStatusLocked(cfg)
	if !status.OffMode {
		t.Fatal("状态里要带 off_mode，前端才能把 0 转说成关停而不是接线故障")
	}
	if status.TargetPWMPercent != 0 {
		t.Fatalf("关停档的目标转速应是 0%%，实际 %d%%", status.TargetPWMPercent)
	}
	if status.CPUTargetPWMPercent != 0 {
		t.Fatalf("关停档的 CPU 目标应是 0%%，实际 %d%%", status.CPUTargetPWMPercent)
	}
	if status.ControlSource != "" {
		t.Fatalf("关停档不该报控制来源，实际 %q", status.ControlSource)
	}
}

// 反过来：不开「关停」时，0 转的风扇仍然要被拦住（不能因为上面的放宽把保护拆掉）。
func TestZeroRPMFanStillRejectedWithoutOffMode(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "0")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	// 非关停档下选中的风扇 0 转，保存时自动剔除而不是报错——用户反插接口后
	// 旧通道不再阻塞配置保存。但全部剔除后没有可用风扇时，必须报错。
	if err := (&Manager{Root: manager.Root}).validateFanLocked(&cfg); err == nil {
		t.Fatal("所有选中风扇都 0 转时必须报错")
	}
}

// 所有选中的风扇都 0 转时，保存必须报错——不能静默保存一个「没有可用风扇」的配置。
func TestAllSelectedFansZeroRPMRejected(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	writeTestValue(t, filepath.Join(hwmon, "fan1_input"), "0")

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := (&Manager{Root: manager.Root}).validateFanLocked(&cfg); err == nil {
		t.Fatal("所有选中风扇都 0 转时必须报错")
	}
}

// 部分风扇 0 转、部分正常时，只剔除 0 转的，保留正常的。
func TestPartialZeroRPMFanFiltered(t *testing.T) {
	manager := newFanDebugTestManager(t)

	cfg := DefaultFanConfig()
	cfg.Enabled = true
	cfg.CPUFanIDs = []string{"it8613:hwmon3:fan1", "it8613:hwmon3:fan2"}
	if err := (&Manager{Root: manager.Root}).validateFanLocked(&cfg); err != nil {
		t.Fatalf("部分风扇 0 转时应自动剔除而非报错: %v", err)
	}
	if len(cfg.CPUFanIDs) != 1 || cfg.CPUFanIDs[0] != "it8613:hwmon3:fan1" {
		t.Fatalf("应只保留有转速的风扇，实际 %+v", cfg.CPUFanIDs)
	}
}

// 关停把风扇停住（RPM=0）之后切回正常档，必须能重新起转：
// 保存不能因为「此刻 0 转」被拒（否则永远回不去），下发要真的把 PWM 拉起来，
// 起转那几秒里周期性下发也不能判失败走 fail-safe。
func TestLeavingOffModeSpinsFansBackUp(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}
	writeTestValue(t, fan1, "0") // 风扇真的停住了

	on := DefaultFanConfig()
	on.Enabled = true
	on.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range on.Curve {
		on.Curve[i].PWMPercent = 100
	}
	if err := manager.SaveFanConfig(on); err != nil {
		t.Fatalf("关停之后切回正常档必须能保存: %v", err)
	}
	if got, _ := readInt(pwm1); got != 255 {
		t.Fatalf("切回正常档要把 PWM 拉满，实际 pwm1=%d", got)
	}

	// 风扇还在起转、转速仍是 0 的这段时间，周期性下发不能失败（失败会 fail-safe）
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("起转过渡期的周期性下发不能失败: %v", err)
	}
	if got, _ := readInt(pwm1); got != 255 {
		t.Fatalf("周期性下发应保持满速，实际 pwm1=%d", got)
	}

	// 转速回来后一切照旧
	writeTestValue(t, fan1, "1900")
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("转速恢复后下发仍应成功: %v", err)
	}
}

// 起转宽限窗口不是永久豁免：离开关停档 30 秒后仍然 0 转的通道必须重新按故障
// 处理（下发 fail-safe 满速、保存被拒）。旧的「见过一次转速就永久免检」会让
// 拔掉/损坏的风扇永远不被发现。
func TestZeroRPMRejectedAfterSpinUpGraceExpires(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	off := DefaultFanConfig()
	off.Enabled = true
	off.OffMode = true
	off.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	for i := range off.Curve {
		off.Curve[i].PWMPercent = 0
	}
	for i := range off.HDDCurve {
		off.HDDCurve[i].PWMPercent = 0
	}
	for i := range off.NVMeCurve {
		off.NVMeCurve[i].PWMPercent = 0
	}
	if err := manager.SaveFanConfig(off); err != nil {
		t.Fatalf("save off: %v", err)
	}
	writeTestValue(t, fan1, "0") // 关停把风扇停住了

	on := DefaultFanConfig()
	on.Enabled = true
	on.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(on); err != nil {
		t.Fatalf("宽限期内切回正常档必须能保存: %v", err)
	}

	// 把宽限期拨到 30 秒之前：模拟用户离开关停档很久之后风扇仍然没有转起来。
	manager.mu.Lock()
	manager.fanOffExitedAt = time.Now().Add(-fanSpinUpGraceWindow - time.Second)
	manager.mu.Unlock()

	// 宽限期过后仍然 0 转：运行时跳过该风扇（不再报错拉满），保存时自动剔除。
	beforeApply, _ := readInt(pwm1)
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("宽限期过后 0 转风扇应被跳过而非报错: %v", err)
	}
	// 风扇保持原样（不 fail-safe 拉满）
	if got, _ := readInt(pwm1); got != beforeApply {
		t.Fatalf("0 转风扇应被跳过而非拉满，pwm1 从 %d 被改写为 %d", beforeApply, got)
	}
	if got, _ := readInt(pwm1); got == 255 {
		t.Fatal("0 转风扇被 fail-safe 拉满到 100%")
	}
	// 保存时重新构建配置（上次的 on 已被修改）
	on2 := DefaultFanConfig()
	on2.Enabled = true
	on2.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(on2); err == nil {
		t.Fatal("宽限期过后保存应报错（全部剔除后无可用风扇）")
	}
}

// 被拒绝的关停配置不得留下假的关停状态：校验在任何运行状态修改之前完成，
// 保存失败时 fanOffWasActive / fanOffExitedAt 都必须保持原样——否则下一轮普通
// 模式会误以为「刚退出关停」而开启 30 秒起转宽限，把真正的 0 转通道当成起转中
// 的风扇重新下发曲线目标，故障风扇就此被豁免。
func TestRejectedOffModeConfigLeavesNoPhantomTransition(t *testing.T) {
	manager := newFanDebugTestManager(t)
	hwmon := filepath.Join(manager.Root, "sys", "class", "hwmon", "hwmon3")
	pwm1 := filepath.Join(hwmon, "pwm1")
	fan1 := filepath.Join(hwmon, "fan1_input")

	// 先保存一份正常配置：fan1 受曲线控制（45°C → 47%）。
	base := DefaultFanConfig()
	base.Enabled = true
	base.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(base); err != nil {
		t.Fatalf("save base: %v", err)
	}

	// 提交无效配置（关停档 + 非法 MinPWMPercent=29）：保存必须失败，
	// 且不得留下「关停档曾生效」的假状态。
	bad := DefaultFanConfig()
	bad.Enabled = true
	bad.OffMode = true
	bad.MinPWMPercent = 29
	bad.CPUFanIDs = []string{"it8613:hwmon3:fan1"}
	if err := manager.SaveFanConfig(bad); err == nil {
		t.Fatal("MinPWMPercent=29 的关停配置必须被拒绝")
	}
	manager.mu.Lock()
	wasActive := manager.fanOffWasActive
	exitedAt := manager.fanOffExitedAt
	manager.mu.Unlock()
	if wasActive {
		t.Fatal("被拒绝的关停配置不得把 fanOffWasActive 置为 true")
	}
	if !exitedAt.IsZero() {
		t.Fatalf("被拒绝的关停配置不得留下关停退出时刻: %v", exitedAt)
	}
	// 磁盘上的配置也仍是旧的正常配置（关停档未生效、未落盘）。
	manager.mu.Lock()
	onDisk, err := manager.loadConfigLocked()
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Fan.OffMode || onDisk.Fan.MinPWMPercent != base.MinPWMPercent {
		t.Fatalf("被拒绝的配置不得落盘: %+v", onDisk.Fan)
	}

	// 风扇此刻 0 转：不存在假的关停宽限时它按故障处理（保存校验剔除 / 运行时
	// 跳过），周期性下发绝不能把它当成「起转中」重新下发曲线目标。先把 pwm1
	// 拨成一个曲线算不出来的值，真假宽限一试便知。
	writeTestValue(t, fan1, "0")
	writeTestValue(t, pwm1, "30")
	if err := manager.ApplyFanCurrent(); err != nil {
		t.Fatalf("0 转风扇应被跳过而非报错: %v", err)
	}
	if got, _ := readInt(pwm1); got != 30 {
		t.Fatalf("0 转风扇应被跳过，pwm1 从 30 被改写为 %d（假的关停宽限让它被当成起转中的风扇）", got)
	}
	manager.mu.Lock()
	exitedAt = manager.fanOffExitedAt
	manager.mu.Unlock()
	if !exitedAt.IsZero() {
		t.Fatalf("宽限状态不得因被拒绝的配置而改变: %v", exitedAt)
	}
}
