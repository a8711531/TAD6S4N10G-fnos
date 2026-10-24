package powerguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 压力测试：CPU、内存与 GPU 均由后端施压并采样系统指标。
// GPU 通过 ffmpeg + VAAPI 调用核显编码，能真正占用 NAS 侧 iGPU（浏览器 WebGL 只压客户端）。
// 所有状态只存在内存里，停止、超时或进程退出后自动结束。

type StressOptions struct {
	CPU             bool `json:"cpu"`
	GPU             bool `json:"gpu"`
	Memory          bool `json:"memory"`
	DurationSeconds int  `json:"duration_seconds"`
	MemoryMB        int  `json:"memory_mb"`
	// MemoryAuto 自动预留：按当前可用内存扣除 256 MB 系统预留后拉满，
	// 此时 MemoryMB 由后端计算并回填进状态，前端传入值被忽略。
	MemoryAuto bool `json:"memory_auto"`
	// AckOffMode 用户已明确知晓风扇处于「关停」档仍要压测（前端红色确认入口）。
	// 缺省时关停档会拒绝启动，兜底「恢复并开始」回读与启动之间被切回关停的竞态。
	AckOffMode bool `json:"ack_off_mode"`
}

// errStressFanOffMode 关停档（维修/检修）下未明确确认风险时拒绝压测：
// 风扇保持 0 转，压测热量散不出去。
var errStressFanOffMode = errors.New("风扇处于「关停」档：压测会持续升温但风扇不会起转，请先恢复风扇曲线，或确认风险后重试")

type StressStatus struct {
	Running          bool          `json:"running"`
	Options          StressOptions `json:"options"`
	ElapsedSeconds   int           `json:"elapsed_seconds"`
	CPUPercent       float64       `json:"cpu_percent"`
	MemoryUsedBytes  uint64        `json:"memory_used_bytes"`
	MemoryTotalBytes uint64        `json:"memory_total_bytes"`
	MemoryAllocated  int64         `json:"memory_allocated_bytes"`
	Load1            float64       `json:"load1"`
	CPUTempC         float64       `json:"cpu_temp_c"`
	GPUFreqMHz       int           `json:"gpu_freq_mhz"`
	GPUBackend       string        `json:"gpu_backend"`
	GPUError         string        `json:"gpu_error"`
	Error            string        `json:"error"`
}

type stressState struct {
	running      bool
	options      StressOptions
	startedAt    time.Time
	finishedAt   time.Time
	stopCh       chan struct{}
	doneCh       chan struct{}
	cpuPercent   float64
	memUsed      uint64
	memTotal     uint64
	memAllocated int64
	load1        float64
	cpuTempC     float64
	gpuFreqMHz   int
	gpuBackend   string
	gpuError     string
	err          string
}

// stressStartPrecheck 启动前提校验：关停档（维修/检修）下风扇保持 0 转，
// 压测产生的热量散不出去；只有请求显式带 ack_off_mode（前端红色确认入口）才放行。
// 这道服务端门槛兜底「恢复并开始」回读与启动之间被其他客户端切回关停的竞态。
func (m *Manager) stressStartPrecheck(options StressOptions) error {
	if options.AckOffMode {
		return nil
	}
	m.mu.Lock()
	offMode := m.fanOffModeActiveLocked()
	m.mu.Unlock()
	if offMode {
		return errStressFanOffMode
	}
	return nil
}

