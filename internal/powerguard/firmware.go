package powerguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 固件升级模块：BIOS 优先走 flashrom，缺失时回退 Intel FPT（Linux 版）；网卡走 Mellanox MFT。
// 刷写属于高风险动作，所有命令都在后台任务里串行执行，页面只轮询任务日志；
// 任务状态保持内存态，守护进程重启即失效（刷写本身不会被重启续跑）。

const (
	firmwareJobIdle    = "idle"
	firmwareJobRunning = "running"
	firmwareJobSuccess = "success"
	firmwareJobFailed  = "failed"

	firmwareJobKindBIOS = "bios"
	firmwareJobKindNIC  = "nic"

	firmwareImageMaxBytes = 64 << 20
	firmwareToolMaxBytes  = 32 << 20
	firmwareDownloadLimit = 256 << 20

	nicTargetFWVersion = "14.32.1900"
)

var (
	errFirmwareBusy     = errors.New("已有固件任务正在进行，请等待完成")
	errShutdownPending  = errors.New("系统正在关机，已拒绝启动固件刷写")
	errNotLinuxELF      = errors.New("不是 Linux ELF 可执行文件：BIOS 刷写需要 Linux 版 FPT（Fpt.efi 无法在 Linux 下运行）")
	errNICNotDetected   = errors.New("未检测到 Mellanox ConnectX 网卡")
	errPSIDMismatch     = errors.New("网卡 PSID 与目标固件不一致，已停止刷写（跨型号强刷可能变砖）")
	errPSIDUnknown      = errors.New("无法确认网卡 PSID（当前或目标固件 PSID 为空），已停止刷写（跨型号强刷可能变砖）")
	firmwareFWVersionRe = regexp.MustCompile(`(?m)^\s*FW Version:\s*(\S+)`)
	firmwarePSIDRe      = regexp.MustCompile(`(?m)^\s*PSID:\s*(\S+)`)
)

// firmwareDownloadHook 测试注入的下载实现；nil 时走真实 HTTP。
var firmwareDownloadHook func(ctx context.Context, url string) ([]byte, error)

// firmwareCommandRunner 测试注入的命令执行实现；nil 时走真实进程。
type firmwareCommandRunner func(ctx context.Context, dir, name string, args []string) (string, error)

type FirmwareFPTStatus struct {
	Present    bool      `json:"present"`
	Builtin    bool      `json:"builtin"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
}

type FirmwareNICStatus struct {
	Present      bool      `json:"present"`
	PCIAddress   string    `json:"pci_address"`
	Model        string    `json:"model"`
	FWVersion    string    `json:"fw_version"`
	PSID         string    `json:"psid"`
	MFTInstalled bool      `json:"mft_installed"`
	TargetFW     string    `json:"target_fw"`
	CheckedAt    time.Time `json:"checked_at"`
	Error        string    `json:"error"`
}

type FirmwareJobStatus struct {
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Step       string    `json:"step"`
	Progress   int       `json:"progress"`
	Logs       []string  `json:"logs"`
	Error      string    `json:"error"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type FirmwareFlashromStatus struct {
	Present bool   `json:"present"`
	Path    string `json:"path"`
}

type FirmwareStatus struct {
	FPT      FirmwareFPTStatus      `json:"fpt"`
	Flashrom FirmwareFlashromStatus `json:"flashrom"`
	NIC      FirmwareNICStatus      `json:"nic"`
	Job      FirmwareJobStatus      `json:"job"`
	DataDir  string                 `json:"data_dir"`
}

type firmwareJobState struct {
	kind       string
	state      string
	step       string
	progress   int
	logs       []string
	err        string
	startedAt  time.Time
	finishedAt time.Time
}

type firmwareNICInfo struct {
	present      bool
	pciAddress   string
	model        string
	fwVersion    string
	psid         string
	mftInstalled bool
	checkedAt    time.Time
	err          string
}

func (m *Manager) firmwareDataDir() string {
	base := filepath.Dir(m.StatePath)
	if m.StatePath == "" || base == "." {
		base = filepath.Dir(m.ConfigPath)
	}
	if base == "" || base == "." {
		return ""
	}
	return filepath.Join(base, "firmware")
}

func (m *Manager) fptBinaryPath() string {
	dir := m.firmwareDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "fpt-linux")
}

