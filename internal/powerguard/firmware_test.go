package powerguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirmwareFilesURLEncodesChinese(t *testing.T) {
	mft := firmwareMFTURL()
	if !strings.HasPrefix(mft, "https://ky.mi-d.cn/?download=TANK%E8%AE%A2%E5%88%B6") {
		t.Fatalf("MFT 地址应使用下载入口并转义「订制」: %s", mft)
	}
	if strings.Contains(mft, "%E5%AE%9A%E5%88%B6") {
		t.Fatalf("下载路径仍是「定制」而不是「订制」: %s", mft)
	}
	if !strings.Contains(mft, "%E5%9C%A8%E7%BA%BF%E5%88%B7%E5%9B%BA%E4%BB%B6") {
		t.Fatalf("MFT 地址缺少在线刷固件路径: %s", mft)
	}
	nic := firmwareNICURL()
	if !strings.Contains(nic, "fw-ConnectX4Lx-rel-14_32_1900") {
		t.Fatalf("网卡固件地址异常: %s", nic)
	}
}

func TestParseMellanoxLspci(t *testing.T) {
	output := "0000:01:00.0 网络控制器 [0200]: Mellanox Technologies MT2892 Family [ConnectX-6 Dx] [15b3:101d]\n"
	pci, model := parseMellanoxLspci(output)
	if pci != "01:00.0" {
		t.Fatalf("PCI 地址解析错误: %q", pci)
	}
	if !strings.Contains(model, "Mellanox") {
		t.Fatalf("型号解析错误: %q", model)
	}
	if pci, model := parseMellanoxLspci("0000:00:1f.0 ISA bridge: Intel\n"); pci != "" || model != "" {
		t.Fatalf("非 Mellanox 设备不应命中: %q %q", pci, model)
	}
}

func TestFirmwareQueryParsing(t *testing.T) {
	output := "Image type:            FS4\nFW Version:            14.32.1900\nPSID:                  MT_2420110034\n"
	if got := firstRegexpGroup(firmwareFWVersionRe, output); got != "14.32.1900" {
		t.Fatalf("FW 版本解析错误: %q", got)
	}
	if got := firstRegexpGroup(firmwarePSIDRe, output); got != "MT_2420110034" {
		t.Fatalf("PSID 解析错误: %q", got)
	}
}

func TestSaveFPTBinaryRejectsNonELF(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "original-state.json")}
	if err := manager.SaveFPTBinary([]byte("not elf")); !errors.Is(err, errNotLinuxELF) {
		t.Fatalf("非 ELF 应被拒绝，实际 %v", err)
	}
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 128)...)
	if err := manager.SaveFPTBinary(elf); err != nil {
		t.Fatalf("合法 ELF 应保存成功: %v", err)
	}
	info, err := os.Stat(manager.fptBinaryPath())
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("FPT 文件未落盘: %v", err)
	}
}

func TestStartBIOSFlashRequiresFPT(t *testing.T) {
	manager := &Manager{
		StatePath:        filepath.Join(t.TempDir(), "original-state.json"),
		firmwareFlashrom: firmwareToolAbsent, // 显式「未安装」：宿主 flashrom 不得绕过下载失败桩
		firmwareFPT:      firmwareToolAbsent,
	}
	// 测试隔离（评审要求）：显式模拟工具「不可用」——下载失败、inhibitor 全桩，
	// 绝不触碰真实网络或启动宿主 systemd-inhibit；任何命令执行都是测试缺陷。
	injectFakeInhibitor(manager)
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		t.Fatalf("拒绝启动的请求不得执行任何命令: %s %v", name, args)
		return "", errors.New("不应执行命令")
	}
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		return nil, errors.New("模拟下载不可用")
	}
	defer func() { firmwareDownloadHook = nil }()
	image := make([]byte, 1<<20+1)
	if err := manager.StartBIOSFlash(image); err == nil || !strings.Contains(err.Error(), "FPT") {
		t.Fatalf("未上传 FPT 时应拒绝启动，实际 %v", err)
	}
}

