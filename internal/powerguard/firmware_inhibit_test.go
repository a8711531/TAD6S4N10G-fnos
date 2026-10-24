package powerguard

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeInhibitor 全模拟的 inhibitor 获取/释放/提前退出桩：默认测试绝不启动
// 真实 systemd-inhibit（评审要求：保护获取/释放及进程发现全部注入）。
type fakeInhibitor struct {
	mu         sync.Mutex
	acquireErr error
	handles    []*fakeInhibitorHandle
}

// fakeInhibitorHandle 模拟一次已持有的关机保护。Release 与真实句柄行为一致：
// 主动释放后 done 关闭（对应子进程被 Kill 后 Wait 返回）。releaseGate 非 nil
// 时先阻塞到其关闭，用于固定「A 完成任务 → B 获取 → A 释放」的交错窗口。
type fakeInhibitorHandle struct {
	done        chan struct{}
	closeOnce   sync.Once
	released    atomic.Int32
	releaseGate chan struct{}
}

func (h *fakeInhibitorHandle) Done() <-chan struct{} { return h.done }

// exit 模拟子进程异常退出（inhibitor 提前失效）：与 Release 竞争时只关闭一次。
func (h *fakeInhibitorHandle) exit() {
	h.closeOnce.Do(func() { close(h.done) })
}

func (h *fakeInhibitorHandle) Release() error {
	if h.released.Add(1) == 1 {
		if h.releaseGate != nil {
			<-h.releaseGate
		}
		h.exit()
	}
	return nil
}

func newFakeInhibitor() *fakeInhibitor { return &fakeInhibitor{} }

func (f *fakeInhibitor) acquire(reason string) (inhibitorHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	h := &fakeInhibitorHandle{done: make(chan struct{})}
	f.handles = append(f.handles, h)
	return h, nil
}

func (f *fakeInhibitor) last() *fakeInhibitorHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.handles) == 0 {
		return nil
	}
	return f.handles[len(f.handles)-1]
}

func (f *fakeInhibitor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handles)
}

// injectFakeInhibitor 给走刷写路径的测试注入全模拟 inhibitor 桩。
func injectFakeInhibitor(m *Manager) *fakeInhibitor {
	fake := newFakeInhibitor()
	m.inhibitorAcquire = fake.acquire
	return fake
}

// inhibitor 获取失败必须向上传播，任务状态回滚，不得在未受保护时进入写入。
func TestBeginFirmwareJobPropagatesInhibitorFailure(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	fake := newFakeInhibitor()
	fake.acquireErr = errors.New("systemd-inhibit 启动后立即退出，未能持有关机锁")
	manager.inhibitorAcquire = fake.acquire

	err := manager.beginFirmwareJob(firmwareJobKindBIOS)
	if err == nil || !strings.Contains(err.Error(), "关机保护") {
		t.Fatalf("inhibitor 获取失败必须传播，实际 %v", err)
	}
	if status := manager.FirmwareStatus(); status.Job.State == firmwareJobRunning {
		t.Fatalf("获取失败后任务状态必须回滚，实际 %s", status.Job.State)
	}
	// 失败不得卡死后续任务：修复获取实现后可以重新启动
	fake.mu.Lock()
	fake.acquireErr = nil
	fake.mu.Unlock()
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatalf("恢复获取后应能启动任务: %v", err)
	}
	manager.firmwareFinish(nil, "")
}

// 关机占位期间刷写启动必须被 errShutdownPending 拒绝；释放占位后恢复。
func TestBeginFirmwareJobRejectsDuringShutdownPending(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	injectFakeInhibitor(manager)
	if !manager.tryAcquireShutdownSlot() {
		t.Fatal("空闲时应能拿到关机占位")
	}
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); !errors.Is(err, errShutdownPending) {
		t.Fatalf("关机已提交必须拒绝刷写，实际 %v", err)
	}
	manager.releaseShutdownSlot()
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatalf("释放占位后应能启动刷写: %v", err)
	}
	manager.firmwareFinish(nil, "")
}

// 反向交错：刷写进行中关机占位不得获准，刷写结束后恢复。
func TestShutdownSlotRejectedWhileFirmwareRunning(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	injectFakeInhibitor(manager)
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatal(err)
	}
	if manager.tryAcquireShutdownSlot() {
		t.Fatal("刷写进行中不得拿到关机占位")
	}
	manager.firmwareFinish(nil, "")
	if !manager.tryAcquireShutdownSlot() {
		t.Fatal("刷写结束后应能拿到关机占位")
	}
	manager.releaseShutdownSlot()
}

// 刷写中 inhibitor 提前退出：句柄摘除、任务日志如实记录，不得继续声称受保护；
// 任务结束时不得再 Release 已退出的句柄。
func TestInhibitorEarlyExitReported(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	fake := injectFakeInhibitor(manager)
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatal(err)
	}
	handle := fake.last()
	if handle == nil {
		t.Fatal("应已获取 inhibitor")
	}
	handle.exit() // 模拟刷写中子进程异常退出（logind 掉锁）
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := manager.FirmwareStatus()
		if strings.Contains(strings.Join(status.Job.Logs, "\n"), "关机保护已失效") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inhibitor 提前退出必须写入任务日志，实际日志: %v", status.Job.Logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.firmwareFinish(nil, "")
	if got := handle.released.Load(); got != 0 {
		t.Fatalf("提前退出的句柄不得再被主动释放，实际 %d 次", got)
	}
}