// builtinFPTPath 返回随模块一起分发的 Linux 版 FPT 路径；找不到返回空。
// 自定义上传的版本存放在数据目录，优先级高于内置版本。
func (m *Manager) builtinFPTPath() string {
	candidates := []string{"/usr/local/bin/fpt", "/usr/bin/fpt"}
	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		candidates = append([]string{
			filepath.Join(dir, "fpt-linux"),
			filepath.Join(dir, "tools", "fpt-linux"),
			filepath.Join(dir, "..", "tools", "fpt-linux"),
		}, candidates...)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

// activeFPTPath 选择实际使用的 FPT：用户上传优先，其次内置版本。
func (m *Manager) activeFPTPath() string {
	// 哨兵值显式表示「工具不存在」：宿主机真实安装的 fpt 不得绕过测试桩。
	if m.firmwareFPT == firmwareToolAbsent {
		return ""
	}
	if m.firmwareFPT != "" {
		return m.firmwareFPT
	}
	if path := m.fptBinaryPath(); path != "" {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return m.builtinFPTPath()
}

// flashromPath 返回可用的 flashrom：Linux 下刷写 BIOS 区域的首选工具。
// 依次查找：自定义配置路径、系统安装、之前自动下载到数据目录的副本。
func (m *Manager) flashromPath() string {
	// 哨兵值显式表示「工具不存在」：宿主机真实安装的 flashrom 不得绕过测试桩。
	if m.firmwareFlashrom == firmwareToolAbsent {
		return ""
	}
	if m.firmwareFlashrom != "" {
		if info, err := os.Stat(m.firmwareFlashrom); err == nil && info.Mode().IsRegular() {
			return m.firmwareFlashrom
		}
	}
	if path := findTool("flashrom", []string{"/usr/sbin/flashrom", "/usr/bin/flashrom", "/sbin/flashrom", "/bin/flashrom"}); path != "" {
		return path
	}
	if path := m.downloadedFlashromPath(); path != "" {
		// 已下载副本必须过摘要校验才被选中：半成品/被改动的坏缓存不可复用。
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && flashromCacheVerified(path) {
			return path
		}
	}
	return ""
}

// flashromCacheVerified 校验数据目录里已下载 flashrom 的摘要，只信任与分发值
// 一致的副本；旧版本可能留下写了一半的坏缓存，不能凭「文件存在」就选中。
func flashromCacheVerified(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.EqualFold(sha256Hex(data), flashromExpectedSHA256)
}

// downloadedFlashromPath 是自动下载的 flashrom 存放路径（数据目录下）。
func (m *Manager) downloadedFlashromPath() string {
	dir := m.firmwareDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "flashrom")
}

// flashromExpectedSHA256 是服务器上分发的 flashrom 二进制的 SHA256，
// 下载后必须匹配才会落盘使用，防止下载到被篡改/截断的文件。测试可覆盖。
// 当前二进制：v1.3.0 meson 构建，未启用 libusb/libftdi/libjaylink 编程器
// （客户机缺这些库，Debian 官方包提取的版本跑不起来），只依赖 libpci/libc。
var flashromExpectedSHA256 = "5FC51D03F21C497C4AAB4EC6CDDBDA39CE9705CEC1D345E32A1B79AEC5858D18"

// flashromToolURL 返回服务器上 flashrom 二进制的下载地址（与 BIOS 索引同目录）。
func flashromToolURL() string {
	return filesURL([]string{
		"TANK订制主板相关",
		"TAD6S4N10G(N00_N305)",
		"主板BIOS",
		"flashrom",
	})
}

// ensureFlashrom 保证有可用 flashrom：系统未安装时从 ky.mi-d.cn 下载一份到数据目录。
// 部分用户主机没装 flashrom（只有刷写/读取 BIOS 才需要），自动下载省去手动装包的步骤。
// 缓存原子发布：先写同目录临时文件，SHA256 校验与 --version 试跑都通过后才 rename
// 到正式路径；正式路径要么不存在、要么是完整可用的副本，任何失败都不留可选缓存。
// 并发获取由 flashromMu 串行化，等锁后复查一次，避免重复下载。
func (m *Manager) ensureFlashrom(ctx context.Context) (string, error) {
	if path := m.flashromPath(); path != "" {
		return path, nil
	}
	m.flashromMu.Lock()
	defer m.flashromMu.Unlock()
	// 等锁期间并发调用可能已完成下载，复查一次再决定是否走网络
	if path := m.flashromPath(); path != "" {
		return path, nil
	}
	target := m.downloadedFlashromPath()
	if target == "" {
		return "", errors.New("未找到 flashrom，且模块数据目录不可用，无法自动下载")
	}
	data, err := downloadFirmwareFile(ctx, flashromToolURL(), firmwareToolMaxBytes)
	if err != nil {
		return "", fmt.Errorf("未找到 flashrom，自动下载失败: %w", err)
	}
	if !isLinuxELF(data) {
		return "", errors.New("下载的 flashrom 不是 Linux ELF 可执行文件")
	}
	if hash := sha256Hex(data); !strings.EqualFold(hash, flashromExpectedSHA256) {
		return "", fmt.Errorf("下载的 flashrom 校验和不匹配（期望 %s，实际 %s）", flashromExpectedSHA256, hash)
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpPath := target + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o755); err != nil {
		return "", fmt.Errorf("保存 flashrom 临时文件失败: %w", err)
	}
	// 落盘前最后一步在临时文件上跑 --version 确认依赖库齐全（目标主机缺
	// libftdi/libjaylink 时直接报错），试跑不过只删临时文件，不碰正式路径。
	if output, err := m.runFirmwareCommand(ctx, dir, tmpPath, []string{"--version"}); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("下载的 flashrom 无法运行（可能缺依赖库）: %v %s", err, trimCommandOutput(output))
	}
	if err := os.Rename(tmpPath, target); err != nil {
		// Windows 上目标已存在时 Rename 失败：删除旧副本后重试
		//（Linux 上 rename 本来就原子覆盖，不会走到这里）。
		_ = os.Remove(target)
		if retryErr := os.Rename(tmpPath, target); retryErr != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("发布 flashrom 缓存失败: %v", retryErr)
		}
	}
	return target, nil
}