func TestBIOSFlashRunsFPTCommands(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{
		StatePath:        filepath.Join(root, "original-state.json"),
		firmwareFlashrom: firmwareToolAbsent, // 必须走 FPT：宿主 flashrom 不得干扰
	}
	injectFakeInhibitor(manager)
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 64)...)
	if err := manager.SaveFPTBinary(elf); err != nil {
		t.Fatalf("准备 FPT 失败: %v", err)
	}
	var commands []string
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		// 备份命令必须真正落盘产物，否则任务会因「无完整备份」终止（防变砖门槛）。
		if len(args) == 3 && args[0] == "-bios" && args[1] == "-d" {
			if err := os.WriteFile(args[2], []byte("backup"), 0o600); err != nil {
				return "", err
			}
		}
		return "ok", nil
	}
	image := make([]byte, 1<<20+1)
	if err := manager.StartBIOSFlash(image); err != nil {
		t.Fatalf("启动 BIOS 刷写失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobSuccess {
		t.Fatalf("BIOS 刷写应成功，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	var flashCommand string
	for _, command := range commands {
		if strings.Contains(command, " -bios -f ") {
			flashCommand = command
		}
	}
	if flashCommand == "" {
		t.Fatalf("未执行 fpt -bios -f，实际命令: %v", commands)
	}
	if !strings.Contains(flashCommand, "bios-") || !strings.HasSuffix(flashCommand, ".bin") {
		t.Fatalf("刷写命令未指向已保存镜像: %s", flashCommand)
	}
	if len(status.Job.Logs) == 0 || !strings.Contains(strings.Join(status.Job.Logs, "\n"), "完全断电") {
		t.Fatalf("成功日志应提示完全断电: %v", status.Job.Logs)
	}
}

func TestBIOSFlashUsesFlashrom(t *testing.T) {
	root := t.TempDir()
	flashrom := filepath.Join(root, "flashrom")
	if err := os.WriteFile(flashrom, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("准备 flashrom 失败: %v", err)
	}
	manager := &Manager{
		StatePath:        filepath.Join(root, "original-state.json"),
		firmwareFlashrom: flashrom,
		firmwareFPT:      firmwareToolAbsent,
	}
	injectFakeInhibitor(manager)
	var commands []string
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		// flashrom 的备份产物必须真正落盘（-r <backup> 为最后两个参数）。
		if len(args) >= 2 && args[len(args)-2] == "-r" {
			if err := os.WriteFile(args[len(args)-1], []byte("backup"), 0o600); err != nil {
				return "", err
			}
		}
		return "ok", nil
	}
	image := make([]byte, 1<<20+1)
	if err := manager.StartBIOSFlash(image); err != nil {
		t.Fatalf("启动 bios 刷写失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobSuccess {
		t.Fatalf("BIOS 刷写应成功，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	if !status.Flashrom.Present || status.Flashrom.Path != flashrom {
		t.Fatalf("状态应显示 flashrom: %+v", status.Flashrom)
	}
	var backupCommand, writeCommand string
	for _, command := range commands {
		if !strings.Contains(command, "-p internal --ifd -i bios") {
			continue
		}
		if strings.Contains(command, " -r ") {
			backupCommand = command
		}
		if strings.Contains(command, " -w ") {
			writeCommand = command
		}
	}
	if backupCommand == "" || !strings.Contains(backupCommand, "bios-backup-") {
		t.Fatalf("未执行 flashrom 备份，实际命令: %v", commands)
	}
	if writeCommand == "" || !strings.Contains(writeCommand, "bios-") || !strings.HasSuffix(writeCommand, ".bin") {
		t.Fatalf("未执行 flashrom 写入，实际命令: %v", commands)
	}
}

// newFlashromManager 构造只走 flashrom 的刷写管理器（FPT 显式不可用），
// 并注入全模拟 inhibitor，绝不触碰宿主机工具与系统关机保护。
func newFlashromManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	flashrom := filepath.Join(root, "flashrom")
	if err := os.WriteFile(flashrom, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("准备 flashrom 失败: %v", err)
	}
	manager := &Manager{
		StatePath:        filepath.Join(root, "original-state.json"),
		firmwareFlashrom: flashrom,
		firmwareFPT:      firmwareToolAbsent,
	}
	injectFakeInhibitor(manager)
	return manager
}

// 备份失败必须终止任务：没有完整备份就写入，刷坏后无法恢复。
func TestBIOSFlashStopsWhenBackupFails(t *testing.T) {
	manager := newFlashromManager(t)
	writeCalled := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		if len(args) >= 2 && args[len(args)-2] == "-r" {
			return "", errors.New("flash 读取失败")
		}
		if len(args) >= 2 && args[len(args)-2] == "-w" {
			writeCalled = true
		}
		return "ok", nil
	}
	if err := manager.StartBIOSFlash(make([]byte, 1<<20+1)); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed || !strings.Contains(status.Job.Error, "备份") {
		t.Fatalf("备份失败应终止任务，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	if writeCalled {
		t.Fatal("备份失败后不得执行写入")
	}
}

// 备份命令返回成功但产物缺失/为空：仍不得写入。
func TestBIOSFlashStopsWhenBackupMissing(t *testing.T) {
	manager := newFlashromManager(t)
	writeCalled := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		if len(args) >= 2 && args[len(args)-2] == "-w" {
			writeCalled = true
		}
		return "ok", nil // 备份命令不落盘产物
	}
	if err := manager.StartBIOSFlash(make([]byte, 1<<20+1)); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed || !strings.Contains(status.Job.Error, "备份产物") {
		t.Fatalf("备份产物缺失应终止任务，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	if writeCalled {
		t.Fatal("备份产物缺失时不得执行写入")
	}
}

// 备份完成后 inhibitor 提前退出：写入前复查必须终止任务，不得继续写。
func TestBIOSFlashStopsWhenProtectionLostAfterBackup(t *testing.T) {
	manager := newFlashromManager(t)
	fake := injectFakeInhibitor(manager) // 复用桩的句柄列表，模拟提前退出
	writeCalled := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		if len(args) >= 2 && args[len(args)-2] == "-r" {
			if err := os.WriteFile(args[len(args)-1], []byte("backup"), 0o600); err != nil {
				return "", err
			}
			fake.mu.Lock()
			current := fake.handles[len(fake.handles)-1]
			fake.mu.Unlock()
			current.exit() // 备份期间 inhibitor 提前退出
			return "ok", nil
		}
		if len(args) >= 2 && args[len(args)-2] == "-w" {
			writeCalled = true
		}
		return "ok", nil
	}
	if err := manager.StartBIOSFlash(make([]byte, 1<<20+1)); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed || !strings.Contains(status.Job.Error, "关机保护已失效") {
		t.Fatalf("保护失效应终止任务，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	if writeCalled {
		t.Fatal("关机保护失效后不得执行写入")
	}
}

func TestNICFlashPSIDMismatchStopsBeforeBurn(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{
		StatePath:     filepath.Join(root, "original-state.json"),
		firmwareFlint: "flint",
		firmwareMST:   "mst",
	}
	injectFakeInhibitor(manager)
	lspci := "0000:01:00.0 网络控制器 [0200]: Mellanox Technologies MT27710 Family [ConnectX-4 Lx] [15b3:1015]\n"
	burned := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "lspci":
			return lspci, nil
		case name == "mst":
			return "mst started", nil
		case name == "flint" && strings.HasPrefix(joined, "-d 01:00.0 q"):
			return "FW Version: 14.28.1000\nPSID: MT_AAA\n", nil
		case name == "flint" && strings.HasPrefix(joined, "-i ") && strings.HasSuffix(joined, " q"):
			return "FW Version: 14.32.1900\nPSID: MT_BBB\n", nil
		case name == "flint" && strings.Contains(joined, "burn"):
			burned = true
			return "burning", nil
		}
		return "ok", nil
	}
	if err := manager.StartNICFlash([]byte("firmware")); err != nil {
		t.Fatalf("启动网卡刷写失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed {
		t.Fatalf("PSID 不一致应失败，实际 %s", status.Job.State)
	}
	if !strings.Contains(status.Job.Error, "PSID") {
		t.Fatalf("失败原因应说明 PSID: %q", status.Job.Error)
	}
	if burned {
		t.Fatal("PSID 不一致时不得执行 flint burn")
	}
	if !status.NIC.Present || status.NIC.PCIAddress != "01:00.0" {
		t.Fatalf("网卡探测结果异常: %+v", status.NIC)
	}
}

// PSID 未知（当前固件读不到 PSID）时不得放行：无法判断是否跨型号。
func TestNICFlashUnknownPSIDStopsBeforeBurn(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{
		StatePath:     filepath.Join(root, "original-state.json"),
		firmwareFlint: "flint",
		firmwareMST:   "mst",
	}
	injectFakeInhibitor(manager)
	lspci := "0000:01:00.0 网络控制器 [0200]: Mellanox Technologies MT27710 Family [ConnectX-4 Lx] [15b3:1015]\n"
	burned := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "lspci":
			return lspci, nil
		case name == "mst":
			return "mst started", nil
		case name == "flint" && strings.HasPrefix(joined, "-d 01:00.0 q"):
			return "FW Version: 14.28.1000\n", nil // 当前固件读不到 PSID
		case name == "flint" && strings.HasPrefix(joined, "-i ") && strings.HasSuffix(joined, " q"):
			return "FW Version: 14.32.1900\nPSID: MT_BBB\n", nil
		case name == "flint" && strings.Contains(joined, "burn"):
			burned = true
			return "burning", nil
		}
		return "ok", nil
	}
	if err := manager.StartNICFlash([]byte("firmware")); err != nil {
		t.Fatalf("启动网卡刷写失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed || !strings.Contains(status.Job.Error, "PSID") {
		t.Fatalf("PSID 未知应失败，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	if burned {
		t.Fatal("PSID 未知时不得执行 flint burn")
	}
}

// 备份期间 inhibitor 提前退出：烧写前复查必须终止任务，不得执行 burn。
func TestNICFlashStopsWhenProtectionLostBeforeBurn(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{
		StatePath:     filepath.Join(root, "original-state.json"),
		firmwareFlint: "flint",
		firmwareMST:   "mst",
	}
	fake := injectFakeInhibitor(manager)
	lspci := "0000:01:00.0 网络控制器 [0200]: Mellanox Technologies MT27710 Family [ConnectX-4 Lx] [15b3:1015]\n"
	burned := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case name == "lspci":
			return lspci, nil
		case name == "mst":
			return "mst started", nil
		case name == "flint" && strings.HasPrefix(joined, "-d 01:00.0 q"):
			return "FW Version: 14.28.1000\nPSID: MT_AAA\n", nil
		case name == "flint" && strings.HasPrefix(joined, "-i ") && strings.HasSuffix(joined, " q"):
			return "FW Version: 14.32.1900\nPSID: MT_AAA\n", nil
		case name == "flint" && strings.HasPrefix(joined, "-d 01:00.0 ri"):
			// 备份原固件时 inhibitor 提前退出（模拟 logind 掉锁）
			fake.mu.Lock()
			current := fake.handles[len(fake.handles)-1]
			fake.mu.Unlock()
			current.exit()
			return "ok", nil
		case name == "flint" && strings.Contains(joined, "burn"):
			burned = true
			return "burning", nil
		}
		return "ok", nil
	}
	if err := manager.StartNICFlash([]byte("firmware")); err != nil {
		t.Fatalf("启动网卡刷写失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed || !strings.Contains(status.Job.Error, "关机保护已失效") {
		t.Fatalf("保护失效应终止任务，实际 %s（%s）", status.Job.State, status.Job.Error)
	}
	if burned {
		t.Fatal("关机保护失效后不得执行 flint burn")
	}
}