// 完成任务 A 与释放 A 的句柄之间不得留出「下一个任务继承旧句柄」的窗口：
// A 的完成状态转换内摘除 A 的句柄，B 启动时必须拿到自己的新句柄。
func TestFinishReleasesOnlyOwnInhibitor(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	fake := injectFakeInhibitor(manager)
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	handleA := fake.handles[0]
	handleA.releaseGate = make(chan struct{})
	fake.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		manager.firmwareFinish(nil, "")
		close(finished)
	}()
	// 等待 A 的任务状态转换完成（此时 A 的句柄已在同一临界区内摘除），
	// 且 A 的 Release 已进入并阻塞在 gate 上。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if manager.FirmwareStatus().Job.State != firmwareJobRunning && handleA.released.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A 的完成转换或释放未按预期推进")
		}
		time.Sleep(time.Millisecond)
	}

	// B 在 A 释放之前启动：必须获取自己的句柄，保护有效。
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatalf("A 完成后 B 应能启动: %v", err)
	}
	if got := fake.count(); got != 2 {
		t.Fatalf("B 必须获取自己的 inhibitor，实际句柄数 %d", got)
	}
	handleB := fake.last()
	if handleB == handleA {
		t.Fatal("B 不得继承 A 的旧句柄")
	}
	if manager.firmwareProtectionLost() {
		t.Fatal("B 必须拥有自己的有效保护")
	}

	// 放行 A 的释放：不得误释放 B 的句柄，B 的保护仍需存活。
	close(handleA.releaseGate)
	<-finished
	if got := handleB.released.Load(); got != 0 {
		t.Fatalf("A 的释放不得碰 B 的句柄，实际释放 %d 次", got)
	}
	if manager.firmwareProtectionLost() {
		t.Fatal("A 释放后 B 的保护必须仍然有效")
	}
	manager.firmwareFinish(nil, "")
	if got := handleB.released.Load(); got != 1 {
		t.Fatalf("B 结束时自己的句柄应恰好释放一次，实际 %d", got)
	}
}

// 仅「子进程存活」不构成拿锁确认：获取实现返回未登记错误时任务必须失败，
// 且不得执行任何写入命令。
func TestBeginFirmwareJobRejectsUnregisteredInhibitor(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	execCalled := false
	manager.firmwareExec = func(ctx context.Context, dir, name string, args []string) (string, error) {
		execCalled = true
		return "ok", nil
	}
	fake := newFakeInhibitor()
	fake.acquireErr = errors.New("systemd-inhibit 已启动但未登记有效关机锁（--list 未命中），未能持有关机锁")
	manager.inhibitorAcquire = fake.acquire
	err := manager.beginFirmwareJob(firmwareJobKindBIOS)
	if err == nil || !strings.Contains(err.Error(), "关机保护") {
		t.Fatalf("未登记关机锁必须拒绝启动，实际 %v", err)
	}
	if execCalled {
		t.Fatal("未确认保护时不得执行任何命令")
	}
	if state := manager.FirmwareStatus().Job.State; state == firmwareJobRunning {
		t.Fatalf("被拒绝的任务不得留下运行状态，实际 %s", state)
	}
}

// --list 解析：必须同时命中 WHO、PID 与 WHY，不能靠整表里出现关键词就放行。
func TestInhibitorListShowsLock(t *testing.T) {
	output := "WHO             UID USER PID  COMM  WHAT           WHY                    MODE\n" +
		"fnos-powerguard 0   root 4242 sleep shutdown:sleep 固件刷写中，禁止关机   block\n" +
		"unrelated       0   root 9999 sleep shutdown:sleep 其他任务               block\n"
	if !inhibitorListShowsLock(output, "fnos-powerguard", "固件刷写中，禁止关机", 4242) {
		t.Fatal("who/PID/why 全部匹配时应确认已登记")
	}
	if inhibitorListShowsLock(output, "fnos-powerguard", "固件刷写中，禁止关机", 4243) {
		t.Fatal("PID 不匹配不得确认")
	}
	if inhibitorListShowsLock(output, "other", "固件刷写中，禁止关机", 4242) {
		t.Fatal("WHO 不匹配不得确认")
	}
	if inhibitorListShowsLock(output, "fnos-powerguard", "其他原因", 4242) {
		t.Fatal("WHY 不匹配不得确认")
	}
	if inhibitorListShowsLock("WHO UID USER PID COMM WHAT WHY MODE\n", "fnos-powerguard", "固件刷写中，禁止关机", 4242) {
		t.Fatal("只有表头时不得确认")
	}
}

// 重复获取幂等：已持有时不得新建句柄；任务结束主动释放恰好一次，
// 且主动释放是正常路径，不得误报「提前退出」到任务日志。
func TestHoldSystemInhibitorIdempotent(t *testing.T) {
	manager := &Manager{StatePath: filepath.Join(t.TempDir(), "state.json")}
	fake := injectFakeInhibitor(manager)
	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatal(err)
	}
	if err := manager.holdSystemInhibitor("重复获取"); err != nil {
		t.Fatal(err)
	}
	if got := fake.count(); got != 1 {
		t.Fatalf("已持有时重复获取不得新建句柄，实际 %d 个", got)
	}
	manager.firmwareFinish(nil, "")
	if got := fake.last().released.Load(); got != 1 {
		t.Fatalf("任务结束主动释放应恰好一次，实际 %d 次", got)
	}
	status := manager.FirmwareStatus()
	if strings.Contains(strings.Join(status.Job.Logs, "\n"), "关机保护已失效") {
		t.Fatalf("主动释放不得误报提前退出: %v", status.Job.Logs)
	}
}