// activeBIOSBackend 选择 BIOS 刷写后端：优先 flashrom（随系统安装即可用），
// 其次才是用户上传或内置的 Linux 版 FPT（CSME 16 官方已不提供 Linux 版）。
func (m *Manager) activeBIOSBackend() (string, string) {
	if path := m.flashromPath(); path != "" {
		return "flashrom", path
	}
	if path := m.activeFPTPath(); path != "" {
		return "fpt", path
	}
	return "", ""
}

// tryAcquireShutdownSlot 计划关机触发前调用（power_schedule.go）：在同一把
// firmwareMu 内原子完成「确认无刷写任务 + 标记关机进行中」，与
// beginFirmwareJob 的刷写启动检查互斥，堵死「检查通过到关机提交」窗口内
// 另一请求开始刷写的交错（反向交错同样被 shutdownPending 占位挡住）。
// 返回 false 表示刷写进行中，本次关机必须让路。拿到 slot 后若最终放弃关机
// （关机保护窗口跳过/关机命令失败），必须调 releaseShutdownSlot 释放占位；
// 关机命令成功发出后保留占位，直到进程随系统关机退出，期间不得开始刷写。
func (m *Manager) tryAcquireShutdownSlot() bool {
	m.firmwareMu.Lock()
	defer m.firmwareMu.Unlock()
	if m.firmwareJob.state == firmwareJobRunning {
		return false
	}
	m.shutdownPending = true
	return true
}

// releaseShutdownSlot 放弃已占位的计划关机（未真正关机时调用），恢复允许刷写。
func (m *Manager) releaseShutdownSlot() {
	m.firmwareMu.Lock()
	m.shutdownPending = false
	m.firmwareMu.Unlock()
}

// FirmwareStatus 只返回缓存状态，不在轮询路径上执行外部命令。
func (m *Manager) FirmwareStatus() FirmwareStatus {
	m.firmwareMu.Lock()
	defer m.firmwareMu.Unlock()
	return m.firmwareStatusLocked()
}

func (m *Manager) firmwareStatusLocked() FirmwareStatus {
	status := FirmwareStatus{DataDir: m.firmwareDataDir()}
	if path := m.flashromPath(); path != "" {
		status.Flashrom.Present = true
		status.Flashrom.Path = path
	}
	if path := m.activeFPTPath(); path != "" {
		status.FPT.Path = path
		status.FPT.Builtin = path == m.builtinFPTPath()
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			status.FPT.Present = true
			status.FPT.Size = info.Size()
			status.FPT.UploadedAt = info.ModTime()
		}
	}
	info := m.firmwareNIC
	status.NIC = FirmwareNICStatus{
		Present:      info.present,
		PCIAddress:   info.pciAddress,
		Model:        info.model,
		FWVersion:    info.fwVersion,
		PSID:         info.psid,
		MFTInstalled: info.mftInstalled,
		TargetFW:     nicTargetFWVersion,
		CheckedAt:    info.checkedAt,
		Error:        info.err,
	}
	job := m.firmwareJob
	status.Job = FirmwareJobStatus{
		Kind:       job.kind,
		State:      job.state,
		Step:       job.step,
		Progress:   job.progress,
		Error:      job.err,
		StartedAt:  job.startedAt,
		FinishedAt: job.finishedAt,
	}
	if len(job.logs) > 0 {
		status.Job.Logs = append([]string(nil), job.logs...)
	}
	if status.Job.State == "" {
		status.Job.State = firmwareJobIdle
	}
	return status
}

// RefreshFirmwareStatus 重新探测网卡与 MFT 状态（页面首次进入或手动点检测时调用）。
func (m *Manager) RefreshFirmwareStatus(ctx context.Context) FirmwareStatus {
	info := m.detectNIC(ctx)
	m.firmwareMu.Lock()
	m.firmwareNIC = info
	status := m.firmwareStatusLocked()
	m.firmwareMu.Unlock()
	return status
}

func (m *Manager) detectNIC(ctx context.Context) firmwareNICInfo {
	info := firmwareNICInfo{checkedAt: time.Now(), mftInstalled: m.mftInstalled()}
	output, err := m.runFirmwareCommand(ctx, "", "lspci", []string{"-D", "-nn"})
	if err != nil {
		info.err = fmt.Sprintf("执行 lspci 失败: %v", err)
		return info
	}
	pci, model := parseMellanoxLspci(output)
	if pci == "" {
		return info
	}
	info.present = true
	info.pciAddress = pci
	info.model = model
	flint := m.flintPath()
	if flint == "" {
		info.err = "未安装 MFT 工具：点击「开始升级」会自动下载并安装"
		return info
	}
	query, queryErr := m.runFirmwareCommand(ctx, "", flint, []string{"-d", pci, "q"})
	if queryErr != nil {
		info.err = fmt.Sprintf("读取网卡固件信息失败: %v（可先安装 MFT 并执行 mst start）", queryErr)
		return info
	}
	info.fwVersion = firstRegexpGroup(firmwareFWVersionRe, query)
	info.psid = firstRegexpGroup(firmwarePSIDRe, query)
	return info
}

func (m *Manager) mftInstalled() bool {
	return m.flintPath() != "" && m.mstPath() != ""
}