func TestEnsureFlashromDownloads(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: filepath.Join(root, "original-state.json")}
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("fake-flashrom")...)
	oldHash := flashromExpectedSHA256
	flashromExpectedSHA256 = sha256Hex(payload)
	defer func() { flashromExpectedSHA256 = oldHash }()
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		if address != flashromToolURL() {
			t.Fatalf("下载地址异常: %s", address)
		}
		return payload, nil
	}
	defer func() { firmwareDownloadHook = nil }()
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		return "flashrom v1.3", nil
	}
	path, err := manager.ensureFlashrom(context.Background())
	if err != nil {
		t.Fatalf("自动下载 flashrom 失败: %v", err)
	}
	if path != manager.downloadedFlashromPath() {
		t.Fatalf("应使用数据目录副本: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("落盘内容异常: %v", err)
	}
	// 第二次调用应直接命中已下载副本，不再走网络。
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		t.Fatal("已下载副本存在时不应再下载")
		return nil, nil
	}
	if again, err := manager.ensureFlashrom(context.Background()); err != nil || again != path {
		t.Fatalf("应复用已下载副本: %s %v", again, err)
	}
}

func TestStartNICMFTInstallSkipsWhenInstalled(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: filepath.Join(root, "state.json")}
	injectFakeInhibitor(manager)
	// 伪装 MFT 已安装：flint/mst 指向真实存在的文件，installMFT 不应被触发。
	tool := filepath.Join(root, "flint")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager.firmwareFlint = tool
	manager.firmwareMST = tool
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		t.Fatal("MFT 已安装时不应再下载")
		return nil, nil
	}
	defer func() { firmwareDownloadHook = nil }()
	if err := manager.StartNICMFTInstall(); err != nil {
		t.Fatalf("启动 MFT 安装任务失败: %v", err)
	}
	waitFirmwareJob(t, manager)
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobSuccess {
		t.Fatalf("已安装时应直接成功，实际 %s: %s", status.Job.State, status.Job.Error)
	}
	if err := manager.StartNICMFTInstall(); err != nil {
		t.Fatalf("空闲状态下应能再次启动: %v", err)
	}
	waitFirmwareJob(t, manager)
}