// StartStress 启动压力测试；CPU、内存与 GPU 工作协程在后台运行，超时自动停止。
// duration 为 0 表示永久运行，只有调用 StopStress 才会结束。
func (m *Manager) StartStress(options StressOptions) error {
	normalized, err := m.normalizeStressOptions(options)
	if err != nil {
		return err
	}
	if err := m.stressStartPrecheck(normalized); err != nil {
		return err
	}
	m.stressMu.Lock()
	if m.stress.running {
		m.stressMu.Unlock()
		return errors.New("压力测试已在运行")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	m.stress = stressState{
		running:      true,
		options:      normalized,
		startedAt:    time.Now(),
		stopCh:       stop,
		doneCh:       done,
		memAllocated: int64(normalized.MemoryMB) << 20,
	}
	m.stressMu.Unlock()

	var gpu *gpuStressProcess
	if normalized.GPU {
		gpu, err = startGPUStressProcess(m)
		if err != nil {
			m.stressMu.Lock()
			m.stress = stressState{}
			m.stressMu.Unlock()
			close(done)
			return err
		}
		m.stressMu.Lock()
		m.stress.gpuBackend = gpu.backend
		m.stressMu.Unlock()
	}

	var workers sync.WaitGroup
	if normalized.CPU {
		// 物理核显之外，N305 有 8 个线程：默认全部打满，负载才接近 AIDA64 的 CPU 压测。
		for index := 0; index < runtime.NumCPU(); index++ {
			workers.Add(1)
			go cpuStressWorker(stop, &workers)
		}
	}
	if normalized.Memory {
		workers.Add(1)
		go memoryStressWorker(stop, &workers, int64(normalized.MemoryMB)<<20)
	}
	if gpu != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			m.runGPUStressProcess(stop, gpu)
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		m.stressSampleLoop(stop, time.Duration(normalized.DurationSeconds)*time.Second)
	}()
	go func() {
		workers.Wait()
		m.stressMu.Lock()
		m.stress.running = false
		m.stress.finishedAt = time.Now()
		m.stress.stopCh = nil
		close(done)
		m.stressMu.Unlock()
	}()
	return nil
}

// StopStress 请求停止；实际停止由工作协程退出后置位，接口立即返回。
func (m *Manager) StopStress() error {
	m.stressMu.Lock()
	defer m.stressMu.Unlock()
	if !m.stress.running {
		return errors.New("没有正在运行的压力测试")
	}
	if m.stress.stopCh != nil {
		close(m.stress.stopCh)
		m.stress.stopCh = nil
	}
	return nil
}

func (m *Manager) StressStatus() StressStatus {
	m.stressMu.Lock()
	defer m.stressMu.Unlock()
	status := StressStatus{
		Running:          m.stress.running,
		Options:          m.stress.options,
		CPUPercent:       m.stress.cpuPercent,
		MemoryUsedBytes:  m.stress.memUsed,
		MemoryTotalBytes: m.stress.memTotal,
		MemoryAllocated:  m.stress.memAllocated,
		Load1:            m.stress.load1,
		CPUTempC:         m.stress.cpuTempC,
		GPUFreqMHz:       m.stress.gpuFreqMHz,
		GPUBackend:       m.stress.gpuBackend,
		GPUError:         m.stress.gpuError,
		Error:            m.stress.err,
	}
	if !m.stress.startedAt.IsZero() {
		end := time.Now()
		if !m.stress.running && !m.stress.finishedAt.IsZero() {
			end = m.stress.finishedAt
		}
		status.ElapsedSeconds = int(end.Sub(m.stress.startedAt).Seconds())
		if status.ElapsedSeconds < 0 {
			status.ElapsedSeconds = 0
		}
	}
	return status
}

