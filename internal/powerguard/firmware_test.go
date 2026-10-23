package powerguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "original-state.json")}
	image := make([]byte, 1<<20+1)
	if err := manager.StartBIOSFlash(image); err == nil || !strings.Contains(err.Error(), "FPT") {
		t.Fatalf("未上传 FPT 时应拒绝启动，实际 %v", err)
	}
}

func TestBIOSFlashRunsFPTCommands(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: filepath.Join(root, "original-state.json")}
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 64)...)
	if err := manager.SaveFPTBinary(elf); err != nil {
		t.Fatalf("准备 FPT 失败: %v", err)
	}
	var commands []string
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
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
	manager := &Manager{StatePath: filepath.Join(root, "original-state.json"), firmwareFlashrom: flashrom}
	var commands []string
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
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

func TestNICFlashPSIDMismatchStopsBeforeBurn(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{
		StatePath:     filepath.Join(root, "original-state.json"),
		firmwareFlint: "flint",
		firmwareMST:   "mst",
	}
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
