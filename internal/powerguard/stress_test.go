package powerguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeStressOptions(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if _, err := manager.normalizeStressOptions(StressOptions{}); err == nil {
		t.Fatal("未勾选任何项目应拒绝启动")
	}
	options, err := manager.normalizeStressOptions(StressOptions{CPU: true, DurationSeconds: 1})
	if err != nil {
		t.Fatalf("CPU 压力参数应通过: %v", err)
	}
	if options.DurationSeconds != 10 {
		t.Fatalf("时长下限应钳制到 10 秒，实际 %d", options.DurationSeconds)
	}
	options, err = manager.normalizeStressOptions(StressOptions{Memory: true, DurationSeconds: 9999})
	if err != nil {
		t.Fatalf("内存压力参数应通过: %v", err)
	}
	if options.DurationSeconds != 3600 || options.MemoryMB != 512 {
		t.Fatalf("时长/内存默认值异常: %+v", options)
	}
	options, err = manager.normalizeStressOptions(StressOptions{CPU: true, DurationSeconds: 0})
	if err != nil {
		t.Fatalf("永久测试应通过: %v", err)
	}
	if options.DurationSeconds != 0 {
		t.Fatalf("0 应表示永久测试，实际 %d", options.DurationSeconds)
	}
}

func TestNormalizeStressMemoryLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	meminfo := "MemTotal:       8388608 kB\nMemAvailable:   2097152 kB\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "meminfo"), []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	// 可用 2048 MB，预留 256 MB 后上限为 1792 MB；不再有固定 4096 MB 上限。
	if _, err := manager.normalizeStressOptions(StressOptions{Memory: true, MemoryMB: 2048}); err == nil {
		t.Fatal("超过可用内存（扣除预留）应拒绝")
	}
	if _, err := manager.normalizeStressOptions(StressOptions{Memory: true, MemoryMB: 1700}); err != nil {
		t.Fatalf("安全范围内的内存压力应通过: %v", err)
	}
}

func TestCPUSampleAndUtilization(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 0 1 7 1 0 0 0 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	sample, ok := readCPUSample(filepath.Join(root, "proc", "stat"))
	if !ok {
		t.Fatal("应解析 /proc/stat")
	}
	if sample.total != 1000 || sample.idle != 800 {
		t.Fatalf("CPU 样本解析错误: %+v", sample)
	}
	previous := cpuSample{total: 1000, idle: 800}
	current := cpuSample{total: 2000, idle: 900}
	if got := cpuUtilization(previous, current); got != 90 {
		t.Fatalf("CPU 使用率计算错误: %.1f", got)
	}
}

func TestStressStartStop(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if err := manager.StartStress(StressOptions{CPU: true, DurationSeconds: 10}); err != nil {
		t.Fatalf("启动压力测试失败: %v", err)
	}
	if !manager.StressStatus().Running {
		t.Fatal("启动后应为运行状态")
	}
	if err := manager.StartStress(StressOptions{CPU: true}); err == nil {
		t.Fatal("重复启动应被拒绝")
	}
	if err := manager.StopStress(); err != nil {
		t.Fatalf("停止压力测试失败: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !manager.StressStatus().Running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("停止后工作协程未退出")
}

func TestStressStatusReportsOptions(t *testing.T) {
	manager := &Manager{Root: t.TempDir()}
	if err := manager.StartStress(StressOptions{Memory: true, MemoryMB: 64, DurationSeconds: 20}); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer func() { _ = manager.StopStress() }()
	status := manager.StressStatus()
	if !status.Options.Memory || status.Options.MemoryMB != 64 {
		t.Fatalf("状态未返回启动参数: %+v", status.Options)
	}
	if status.ElapsedSeconds < 0 {
		t.Fatalf("运行时长不应为负: %d", status.ElapsedSeconds)
	}
}

func TestParseMemInfo(t *testing.T) {
	total, available, ok := parseMemInfo("MemTotal: 1024 kB\nMemFree: 100 kB\nMemAvailable: 512 kB\n")
	if !ok || total != 1024*1024 || available != 512*1024 {
		t.Fatalf("meminfo 解析错误: %d %d %v", total, available, ok)
	}
}

func TestReadGPUFrequency(t *testing.T) {
	root := t.TempDir()
	card := filepath.Join(root, "card0")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(card, "gt_cur_freq_mhz"), []byte("1200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readGPUFrequency(root); got != 1200 {
		t.Fatalf("GPU 频率读取错误: %d", got)
	}
}

func TestTrimCommandOutputKeepsTail(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < 30; index++ {
		builder.WriteString("line\n")
	}
	trimmed := trimCommandOutput(builder.String())
	if !strings.Contains(trimmed, "输出过长") {
		t.Fatalf("超长输出应被截断: %q", trimmed)
	}
}