// firmwareToolAbsent 是测试注入字段的哨兵值：显式表示「工具未安装」，
// 避免开发机上真实安装的同名工具（如 Windows 版 WinMFT）被 findTool 命中。
const firmwareToolAbsent = "-"

func (m *Manager) flintPath() string {
	if m.firmwareFlint == firmwareToolAbsent {
		return ""
	}
	if m.firmwareFlint != "" {
		return m.firmwareFlint
	}
	return findTool("flint", []string{
		"/usr/bin/flint",
		"/usr/local/bin/flint",
		"/opt/mellanox/mft/bin/flint",
	})
}

func (m *Manager) mstPath() string {
	if m.firmwareMST == firmwareToolAbsent {
		return ""
	}
	if m.firmwareMST != "" {
		return m.firmwareMST
	}
	return findTool("mst", []string{
		"/usr/bin/mst",
		"/usr/local/bin/mst",
		"/opt/mellanox/mft/bin/mst",
	})
}

func findTool(name string, candidates []string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// SaveFPTBinary 保存用户上传的 Linux 版 FPT 可执行文件。
func (m *Manager) SaveFPTBinary(data []byte) error {
	if len(data) == 0 {
		return errors.New("FPT 文件为空")
	}
	if len(data) > firmwareToolMaxBytes {
		return fmt.Errorf("FPT 文件超过 %d MB 上限", firmwareToolMaxBytes>>20)
	}
	if !isLinuxELF(data) {
		return errNotLinuxELF
	}
	dir := m.firmwareDataDir()
	if dir == "" {
		return errors.New("模块数据目录不可用")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := m.fptBinaryPath()
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(target, 0o755)
}

func isLinuxELF(data []byte) bool {
	return len(data) >= 4 && data[0] == 0x7f && data[1] == 'E' && data[2] == 'L' && data[3] == 'F'
}

// StartBIOSFlash 校验后异步执行 BIOS 刷写；image 为页面选择的 BIOS 镜像。
func (m *Manager) StartBIOSFlash(image []byte) error {
	if len(image) < 1<<20 {
		return errors.New("BIOS 镜像过小（至少 1 MB），请确认选择的是完整 BIOS 文件")
	}
	if len(image) > firmwareImageMaxBytes {
		return fmt.Errorf("BIOS 镜像超过 %d MB 上限", firmwareImageMaxBytes>>20)
	}
	backend, path := m.activeBIOSBackend()
	if path == "" {
		// 系统既没装 flashrom 也没上传 FPT 时，自动从服务器下载 flashrom 到数据目录。
		if downloaded, err := m.ensureFlashrom(context.Background()); err == nil {
			backend, path = "flashrom", downloaded
		} else {
			return fmt.Errorf("未找到 BIOS 刷写工具（%v），也可在「高级：自定义 FPT」中上传 Linux 版 FPT", err)
		}
	}
	if backend == "fpt" {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取 FPT 工具失败: %w", err)
		}
		if !isLinuxELF(data) {
			return errNotLinuxELF
		}
	}
	if err := m.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		return err
	}
	go m.runBIOSFlash(image)
	return nil
}

// StartNICFlash 下载/安装 MFT 后异步刷写网卡固件；firmware 为空时下载内置版本。
func (m *Manager) StartNICFlash(firmware []byte) error {
	if len(firmware) > firmwareImageMaxBytes {
		return fmt.Errorf("网卡固件超过 %d MB 上限", firmwareImageMaxBytes>>20)
	}
	if err := m.beginFirmwareJob(firmwareJobKindNIC); err != nil {
		return err
	}
	go m.runNICFlash(firmware)
	return nil
}

// StartNICMFTInstall 手动安装 MFT：检测到网卡但未安装 MFT 时由页面触发，
// 自动从服务器下载、解压、安装并启动 mst，安装后刷新网卡缓存状态。
func (m *Manager) StartNICMFTInstall() error {
	if err := m.beginFirmwareJob(firmwareJobKindNIC); err != nil {
		return err
	}
	go m.runMFTInstall()
	return nil
}