func TestEnsureFlashromRejectsHashMismatch(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: filepath.Join(root, "original-state.json")}
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("fake-flashrom")...)
	oldHash := flashromExpectedSHA256
	flashromExpectedSHA256 = strings.Repeat("0", 64)
	defer func() { flashromExpectedSHA256 = oldHash }()
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		return payload, nil
	}
	defer func() { firmwareDownloadHook = nil }()
	if _, err := manager.ensureFlashrom(context.Background()); err == nil || !strings.Contains(err.Error(), "校验和不匹配") {
		t.Fatalf("校验和不匹配应报错: %v", err)
	}
	if _, err := os.Stat(manager.downloadedFlashromPath()); !os.IsNotExist(err) {
		t.Fatalf("校验失败不得落盘: %v", err)
	}
}

// 准备 flashrom 下载测试的公共桩：固定合法载荷与期望摘要，返回 manager 与载荷。
func prepareFlashromDownload(t *testing.T) (*Manager, []byte) {
	t.Helper()
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("fake-flashrom")...)
	oldHash := flashromExpectedSHA256
	flashromExpectedSHA256 = sha256Hex(payload)
	t.Cleanup(func() { flashromExpectedSHA256 = oldHash })
	return manager, payload
}

// 原子发布：--version 试跑在临时文件上进行，失败只清理临时文件，正式路径不留副本。
func TestEnsureFlashromVersionCheckFailureLeavesNoCache(t *testing.T) {
	manager, payload := prepareFlashromDownload(t)
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) { return payload, nil }
	defer func() { firmwareDownloadHook = nil }()
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		if !strings.HasSuffix(name, ".tmp") {
			t.Fatalf("--version 必须在临时文件上试跑，实际: %s", name)
		}
		return "", errors.New("libftdi1 缺失")
	}
	if _, err := manager.ensureFlashrom(context.Background()); err == nil || !strings.Contains(err.Error(), "无法运行") {
		t.Fatalf("试跑失败应报错: %v", err)
	}
	if _, err := os.Stat(manager.downloadedFlashromPath()); !os.IsNotExist(err) {
		t.Fatal("试跑失败不得留下正式缓存")
	}
	if _, err := os.Stat(manager.downloadedFlashromPath() + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("试跑失败要清理临时文件")
	}
}

