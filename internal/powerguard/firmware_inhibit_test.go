package powerguard

import (
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
// 主动释放后 done 关闭（对应子进程被 Kill 后 Wait 返回）。
type fakeInhibitorHandle struct {
	done     chan struct{}
	released atomic.Int32
}

func (h *fakeInhibitorHandle) Done() <-chan struct{} { return h.done }

func (h *fakeInhibitorHandle) Release() error {
	if h.released.Add(1) == 1 {
		close(h.done)
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
	close(handle.done) // 模拟刷写中子进程异常退出（logind 掉锁）
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