func (m *Manager) runMFTInstall() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if m.flintPath() != "" && m.mstPath() != "" {
		m.firmwareFinish(nil, "MFT 工具已安装，无需重复安装。")
		return
	}
	if err := m.installMFT(ctx); err != nil {
		m.firmwareFinish(err, "")
		return
	}
	m.firmwareSetStep("启动 MST", 80)
	var mstErr error
	if output, err := m.runFirmwareCommand(ctx, "", m.mstPath(), []string{"start"}); err != nil {
		mstErr = fmt.Errorf("mst start 报错: %v %s", err, trimCommandOutput(output))
		m.firmwareLog("%v", mstErr)
	}
	// 安装后刷新网卡缓存，页面立刻能看到 MFT 已安装与固件版本。
	info := m.detectNIC(ctx)
	m.firmwareMu.Lock()
	m.firmwareNIC = info
	m.firmwareMu.Unlock()
	// 分级上报（评审要求）：安装文件成功、MST 启动成功、网卡已就绪必须分别表达，
	// 不得把「mst 启动失败 / 探测失败 / 无网卡」混成「可开始升级」。
	switch {
	case info.err != "":
		// 探测/查询失败：工具已装上但读不到网卡信息，不能宣称可升级。
		detail := info.err
		if mstErr != nil {
			detail = fmt.Sprintf("%s；%v", detail, mstErr)
		}
		m.firmwareFinish(fmt.Errorf("MFT 工具已安装，但网卡探测失败: %s", detail), "")
	case !info.present:
		// 无网卡是合理结果（可能用户尚未插入网卡），按成功结束但明确「不可升级」。
		msg := "MFT 工具安装完成，但未检测到 Mellanox 网卡：请接入网卡后重新检测，暂不能升级固件。"
		if mstErr != nil {
			msg = fmt.Sprintf("MFT 工具安装完成，但 %v，且未检测到 Mellanox 网卡：请接入网卡后重新检测，暂不能升级固件。", mstErr)
		}
		m.firmwareFinish(nil, msg)
	case info.fwVersion == "":
		// 找到网卡但没读到固件版本（flint 输出解析失败），视为部分成功，不引导升级。
		detail := "网卡固件版本读取失败"
		if mstErr != nil {
			detail = fmt.Sprintf("%s；%v", detail, mstErr)
		}
		m.firmwareFinish(fmt.Errorf("MFT 工具已安装并检测到网卡，但%s：请检查 mst/flint 状态后重试，暂不能升级固件", detail), "")
	default:
		msg := fmt.Sprintf("MFT 工具安装完成，检测到 %s（当前固件 %s），可开始升级网卡固件。", info.model, info.fwVersion)
		if mstErr != nil {
			// mst start 失败但探测链路仍走通（flint 直读 PCI）：保留部分成功注记。
			msg = fmt.Sprintf("MFT 工具安装完成（%v），检测到 %s（当前固件 %s），可开始升级网卡固件。", mstErr, info.model, info.fwVersion)
		}
		m.firmwareFinish(nil, msg)
	}
}

func (m *Manager) beginFirmwareJob(kind string) error {
	m.firmwareMu.Lock()
	if m.firmwareJob.state == firmwareJobRunning {
		m.firmwareMu.Unlock()
		return errFirmwareBusy
	}
	if m.shutdownPending {
		m.firmwareMu.Unlock()
		return errShutdownPending
	}
	m.firmwareJob = firmwareJobState{
		kind:      kind,
		state:     firmwareJobRunning,
		step:      "准备中",
		startedAt: time.Now(),
	}
	m.firmwareMu.Unlock()
	// 刷写窗口内持有系统关机 inhibitor：外部关机/休眠请求会被推迟到刷写结束，
	// 避免写一半断电变砖（本模块自己的计划关机由 shutdownPending 互斥跳过）。
	// 启动子进程不等于拿到锁：获取失败必须回滚任务状态并向上传播，
	// 不得在未确认受保护的情况下进入写入。
	if err := m.holdSystemInhibitor("固件刷写中，禁止关机"); err != nil {
		m.firmwareMu.Lock()
		m.firmwareJob = firmwareJobState{}
		m.firmwareJobInhibitor = nil
		m.firmwareMu.Unlock()
		return fmt.Errorf("无法持有系统关机保护，已取消刷写: %w", err)
	}
	// 记录本任务自己的句柄：后续破坏性写入前据此确认保护仍属于本任务。
	m.firmwareMu.Lock()
	m.firmwareJobInhibitor = m.currentInhibitor()
	m.firmwareMu.Unlock()
	return nil
}

func (m *Manager) firmwareSetStep(step string, progress int) {
	m.firmwareMu.Lock()
	if m.firmwareJob.state == firmwareJobRunning {
		m.firmwareJob.step = step
		m.firmwareJob.progress = progress
	}
	m.firmwareMu.Unlock()
}

func (m *Manager) firmwareLog(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	m.firmwareMu.Lock()
	defer m.firmwareMu.Unlock()
	m.firmwareJob.logs = append(m.firmwareJob.logs, line)
	// 任务日志只保留最近 400 行，避免 MFT 安装输出把内存撑大。
	if len(m.firmwareJob.logs) > 400 {
		m.firmwareJob.logs = append([]string(nil), m.firmwareJob.logs[len(m.firmwareJob.logs)-400:]...)
	}
}

func (m *Manager) firmwareFinish(err error, success string) {
	m.firmwareMu.Lock()
	m.firmwareJob.finishedAt = time.Now()
	if err != nil {
		m.firmwareJob.state = firmwareJobFailed
		m.firmwareJob.err = err.Error()
		m.firmwareJob.logs = append(m.firmwareJob.logs, "失败: "+err.Error())
	} else {
		m.firmwareJob.state = firmwareJobSuccess
		m.firmwareJob.step = "完成"
		m.firmwareJob.progress = 100
		if success != "" {
			m.firmwareJob.logs = append(m.firmwareJob.logs, success)
		}
	}
	// 任务无论成功失败都要释放系统关机 inhibitor，否则用户关机被永久挡住。
	// 摘除句柄与「任务结束」在同一临界区内完成：下一个任务必须在本次解锁之后
	// 才能 begin，因此不会拿到本任务即将释放的旧句柄、误以为已受保护。
	m.firmwareJobInhibitor = nil
	handle := m.releaseSystemInhibitor()
	m.firmwareMu.Unlock()
	// 释放在 firmwareMu 外进行：真实句柄的 Release 要等子进程收尸，持锁等待
	// 会把状态接口一起卡住。只释放刚才摘出的那个句柄，不碰任何后续任务。
	if handle == nil {
		return
	}
	_ = handle.Release()
	m.logfSafe("firmware: 已释放系统关机 inhibitor")
}