// 已有损坏缓存（旧版本写了一半）：不得凭「文件存在」被选中，重新下载后原子替换。
func TestEnsureFlashromReplacesBrokenCache(t *testing.T) {
	manager, payload := prepareFlashromDownload(t)
	target := manager.downloadedFlashromPath()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("partial"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := manager.flashromPath(); got != "" {
		t.Fatalf("损坏缓存不得被选中，实际: %s", got)
	}
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) { return payload, nil }
	defer func() { firmwareDownloadHook = nil }()
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		return "flashrom v1.3", nil
	}
	path, err := manager.ensureFlashrom(context.Background())
	if err != nil {
		t.Fatalf("坏缓存下重新下载应成功: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(payload) {
		t.Fatalf("坏缓存应被完整替换: %v", err)
	}
}

// 中途写失败：正式路径不得留下任何可选择的缓存（把 .tmp 预建成目录让 WriteFile 失败）。
func TestEnsureFlashromWriteFailureLeavesNoCache(t *testing.T) {
	manager, payload := prepareFlashromDownload(t)
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) { return payload, nil }
	defer func() { firmwareDownloadHook = nil }()
	target := manager.downloadedFlashromPath()
	if err := os.MkdirAll(target+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(target + ".tmp") }()
	if _, err := manager.ensureFlashrom(context.Background()); err == nil {
		t.Fatal("写入失败应报错")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("写失败不得留下正式缓存")
	}
}

