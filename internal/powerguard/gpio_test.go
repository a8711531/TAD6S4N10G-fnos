package powerguard

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeGPIOPort struct {
	values []byte
}

func (port *fakeGPIOPort) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 || int(offset) >= len(port.values) {
		return 0, io.EOF
	}
	buffer[0] = port.values[offset]
	return 1, nil
}

func (port *fakeGPIOPort) set(spec gpioButtonSpec, high bool) {
	if high {
		port.values[spec.Port] |= 1 << spec.Bit
	} else {
		port.values[spec.Port] &^= 1 << spec.Bit
	}
}

func TestDefaultGPIOConfigIsEnabledAndReadyToUse(t *testing.T) {
	config := DefaultGPIOConfig()
	if !config.Enabled || config.Version != gpioConfigVersion || len(config.Buttons) != 3 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	if err := validateGPIOConfig(config); err != nil {
		t.Fatal(err)
	}
	wantShort := map[string]string{
		"copy":       GPIOActionNone,
		"network":    GPIOActionRefreshSDCard,
		"rear_reset": GPIOActionNone,
	}
	// 开箱即用只保留最常用的一下：网络键短按＝刷新SD卡（换卡后读卡器认不出卡时
	// 随手按一下）。长按三档与其余按键一律留空，免得「按一下不知道会做什么」。
	for _, button := range config.Buttons {
		if button.Actions.Short != wantShort[button.ID] {
			t.Fatalf("default short action is unexpected: %+v", button)
		}
		if button.Actions.Hold3S != GPIOActionNone {
			t.Fatalf("default hold_3s action should stay empty: %+v", button)
		}
		if button.Actions.Hold9S != GPIOActionNone || button.Actions.Hold15S != GPIOActionNone {
			t.Fatalf("default long hold actions should stay empty: %+v", button)
		}
	}
	if config.StorageNotify == nil || !*config.StorageNotify {
		t.Fatal("存储设备插入提示音应当默认开启")
	}
}

func TestNormalizeConfigPreservesShortActions(t *testing.T) {
	config := DefaultConfig(profiles[1])
	config.GPIO.Buttons[0].Actions.Short = GPIOActionLog
	normalizeConfig(&config)
	if config.GPIO.Buttons[0].Actions.Short != GPIOActionLog {
		t.Fatalf("short action was cleared: %+v", config.GPIO.Buttons[0])
	}
}

func TestNormalizeConfigUpgradesUntouchedGPIOConfig(t *testing.T) {
	config := DefaultConfig(profiles[1])
	// 复刻旧版本的默认值：按键映射关闭且四个档位全为无动作。
	config.GPIO.Enabled = false
	for index := range config.GPIO.Buttons {
		config.GPIO.Buttons[index].Actions = GPIOActions{}
	}
	if !normalizeConfig(&config) {
		t.Fatal("untouched gpio config did not trigger migration")
	}
	if !config.GPIO.Enabled {
		t.Fatalf("untouched gpio config was not upgraded: %+v", config.GPIO)
	}
	network, ok := gpioTestButton(config.GPIO, "network")
	if !ok || network.Actions.Short != GPIOActionRefreshSDCard {
		t.Fatalf("网络键短回应被补上刷新SD卡: %+v", network)
	}
	copyButton, _ := gpioTestButton(config.GPIO, "copy")
	if !gpioActionIsEmpty(copyButton.Actions.Short) {
		t.Fatalf("其余按键应保持无动作: %+v", copyButton)
	}
}

func TestNormalizeConfigKeepsDeliberatelyDisabledGPIO(t *testing.T) {
	config := DefaultConfig(profiles[1])
	config.GPIO.Enabled = false
	config.GPIO.Buttons[0].Actions.Hold15S = GPIOActionLog
	normalizeConfig(&config)
	if config.GPIO.Enabled || config.GPIO.Buttons[0].Actions.Hold15S != GPIOActionLog {
		t.Fatalf("deliberate gpio config was overwritten: %+v", config.GPIO)
	}
}

func TestGPIOValidationRejectsUnknownAction(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Buttons[0].Actions.Hold3S = "shell_command"
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("unknown GPIO action was accepted")
	}
}

func TestGPIOValidationRejectsUnknownShortAction(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Buttons[0].Actions.Short = "legacy_unsupported_action"
	if err := validateGPIOConfig(config); err == nil {
		t.Fatal("unknown short action was accepted")
	}
}