func (m *Manager) runBIOSFlash(image []byte) {
	m.firmwareSetStep("写入镜像", 5)
	dir := m.firmwareDataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.firmwareFinish(err, "")
		return
	}
	stamp := time.Now().Format("20060102-150405")
	imagePath := filepath.Join(dir, "bios-"+stamp+".bin")
	if err := os.WriteFile(imagePath, image, 0o600); err != nil {
		m.firmwareFinish(fmt.Errorf("保存 BIOS 镜像失败: %w", err), "")
		return
	}
	m.firmwareLog("BIOS 镜像: %s（%d 字节，SHA256 %s）", imagePath, len(image), sha256Hex(image))
	backup := filepath.Join(dir, "bios-backup-"+stamp+".bin")
	backend, tool := m.activeBIOSBackend()
	if tool == "" {
		m.firmwareFinish(errors.New("未找到可用的 BIOS 刷写工具"), "")
		return
	}
	toolName := "FPT"
	var backupArgs, writeArgs []string
	if backend == "flashrom" {
		toolName = "flashrom"
		backupArgs = []string{"-p", "internal", "--ifd", "-i", "bios", "-r", backup}
		writeArgs = []string{"-p", "internal", "--ifd", "-i", "bios", "-w", imagePath}
	} else {
		backupArgs = []string{"-bios", "-d", backup}
		writeArgs = []string{"-bios", "-f", imagePath}
	}
	m.firmwareSetStep("备份当前 BIOS", 15)
	output, backupErr := m.runFirmwareCommand(context.Background(), dir, tool, backupArgs)
	if backupErr != nil {
		// 没有完整备份就写入，一旦刷坏无法恢复：必须终止任务。
		m.firmwareFinish(fmt.Errorf("备份当前 BIOS 失败（未执行写入，防变砖）: %v %s", backupErr, trimCommandOutput(output)), "")
		return
	}
	backupInfo, statErr := os.Stat(backup)
	if statErr != nil || backupInfo.Size() == 0 {
		m.firmwareFinish(fmt.Errorf("BIOS 备份产物缺失或不完整（%s，%v），未执行写入", backup, statErr), "")
		return
	}
	m.firmwareLog("已备份当前 BIOS: %s（%d 字节）", backup, backupInfo.Size())
	// 破坏性写入前复查关机保护：inhibitor 已提前退出时停止任务，不重复写入。
	if m.firmwareProtectionLost() {
		m.firmwareFinish(errors.New("系统关机保护已失效（inhibitor 提前退出），已停止 BIOS 刷写：未执行写入"), "")
		return
	}
	m.firmwareSetStep("刷写 BIOS（约 1–3 分钟，请勿断电）", 40)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	output, err := m.runFirmwareCommand(ctx, dir, tool, writeArgs)
	if err != nil {
		m.firmwareFinish(fmt.Errorf("%s 刷写失败: %v %s", toolName, err, trimCommandOutput(output)), "")
		return
	}
	m.firmwareLog("%s 输出: %s", toolName, trimCommandOutput(output))
	m.firmwareFinish(nil, "BIOS 写入完成：请完全断电（拔掉电源线并等待 30 秒以上）后重新上电。")
}

