package powerguard

import "testing"

const unitConvFanID = "it8613:hwmon3:fan1"

// seedFanCalibration 直接注入标定结果(满转基准 + 特性表),跳过配置载入。
func seedFanCalibration(m *Manager, id string, base int, table map[int]int) {
	m.fanRPMBaseLoad.Do(func() { m.fanRPMBase = map[string]int{} })
	m.fanRPMMapLoad.Do(func() { m.fanRPMLearned = map[string]map[int]int{} })
	m.fanRPMBase[id] = base
	m.fanRPMLearned[id] = table
}

// switchUnitViaAuto 接管 → 以 fromUnit 设定 value → 以 toUnit 开启自动递增
// (长间隔,不会在断言前递增),返回换算后的已接管目标值。
func switchUnitViaAuto(t *testing.T, m *Manager, value int, fromUnit, toUnit string, step int) int {
	t.Helper()
	if err := m.SetFanDebugTakeover(unitConvFanID, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugValue(unitConvFanID, value, fromUnit); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFanDebugAuto(unitConvFanID, true, step, 3600, toUnit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.SetFanDebugAuto(unitConvFanID, false, 0, 0, "") })
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.fanDebugUnits[unitConvFanID]; got != toUnit {
		t.Fatalf("unit should switch to %s, got %s", toUnit, got)
	}
	return m.fanDebugTakenOver[unitConvFanID]
}

func TestFanDebugAutoUnitSwitchPercentToRPMUncalibrated(t *testing.T) {
	m := newFanDebugTestManager(t)
	// 未标定:按默认满转基准 2000 线性换算,50% → PWM 128 → ≈1004 RPM(绝不是 50 RPM)
	got := switchUnitViaAuto(t, m, 50, "percent", "rpm", 100)
	if got < 950 || got > 1050 {
		t.Fatalf("50%% should convert to ~1000 rpm without calibration, got %d", got)
	}
}

func TestFanDebugAutoUnitSwitchPercentToRPMCalibrated(t *testing.T) {
	m := newFanDebugTestManager(t)
	seedFanCalibration(m, unitConvFanID, 4000, map[int]int{64: 900, 128: 2400, 255: 4000})
	got := switchUnitViaAuto(t, m, 50, "percent", "rpm", 100)
	if got != 2400 {
		t.Fatalf("50%% (pwm 128) should map to learned 2400 rpm, got %d", got)
	}
}

func TestFanDebugAutoUnitSwitchRPMToPercentUncalibrated(t *testing.T) {
	m := newFanDebugTestManager(t)
	got := switchUnitViaAuto(t, m, 1000, "rpm", "percent", 5)
	if got != 50 {
		t.Fatalf("1000 rpm should convert to 50%% with default base 2000, got %d", got)
	}
}

func TestFanDebugAutoUnitSwitchRPMToPercentCalibrated(t *testing.T) {
	m := newFanDebugTestManager(t)
	seedFanCalibration(m, unitConvFanID, 4000, map[int]int{64: 900, 128: 2400, 255: 4000})
	got := switchUnitViaAuto(t, m, 2400, "rpm", "percent", 5)
	if got != 50 {
		t.Fatalf("2400 rpm (learned pwm 128) should convert to 50%%, got %d", got)
	}
}

func TestConvertFanDebugValueRoundTrip(t *testing.T) {
	m := newFanDebugTestManager(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if got := m.convertFanDebugValueLocked(unitConvFanID, 40, "percent", "percent"); got != 40 {
		t.Fatalf("same unit must be identity, got %d", got)
	}
	if got := m.convertFanDebugValueLocked(unitConvFanID, 100, "percent", "pwm"); got != 255 {
		t.Fatalf("100%% should be pwm 255, got %d", got)
	}
	if got := m.convertFanDebugValueLocked(unitConvFanID, 100, "percent", "rpm"); got != 2000 {
		t.Fatalf("100%% should be full base 2000 rpm, got %d", got)
	}
	if got := m.convertFanDebugValueLocked(unitConvFanID, 0, "rpm", "percent"); got != 0 {
		t.Fatalf("0 rpm should be 0%%, got %d", got)
	}
}