func TestGPIOActionForDurationDispatchesShortPress(t *testing.T) {
	actions := GPIOActions{Short: GPIOActionLog, Hold3S: "3", Hold9S: "9", Hold15S: "15"}
	stage, action := gpioActionForDuration(actions, 1200*time.Millisecond)
	if stage != "短按" || action != GPIOActionLog {
		t.Fatalf("short press dispatch = %q/%q, want 短按/%q", stage, action, GPIOActionLog)
	}
}

func TestGPIOValidationAcceptsScriptBinding(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Scripts = []GPIOScript{{ID: "backup-1", Name: "备份脚本", Body: "printf 'ok'\n"}}
	config.Buttons[0].Actions.Hold3S = GPIOActionScriptPrefix + "backup-1"
	if err := validateGPIOConfig(config); err != nil {
		t.Fatal(err)
	}
}

func TestGPIOValidationRejectsInvalidScripts(t *testing.T) {
	valid := GPIOScript{ID: "script-1", Name: "测试脚本", Body: "true\n"}
	for _, test := range []struct {
		name   string
		mutate func(*GPIOConfig)
	}{
		{"path id", func(config *GPIOConfig) { config.Scripts[0].ID = "../bad" }},
		{"empty name", func(config *GPIOConfig) { config.Scripts[0].Name = "  " }},
		{"empty body", func(config *GPIOConfig) { config.Scripts[0].Body = "\n" }},
		{"missing binding", func(config *GPIOConfig) { config.Buttons[0].Actions.Hold3S = GPIOActionScriptPrefix + "missing" }},
		{"duplicate id", func(config *GPIOConfig) { config.Scripts = append(config.Scripts, config.Scripts[0]) }},
		{"duplicate name", func(config *GPIOConfig) {
			config.Scripts = append(config.Scripts, GPIOScript{ID: "script-2", Name: " 测试脚本 ", Body: "true\n"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultGPIOConfig()
			config.Scripts = []GPIOScript{valid}
			test.mutate(&config)
			if err := validateGPIOConfig(config); err == nil {
				t.Fatal("invalid GPIO script configuration was accepted")
			}
		})
	}
}

func TestGPIOPollEmitsShortPressFeedbackAndAction(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Enabled = true
	config.Buttons[0].Actions.Short = GPIOActionLog
	port := &fakeGPIOPort{values: make([]byte, 0xA05)}
	for _, spec := range gpioButtonSpecs {
		port.set(spec, true)
	}
	manager := &Manager{}
	start := time.Unix(100, 0)
	if events, err := manager.PollGPIO(config, port, start); err != nil || len(events) != 0 {
		t.Fatalf("initial poll: events=%+v err=%v", events, err)
	}

	copyButton := gpioButtonSpecs[0]
	port.set(copyButton, false)
	_, _ = manager.PollGPIO(config, port, start.Add(10*time.Millisecond))
	_, _ = manager.PollGPIO(config, port, start.Add(120*time.Millisecond))
	port.set(copyButton, true)
	_, _ = manager.PollGPIO(config, port, start.Add(time.Second))
	events, err := manager.PollGPIO(config, port, start.Add(1120*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("short press produced %d GPIO events: %+v", len(events), events)
	}
	if !events[0].IsFeedback() || events[0].Stage != gpioStageShortPress ||
		events[0].Feedback.Tones != gpioFeedbackTones || events[0].Feedback.Flashes != gpioFeedbackFlashes {
		t.Fatalf("unexpected short press feedback: %+v", events[0])
	}
	if events[1].ButtonID != "copy" || events[1].Stage != gpioStageShortPress || events[1].Action != GPIOActionLog || events[1].Kind != GPIOEventAction {
		t.Fatalf("unexpected short press action event: %+v", events[1])
	}
}

func TestGPIOActionThresholds(t *testing.T) {
	actions := GPIOActions{Short: "s", Hold3S: "3", Hold9S: "9", Hold15S: "15"}
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{0, "s"}, {2 * time.Second, "s"}, {3 * time.Second, "3"},
		{9 * time.Second, "9"}, {15 * time.Second, "15"},
	} {
		_, got := gpioActionForDuration(actions, test.duration)
		if got != test.want {
			t.Fatalf("duration %s got %q, want %q", test.duration, got, test.want)
		}
	}
}

func TestGPIOPollConfirmsThresholdOnceAndResolvesBoundScriptOnRelease(t *testing.T) {
	config := DefaultGPIOConfig()
	config.Enabled = true
	config.Scripts = []GPIOScript{{ID: "hello", Name: "问候", Body: "printf hello\n"}}
	config.Buttons[0].Actions.Hold3S = GPIOActionScriptPrefix + "hello"
	port := &fakeGPIOPort{values: make([]byte, 0xA05)}
	for _, spec := range gpioButtonSpecs {
		port.set(spec, true)
	}
	manager := &Manager{}
	start := time.Unix(200, 0)
	_, _ = manager.PollGPIO(config, port, start)
	copyButton := gpioButtonSpecs[0]
	port.set(copyButton, false)
	_, _ = manager.PollGPIO(config, port, start.Add(10*time.Millisecond))
	_, _ = manager.PollGPIO(config, port, start.Add(120*time.Millisecond))
	confirmations, err := manager.PollGPIO(config, port, start.Add(3120*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmations) != 1 || !confirmations[0].IsFeedback() || confirmations[0].Stage != "长按 3 秒" || confirmations[0].Feedback.Tones != gpioFeedbackTones {
		t.Fatalf("unexpected GPIO confirmation: %+v", confirmations)
	}
	confirmations, err = manager.PollGPIO(config, port, start.Add(3220*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmations) != 0 {
		t.Fatalf("threshold was confirmed more than once: %+v", confirmations)
	}
	port.set(copyButton, true)
	_, _ = manager.PollGPIO(config, port, start.Add(3230*time.Millisecond))
	events, err := manager.PollGPIO(config, port, start.Add(3340*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != GPIOEventAction || events[0].Script == nil || events[0].Script.ID != "hello" || events[0].Script.Body != "printf hello\n" {
		t.Fatalf("script was not resolved into GPIO event: %+v", events)
	}
}

func TestGPIOFeedbackThresholdsShareStandardPattern(t *testing.T) {
	confirmations := gpioFeedbackForDuration(16*time.Second, 0)
	if len(confirmations) != 3 {
		t.Fatalf("got %d confirmations, want 3", len(confirmations))
	}
	for i, want := range []string{"长按 3 秒", "长按 9 秒", "长按 15 秒"} {
		if confirmations[i].stage != want {
			t.Fatalf("confirmation %d stage = %q, want %q", i, confirmations[i].stage, want)
		}
		if confirmations[i].pattern.Tones != gpioFeedbackTones || confirmations[i].pattern.Flashes != gpioFeedbackFlashes {
			t.Fatalf("confirmation %d pattern = %+v, want %d tones / %d flashes",
				i, confirmations[i].pattern, gpioFeedbackTones, gpioFeedbackFlashes)
		}
	}
	if got := gpioFeedbackForDuration(16*time.Second, 0x7); len(got) != 0 {
		t.Fatalf("already-confirmed thresholds produced feedback: %+v", got)
	}
}

type fakeGPIOTone struct {
	count int
	err   error
}

func (tone *fakeGPIOTone) PlayTones(count int) error {
	tone.count = count
	return tone.err
}

type fakeGPIOLED struct {
	count int
	err   error
}

func (led *fakeGPIOLED) Flash(count int) error {
	led.count = count
	return led.err
}

func TestRunGPIOFeedbackAttemptsBothOutputs(t *testing.T) {
	tone := &fakeGPIOTone{err: io.ErrClosedPipe}
	led := &fakeGPIOLED{}
	err := runGPIOFeedback(GPIOFeedbackPattern{Tones: 2, Flashes: 3}, tone, led)
	if err == nil {
		t.Fatal("tone error was lost")
	}
	if tone.count != 2 || led.count != 3 {
		t.Fatalf("feedback calls = tone %d, led %d", tone.count, led.count)
	}
}

func TestExecuteGPIOScriptUsesBashAndEventEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/bash execution is verified by Linux CI")
	}
	event := GPIOEvent{
		ButtonID: "copy", Stage: "短按", Duration: 1250 * time.Millisecond,
		Action: GPIOActionScriptPrefix + "env-test",
		Script: &GPIOScript{ID: "env-test", Name: "环境测试", Body: `printf '%s|%s|%s|%s' "$TAD_GPIO_BUTTON_ID" "$TAD_GPIO_STAGE" "$TAD_GPIO_DURATION_MS" "$TAD_GPIO_SCRIPT_NAME"`},
	}
	output, err := executeGPIOScript(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "copy|短按|1250|环境测试") {
		t.Fatalf("unexpected script output %q", output)
	}
}