func (m *Manager) runNICFlash(firmware []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	m.firmwareSetStep("检测网卡", 5)
	info := m.detectNIC(ctx)
	m.firmwareMu.Lock()
	m.firmwareNIC = info
	m.firmwareMu.Unlock()
	if !info.present {
		m.firmwareFinish(errNICNotDetected, "")
		return
	}
	m.firmwareLog("网卡: %s（PCI %s），当前固件 %s，PSID %s", info.model, info.pciAddress, orDash(info.fwVersion), orDash(info.psid))
	if m.flintPath() == "" || m.mstPath() == "" {
		if err := m.installMFT(ctx); err != nil {
			m.firmwareFinish(err, "")
			return
		}
	}
	m.firmwareSetStep("启动 MST", 35)
	if output, err := m.runFirmwareCommand(ctx, "", m.mstPath(), []string{"start"}); err != nil {
		m.firmwareLog("mst start: %v %s", err, trimCommandOutput(output))
	}
	if _, err := m.runFirmwareCommand(ctx, "", m.mstPath(), []string{"status"}); err != nil {
		m.firmwareFinish(errors.New("mst 未就绪：请确认 MFT 内核模块已加载（可查看上方的 mst start 输出）"), "")
		return
	}
	dev := info.pciAddress
	m.firmwareSetStep("读取当前固件", 45)
	current, err := m.runFirmwareCommand(ctx, "", m.flintPath(), []string{"-d", dev, "q"})
	if err != nil {
		m.firmwareFinish(fmt.Errorf("读取当前固件失败: %v %s", err, trimCommandOutput(current)), "")
		return
	}
	currentPSID := firstRegexpGroup(firmwarePSIDRe, current)
	currentFW := firstRegexpGroup(firmwareFWVersionRe, current)
	m.firmwareLog("当前 FW %s，PSID %s", orDash(currentFW), orDash(currentPSID))

	dir := m.firmwareDataDir()
	if dir == "" {
		m.firmwareFinish(errors.New("模块数据目录不可用"), "")
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.firmwareFinish(err, "")
		return
	}
	stamp := time.Now().Format("20060102-150405")
	fwPath := filepath.Join(dir, "nic-fw-"+stamp+".bin")
	if len(firmware) == 0 {
		m.firmwareSetStep("下载目标固件", 55)
		firmware, err = downloadFirmwareFile(ctx, firmwareNICURL(), firmwareDownloadLimit)
		if err != nil {
			m.firmwareFinish(fmt.Errorf("下载网卡固件失败: %w", err), "")
			return
		}
	}
	if err := os.WriteFile(fwPath, firmware, 0o600); err != nil {
		m.firmwareFinish(fmt.Errorf("保存网卡固件失败: %w", err), "")
		return
	}
	m.firmwareLog("目标固件: %s（%d 字节，SHA256 %s）", fwPath, len(firmware), sha256Hex(firmware))

	m.firmwareSetStep("校验固件与 PSID", 65)
	target, err := m.runFirmwareCommand(ctx, "", m.flintPath(), []string{"-i", fwPath, "q"})
	if err != nil {
		m.firmwareFinish(fmt.Errorf("读取目标固件信息失败: %v %s", err, trimCommandOutput(target)), "")
		return
	}
	targetPSID := firstRegexpGroup(firmwarePSIDRe, target)
	targetFW := firstRegexpGroup(firmwareFWVersionRe, target)
	m.firmwareLog("目标 FW %s，PSID %s", orDash(targetFW), orDash(targetPSID))
	// PSID 未知时不能判断是否跨型号：宁可停止也不放行。
	if currentPSID == "" || targetPSID == "" {
		m.firmwareFinish(fmt.Errorf("%w：当前 PSID=%q，目标 PSID=%q", errPSIDUnknown, currentPSID, targetPSID), "")
		return
	}
	if currentPSID != targetPSID {
		m.firmwareFinish(fmt.Errorf("%w：当前 %s，目标 %s", errPSIDMismatch, currentPSID, targetPSID), "")
		return
	}

	backupDir := filepath.Join(dir, "nic-backup-"+stamp)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		m.firmwareFinish(err, "")
		return
	}
	m.firmwareSetStep("备份当前固件与配置", 75)
	if output, err := m.runFirmwareCommand(ctx, "", m.flintPath(), []string{"-d", dev, "ri", filepath.Join(backupDir, "original-fw.bin")}); err != nil {
		m.firmwareFinish(fmt.Errorf("备份原固件失败（为防变砖已停止）: %v %s", err, trimCommandOutput(output)), "")
		return
	}
	if output, err := m.runFirmwareCommand(ctx, "", m.flintPath(), []string{"-d", dev, "dc", filepath.Join(backupDir, "original-cfg.ini")}); err != nil {
		m.firmwareFinish(fmt.Errorf("备份原配置失败（为防变砖已停止）: %v %s", err, trimCommandOutput(output)), "")
		return
	}
	m.firmwareLog("已备份到 %s", backupDir)

	// 破坏性写入前复查关机保护：备份期间 inhibitor 提前退出时停止任务。
	if m.firmwareProtectionLost() {
		m.firmwareFinish(errors.New("系统关机保护已失效（inhibitor 提前退出），已停止网卡固件烧写：未执行 burn"), "")
		return
	}
	m.firmwareSetStep("烧写固件（请勿断电）", 85)
	output, err := m.runFirmwareCommand(ctx, "", m.flintPath(), []string{"-d", dev, "-i", fwPath, "burn"})
	m.firmwareLog("flint burn 输出: %s", trimCommandOutput(output))
	if err != nil {
		m.firmwareFinish(fmt.Errorf("烧写失败: %v", err), "")
		return
	}
	m.firmwareFinish(nil, "网卡固件烧写完成：必须完全断电（拔掉全部电源线，等待 30 秒以上）后再开机，单纯重启不生效。")
}

func (m *Manager) installMFT(ctx context.Context) error {
	dir := m.firmwareDataDir()
	if dir == "" {
		return errors.New("模块数据目录不可用")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	m.firmwareSetStep("下载 MFT 工具", 15)
	m.firmwareLog("下载 MFT: %s", firmwareMFTURL())
	data, err := downloadFirmwareFile(ctx, firmwareMFTURL(), firmwareDownloadLimit)
	if err != nil {
		return fmt.Errorf("下载 MFT 失败: %w", err)
	}
	tgz := filepath.Join(dir, "mft.tgz")
	if err := os.WriteFile(tgz, data, 0o600); err != nil {
		return fmt.Errorf("保存 MFT 压缩包失败: %w", err)
	}
	extractDir := filepath.Join(dir, "mft")
	if err := os.RemoveAll(extractDir); err != nil {
		return err
	}
	if err := os.MkdirAll(extractDir, 0o700); err != nil {
		return err
	}
	m.firmwareSetStep("解压 MFT 工具", 20)
	if output, err := m.runFirmwareCommand(ctx, extractDir, "tar", []string{"-xzf", tgz, "-C", extractDir}); err != nil {
		return fmt.Errorf("解压 MFT 失败: %v %s", err, trimCommandOutput(output))
	}
	installer := findInstaller(extractDir)
	if installer == "" {
		return errors.New("MFT 压缩包内未找到 install.sh")
	}
	m.firmwareSetStep("安装 MFT 工具", 25)
	m.firmwareLog("执行 %s", installer)
	// install.sh 用 bash 语法（[[ ]]），Debian 默认 sh 是 dash 会报 "[[: not found"
	output, err := m.runFirmwareCommand(ctx, filepath.Dir(installer), "bash", []string{installer})
	m.firmwareLog("install.sh 输出: %s", trimCommandOutput(output))
	if err != nil {
		return fmt.Errorf("安装 MFT 失败: %v", err)
	}
	if m.flintPath() == "" || m.mstPath() == "" {
		return errors.New("MFT 安装完成但未找到 flint/mst，请检查 install.sh 输出")
	}
	return nil
}

func findInstaller(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			candidate := filepath.Join(root, entry.Name(), "install.sh")
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate
			}
			continue
		}
		if entry.Name() == "install.sh" {
			return filepath.Join(root, entry.Name())
		}
	}
	return ""
}