// 并发获取：互斥串行化 + 等锁复查，只下载一次，所有调用拿到同一路径。
func TestEnsureFlashromConcurrentDownload(t *testing.T) {
	manager, payload := prepareFlashromDownload(t)
	var downloads int32
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		atomic.AddInt32(&downloads, 1)
		time.Sleep(20 * time.Millisecond) // 放大并发窗口
		return payload, nil
	}
	defer func() { firmwareDownloadHook = nil }()
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		return "flashrom v1.3", nil
	}
	const callers = 8
	var wg sync.WaitGroup
	paths := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			paths[index], errs[index] = manager.ensureFlashrom(context.Background())
		}(i)
	}
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("并发获取失败: %v", errs[i])
		}
		if paths[i] != manager.downloadedFlashromPath() {
			t.Fatalf("并发获取应得到同一路径，实际: %s", paths[i])
		}
	}
	if got := atomic.LoadInt32(&downloads); got != 1 {
		t.Fatalf("并发获取只应下载一次，实际 %d 次", got)
	}
}

// 下载 Transport：克隆默认 Transport 保留环境代理，只定向覆盖 IPv4 拨号。
func TestFirmwareTransportKeepsEnvironmentProxy(t *testing.T) {
	transport := firmwareHTTPTransport()
	if transport.Proxy == nil {
		t.Fatal("必须保留默认 Transport 的环境代理（ProxyFromEnvironment），只能定向覆盖拨号")
	}
	if transport.DialContext == nil {
		t.Fatal("自定义 IPv4 拨号必须保留")
	}
}

// MFT 安装结果分级上报（评审要求）：安装成功但 mst start 失败、lspci 失败、
// flint 查询失败、无网卡、全部成功五种情形必须分别表达，任务日志和最终状态
// 不得把这些结果混成「可开始升级」。全程命令/下载/发现桩，不触碰真实安装与硬件。
type mftInstallScenario struct {
	name           string
	mstStartErr    error // mst start 是否报错
	lspciErr       error // lspci 执行失败
	lspciOutput    string
	flintQueryErr  error // flint q 查询失败
	flintQueryText string
	wantState      string // 期望最终任务状态
	wantLogMatch   string // 任务日志必须包含
	wantLogReject  string // 任务日志不得包含
}

