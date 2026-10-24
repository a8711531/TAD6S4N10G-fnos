package powerguard

import (
	"fmt"
	"strconv"
	"strings"
)

// inhibitorHandle 一次已持有的系统关机 inhibitor。Done 在保护失效（子进程
// 提前退出）时关闭，可为 nil（平台不支持监测）；Release 主动释放保护。
//
// 实现分平台：Linux 为 systemd-inhibit 子进程（firmware_inhibit_linux.go），
// 其他平台为 no-op 句柄（firmware_inhibit_other.go）。测试经
// Manager.inhibitorAcquire 注入全模拟句柄，绝不启动真实系统命令。
type inhibitorHandle interface {
	Done() <-chan struct{}
	Release() error
}

// holdSystemInhibitor 获取系统关机保护并记录句柄。获取失败返回错误，调用方
// 不得在未受保护的情况下进入刷写（刷到一半断电会变砖）。已持有时重复调用
// 直接成功，保持幂等。
func (m *Manager) holdSystemInhibitor(reason string) error {
	m.inhibitMu.Lock()
	if m.firmwareInhibitor != nil {
		m.inhibitMu.Unlock()
		return nil
	}
	m.inhibitMu.Unlock()

	acquire := m.inhibitorAcquire
	if acquire == nil {
		acquire = m.defaultAcquireInhibitor
	}
	handle, err := acquire(reason)
	if err != nil {
		return err
	}
	if handle == nil {
		return fmt.Errorf("inhibitor 获取实现返回了空句柄")
	}

	m.inhibitMu.Lock()
	if m.firmwareInhibitor != nil {
		// 与并发获取竞态：对方先完成，多余的句柄立即释放。
		m.inhibitMu.Unlock()
		_ = handle.Release()
		return nil
	}
	m.firmwareInhibitor = handle
	m.inhibitMu.Unlock()

	if done := handle.Done(); done != nil {
		go m.watchInhibitor(handle, done)
	}
	m.logfSafe("firmware: 已持有系统关机 inhibitor（%s）", reason)
	return nil
}

// releaseSystemInhibitor 摘出当前 inhibitor 并在锁外释放。调用方须持
// firmwareMu：把摘除与任务状态转换放在同一临界区，保证下一个任务在此之后
// 才能 begin、并获取属于它自己的句柄，不会继承本任务即将释放的旧句柄。
// 返回被摘出的句柄，由调用方在锁外 Release（真实句柄要等子进程收尸）。
func (m *Manager) releaseSystemInhibitor() inhibitorHandle {
	m.inhibitMu.Lock()
	handle := m.firmwareInhibitor
	m.firmwareInhibitor = nil
	m.inhibitMu.Unlock()
	return handle
}

// currentInhibitor 返回当前持有的 inhibitor 句柄（调用方须持 firmwareMu，
// 锁序固定 firmwareMu → inhibitMu）。
func (m *Manager) currentInhibitor() inhibitorHandle {
	m.inhibitMu.Lock()
	defer m.inhibitMu.Unlock()
	return m.firmwareInhibitor
}

// firmwareProtectionLost 在破坏性写入前复查本任务的关机保护是否仍然有效：
// 当前持有的句柄必须与任务启动时记录的一致（inhibitor 提前退出会摘除句柄）。
// 保护丢失时不得开始新的写入，避免写到一半断电变砖；已进行中的写入不会被中断。
func (m *Manager) firmwareProtectionLost() bool {
	m.firmwareMu.Lock()
	defer m.firmwareMu.Unlock()
	if m.firmwareJob.state != firmwareJobRunning {
		return true
	}
	current := m.currentInhibitor()
	if current == nil || current != m.firmwareJobInhibitor {
		return true
	}
	// 句柄 Done 已关闭时不必等监视协程调度：子进程已经退出，保护已失效。
	if done := current.Done(); done != nil {
		select {
		case <-done:
			return true
		default:
		}
	}
	return false
}

// inhibitorListShowsLock 在 systemd-inhibit --list 输出中确认指定 PID 的
// inhibitor 已登记：输出为「WHO UID USER PID COMM WHAT WHY MODE」表格，
// 必须同时命中 WHO、PID 与 WHY 文本，不能只看某一行里出现过关键词。
func inhibitorListShowsLock(output, who, why string, pid int) bool {
	pidText := strconv.Itoa(pid)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		if fields[0] != who || fields[3] != pidText {
			continue
		}
		if strings.Contains(line, why) {
			return true
		}
	}
	return false
}

// watchInhibitor 监测 inhibitor 提前退出：保护失效必须如实记录到运行日志与
// 任务日志，不得继续声称受保护。主动释放走 releaseSystemInhibitor，不会触发
// 这里的告警（句柄已被摘出，current 为 false）。
func (m *Manager) watchInhibitor(handle inhibitorHandle, done <-chan struct{}) {
	<-done
	m.inhibitMu.Lock()
	current := m.firmwareInhibitor == handle
	if current {
		m.firmwareInhibitor = nil
	}
	m.inhibitMu.Unlock()
	if !current {
		return
	}
	m.logfSafe("firmware: 系统关机 inhibitor 提前退出，剩余刷写窗口不受关机保护")
	m.firmwareMu.Lock()
	if m.firmwareJob.state == firmwareJobRunning {
		m.firmwareJob.logs = append(m.firmwareJob.logs, "警告: 系统关机保护已失效（inhibitor 提前退出），刷写期间请勿断电")
	}
	m.firmwareMu.Unlock()
}