func (m *Manager) normalizeStressOptions(options StressOptions) (StressOptions, error) {
	if !options.CPU && !options.Memory && !options.GPU {
		return options, errors.New("请至少勾选 CPU、GPU 或内存中的一项")
	}
	// 0 表示永久测试，只有点击停止才结束；负数按 0 处理。
	if options.DurationSeconds < 0 {
		options.DurationSeconds = 0
	}
	if options.DurationSeconds > 0 && options.DurationSeconds < 10 {
		options.DurationSeconds = 10
	}
	if options.DurationSeconds > 3600 {
		options.DurationSeconds = 3600
	}
	if options.Memory {
		if options.MemoryAuto {
			// 自动预留：不设固定值，按当前可用内存扣掉 256 MB 系统预留后拉满。
			available, ok := readMemAvailable(m.rooted("/proc/meminfo"))
			if !ok || available <= 0 {
				return options, errors.New("无法读取当前可用内存，不能使用自动预留")
			}
			limitMB := int(available>>20) - 256
			if limitMB < 64 {
				return options, fmt.Errorf("可用内存不足（%.0f MB），自动预留扣除 256 MB 后低于 64 MB", float64(available)/1024/1024)
			}
			options.MemoryMB = limitMB
			return options, nil
		}
		if options.MemoryMB <= 0 {
			options.MemoryMB = 512
		}
		if options.MemoryMB < 64 {
			options.MemoryMB = 64
		}
		if available, ok := readMemAvailable(m.rooted("/proc/meminfo")); ok && available > 0 {
			// 不设固定上限：按当前可用内存校验，仅预留 256 MB 给系统。
			limitMB := int(available>>20) - 256
			if limitMB < 64 {
				return options, fmt.Errorf("可用内存不足（%.0f MB），无法启动内存压力测试", float64(available)/1024/1024)
			}
			if options.MemoryMB > limitMB {
				return options, fmt.Errorf("内存压力目标 %d MB 超过安全上限 %d MB（可用内存扣除 256 MB 预留）", options.MemoryMB, limitMB)
			}
		}
	}
	return options, nil
}

func cpuStressWorker(stop <-chan struct{}, workers *sync.WaitGroup) {
	defer workers.Done()
	var value float64
	for {
		select {
		case <-stop:
			return
		default:
		}
		for index := 0; index < 200000; index++ {
			value += math.Sqrt(float64(index)) * math.Cos(float64(index))
		}
		if math.IsInf(value, 0) || math.IsNaN(value) {
			value = 0
		}
	}
}

func memoryStressWorker(stop <-chan struct{}, workers *sync.WaitGroup, total int64) {
	defer workers.Done()
	const chunk = 8 << 20
	blocks := make([][]byte, 0, int(total/chunk)+1)
	for allocated := int64(0); allocated < total; allocated += chunk {
		size := int64(chunk)
		if remaining := total - allocated; remaining < size {
			size = remaining
		}
		block := make([]byte, size)
		// 先写一遍，确保页面真正落位，而不是只占虚拟地址。
		for index := range block {
			block[index] = byte(index)
		}
		blocks = append(blocks, block)
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		for _, block := range blocks {
			for index := 0; index < len(block); index += 4096 {
				block[index]++
			}
		}
	}
}

func (m *Manager) stressSampleLoop(stop <-chan struct{}, duration time.Duration) {
	previous, hasPrevious := readCPUSample(m.rooted("/proc/stat"))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// duration 为 0 表示永久测试：不创建定时器，只等待 StopStress。
	var deadline <-chan time.Time
	if duration > 0 {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case <-stop:
			return
		case <-deadline:
			_ = m.StopStress()
			return
		case <-ticker.C:
			cpuPercent := 0.0
			if current, ok := readCPUSample(m.rooted("/proc/stat")); ok && hasPrevious {
				cpuPercent = cpuUtilization(previous, current)
				previous, hasPrevious = current, true
			}
			memUsed, memTotal := readMemUsage(m.rooted("/proc/meminfo"))
			load1 := readLoad1(m.rooted("/proc/loadavg"))
			gpuFreq := readGPUFrequency(m.rooted("/sys/class/drm"))
			cpuTemp := m.hottestCPUTemperature()

			m.stressMu.Lock()
			m.stress.cpuPercent = cpuPercent
			m.stress.memUsed = memUsed
			m.stress.memTotal = memTotal
			m.stress.load1 = load1
			m.stress.gpuFreqMHz = gpuFreq
			if cpuTemp > m.stress.cpuTempC || m.stress.cpuTempC == 0 {
				m.stress.cpuTempC = cpuTemp
			}
			m.stressMu.Unlock()
		}
	}
}

func (m *Manager) hottestCPUTemperature() float64 {
	var hottest float64
	for _, item := range m.temperatures() {
		if item.Celsius > hottest {
			hottest = item.Celsius
		}
	}
	return hottest
}