func TestRunMFTInstallGradedOutcomes(t *testing.T) {
	lspciFound := "0000:01:00.0 网络控制器 [0200]: Mellanox Technologies MT27710 Family [ConnectX-4 Lx] [15b3:1015]\n"
	scenarios := []mftInstallScenario{
		{
			name:           "安装成功但 mst start 失败（探测链路仍通）",
			mstStartErr:    errors.New("mst 模块加载失败"),
			lspciOutput:    lspciFound,
			flintQueryText: "FW Version: 14.32.1900\nPSID: MT_2420110034\n",
			wantState:      firmwareJobSuccess,
			wantLogMatch:   "mst start 报错",
			wantLogReject:  "",
		},
		{
			name:          "安装成功但 mst start 失败且无网卡",
			mstStartErr:   errors.New("mst 模块加载失败"),
			lspciOutput:   "0000:00:1f.0 ISA bridge: Intel\n",
			wantState:     firmwareJobSuccess,
			wantLogMatch:  "暂不能升级固件",
			wantLogReject: "可开始升级",
		},
		{
			name:          "lspci 失败",
			lspciErr:      errors.New("lspci: command not found"),
			wantState:     firmwareJobFailed,
			wantLogMatch:  "网卡探测失败",
			wantLogReject: "可开始升级",
		},
		{
			name:          "flint 查询失败",
			lspciOutput:   lspciFound,
			flintQueryErr: errors.New("flint 打不开设备"),
			wantState:     firmwareJobFailed,
			wantLogMatch:  "网卡探测失败",
			wantLogReject: "可开始升级",
		},
		{
			name:          "无网卡",
			lspciOutput:   "0000:00:1f.0 ISA bridge: Intel\n",
			wantState:     firmwareJobSuccess,
			wantLogMatch:  "未检测到 Mellanox 网卡",
			wantLogReject: "可开始升级",
		},
		{
			name:           "全部成功",
			lspciOutput:    lspciFound,
			flintQueryText: "FW Version: 14.28.1000\nPSID: MT_2420110034\n",
			wantState:      firmwareJobSuccess,
			wantLogMatch:   "可开始升级网卡固件",
			wantLogReject:  "暂不能升级",
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			manager := &Manager{
				StatePath:     filepath.Join(root, "state.json"),
				firmwareFlint: firmwareToolAbsent, // 显式「未安装」：开发机上的 WinMFT 不得干扰
				firmwareMST:   firmwareToolAbsent,
			}
			injectFakeInhibitor(manager)
			// 安装前 MFT 不存在：不触发「已安装」短路，会走 installMFT。
			firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
				return []byte("fake-mft-tgz"), nil
			}
			defer func() { firmwareDownloadHook = nil }()
			manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
				joined := strings.Join(args, " ")
				switch {
				case name == "tar":
					// 解压桩：installMFT 随后要在解包目录找 install.sh，这里模拟落盘。
					//（该桩在任务 goroutine 内执行，失败只能返回 error，不能 t.Fatal）
					extractDir := filepath.Join(manager.firmwareDataDir(), "mft")
					if err := os.WriteFile(filepath.Join(extractDir, "install.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
						return "", err
					}
					return "ok", nil
				case name == "bash":
					// 模拟 install.sh 完成后工具可查：安装后置入路径字段。
					manager.firmwareFlint = "flint"
					manager.firmwareMST = "mst"
					return "installed", nil
				case name == "mst":
					return "", scenario.mstStartErr
				case name == "lspci":
					if scenario.lspciErr != nil {
						return "", scenario.lspciErr
					}
					return scenario.lspciOutput, nil
				case name == "flint" && strings.Contains(joined, " q"):
					if scenario.flintQueryErr != nil {
						return "", scenario.flintQueryErr
					}
					return scenario.flintQueryText, nil
				}
				return "ok", nil
			}
			if err := manager.StartNICMFTInstall(); err != nil {
				t.Fatalf("启动 MFT 安装任务失败: %v", err)
			}
			waitFirmwareJob(t, manager)
			status := manager.FirmwareStatus()
			if status.Job.State != scenario.wantState {
				t.Fatalf("任务状态应为 %s，实际 %s（%s）", scenario.wantState, status.Job.State, status.Job.Error)
			}
			logs := strings.Join(status.Job.Logs, "\n")
			if scenario.wantLogMatch != "" && !strings.Contains(logs+status.Job.Error, scenario.wantLogMatch) {
				t.Fatalf("日志/错误应包含 %q，实际:\n%s\nerr=%s", scenario.wantLogMatch, logs, status.Job.Error)
			}
			if scenario.wantLogReject != "" {
				if strings.Contains(logs, scenario.wantLogReject) || strings.Contains(status.Job.Error, scenario.wantLogReject) {
					t.Fatalf("日志/错误不得包含 %q，实际:\n%s\nerr=%s", scenario.wantLogReject, logs, status.Job.Error)
				}
			}
		})
	}
}

func waitFirmwareJob(t *testing.T, manager *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if manager.FirmwareStatus().Job.State != firmwareJobRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("固件任务超时未结束")
}