func (m *Manager) runFirmwareCommand(ctx context.Context, dir, name string, args []string) (string, error) {
	if name == "" {
		return "", errors.New("命令不存在")
	}
	if m.firmwareExec != nil {
		return m.firmwareExec(ctx, dir, name, args)
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	var buffer bytes.Buffer
	command.Stdout = &buffer
	command.Stderr = &buffer
	// FPT 等工具可能交互确认：统一喂 y，避免后台任务卡在提问上。
	command.Stdin = strings.NewReader(strings.Repeat("y\n", 16))
	err := command.Run()
	return buffer.String(), err
}

func downloadFirmwareFile(ctx context.Context, address string, limit int64) ([]byte, error) {
	if firmwareDownloadHook != nil {
		return firmwareDownloadHook(ctx, address)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Minute, Transport: firmwareHTTPTransport()}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("文件超过 %d MB 上限", limit>>20)
	}
	return data, nil
}

// firmwareHTTPTransport：部分测试主机的 /etc/resolv.conf 只配了 IPv6 DNS
// （如 AliDNS 2402:4e00::），但本机 IPv6 出口不通，Go 纯 resolver 会一路
// 等超时。这里自定义解析器：DNS 查询一律走 IPv4 传输；遇上 IPv6 的
// nameserver 直接替换成公共 IPv4 DNS，保证能解析出 ky.mi-d.cn 的 A 记录。
// 对端服务器只有 A 记录，数据面本来就走 IPv4，不受影响。
func firmwareHTTPTransport() *http.Transport {
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(address); err == nil {
				if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
					address = net.JoinHostPort("223.5.5.5", port)
				}
			}
			if strings.HasPrefix(network, "udp") {
				network = "udp4"
			} else if strings.HasPrefix(network, "tcp") {
				network = "tcp4"
			}
			dialer := &net.Dialer{Timeout: 5 * time.Second}
			return dialer.DialContext(ctx, network, address)
		},
	}
	// 克隆默认 Transport 再定向替换拨号/解析：保留 ProxyFromEnvironment 等
	// 环境代理支持，只能经 HTTPS_PROXY 出网的环境不会被改成直连。
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{}
	}
	clone := transport.Clone()
	clone.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  resolver,
	}).DialContext
	return clone
}

func firmwareMFTURL() string {
	return filesURL([]string{
		"TANK订制主板相关",
		"TAD6S4N10G(N00_N305)",
		"网卡固件",
		"在线刷固件",
		"linux",
		"mft-4.37.0-154-x86_64-deb.tgz",
	})
}

func firmwareNICURL() string {
	return filesURL([]string{
		"TANK订制主板相关",
		"TAD6S4N10G(N00_N305)",
		"网卡固件",
		"在线刷固件",
		"fw-ConnectX4Lx-rel-14_32_1900-MCX4421A-ACQ_Ax-UEFI-14.25.17-FlexBoot-3.6.502.bin",
	})
}

// filesURL 按教程的下载入口拼接：https://ky.mi-d.cn/?download=<相对路径>。
// 相对路径不能带前缀 ky/，否则服务器返回 200 的说明页而不是文件。
func filesURL(segments []string) string {
	return "https://ky.mi-d.cn/?download=" + url.QueryEscape(strings.Join(segments, "/"))
}

func parseMellanoxLspci(output string) (pci, model string) {
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "mellanox") && !strings.Contains(lower, "connectx") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pci = fields[0]
		// lspci -nn 的格式：0000:01:00.0 网络控制器 [0200]: Mellanox ... [15b3:1015]
		if index := strings.Index(line, ": "); index >= 0 {
			model = strings.TrimSpace(line[index+2:])
		}
		// flint 只需要 domain 之后的 01:00.0。
		if parts := strings.Split(pci, ":"); len(parts) == 3 {
			pci = parts[1] + ":" + parts[2]
		}
		return pci, model
	}
	return "", ""
}

func firstRegexpGroup(expression *regexp.Regexp, text string) string {
	match := expression.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func trimCommandOutput(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 20 {
		lines = append([]string{"…（输出过长，仅显示最后 20 行）"}, lines[len(lines)-20:]...)
	}
	return strings.Join(lines, "\n")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