func readCPUSample(path string) (cpuSample, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cpuSample{}, false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return cpuSample{}, false
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, false
	}
	var values []uint64
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, false
		}
		values = append(values, value)
	}
	sample := cpuSample{}
	for index, value := range values {
		sample.total += value
		if index == 3 || index == 4 { // idle + iowait
			sample.idle += value
		}
	}
	return sample, true
}

type cpuSample struct {
	total uint64
	idle  uint64
}

func cpuUtilization(previous, current cpuSample) float64 {
	if current.total <= previous.total {
		return 0
	}
	totalDelta := current.total - previous.total
	idleDelta := uint64(0)
	if current.idle > previous.idle {
		idleDelta = current.idle - previous.idle
	}
	if idleDelta >= totalDelta {
		return 0
	}
	return float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
}

func readMemUsage(path string) (used, total uint64) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	memTotal, available, _ := parseMemInfo(string(data))
	if memTotal > available {
		return memTotal - available, memTotal
	}
	return 0, memTotal
}

func readMemAvailable(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	_, available, ok := parseMemInfo(string(data))
	return available, ok
}

func parseMemInfo(text string) (total, available uint64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = value * 1024
			ok = true
		case "MemAvailable":
			available = value * 1024
		}
	}
	return total, available, ok
}

func readLoad1(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return value
}

func readGPUFrequency(drmRoot string) int {
	paths, _ := filepath.Glob(drmRoot + "/card[0-9]*/gt_cur_freq_mhz")
	hottest := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && value > hottest {
			hottest = value
		}
	}
	return hottest
}

const stressGPUBackendVAAPI = "vaapi"

type gpuStressProcess struct {
	cmd     *exec.Cmd
	stderr  *bytes.Buffer
	backend string
}

// startGPUStressProcess 以 ffmpeg + VAAPI 对 NAS 核显施加编码负载。
// 浏览器 WebGL 压的是客户端 GPU，这里必须由后端进程直接占用 renderD128。
func startGPUStressProcess(m *Manager) (*gpuStressProcess, error) {
	ffmpeg := findTool("ffmpeg", []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg"})
	if ffmpeg == "" {
		return nil, errors.New("未找到 ffmpeg，无法对核显施加 GPU 压测")
	}
	devices, _ := filepath.Glob(m.rooted("/dev/dri/renderD*"))
	if len(devices) == 0 {
		return nil, errors.New("未找到 /dev/dri/renderD* 设备，当前系统不支持 VAAPI GPU 压测")
	}
	stderr := &bytes.Buffer{}
	cmd := exec.Command(ffmpeg, []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-vaapi_device", devices[0],
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=60",
		"-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi",
		"-f", "null", "-",
	}...)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 GPU 压测进程失败: %w", err)
	}
	return &gpuStressProcess{cmd: cmd, stderr: stderr, backend: stressGPUBackendVAAPI}, nil
}

// runGPUStressProcess 等待施压进程结束；主动停止时直接 kill，异常退出则记录错误。
func (m *Manager) runGPUStressProcess(stop <-chan struct{}, gpu *gpuStressProcess) {
	exited := make(chan struct{})
	go func() {
		select {
		case <-stop:
			if gpu.cmd.Process != nil {
				_ = gpu.cmd.Process.Kill()
			}
		case <-exited:
		}
	}()
	err := gpu.cmd.Wait()
	close(exited)
	if err == nil {
		return
	}
	select {
	case <-stop:
		return // 主动停止，不算异常
	default:
	}
	message := strings.TrimSpace(gpu.stderr.String())
	if message == "" {
		message = err.Error()
	}
	m.stressMu.Lock()
	m.stress.gpuError = fmt.Sprintf("GPU 压测进程退出：%s", message)
	onlyGPU := !m.stress.options.CPU && !m.stress.options.Memory
	m.stressMu.Unlock()
	if onlyGPU {
		_ = m.StopStress()
	}
}
