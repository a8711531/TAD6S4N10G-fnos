package powerguard

import "fmt"

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

// releaseSystemInhibitor 主动释放 inhibitor，系统关机恢复正常。
func (m *Manager) releaseSystemInhibitor() {
	m.inhibitMu.Lock()
	handle := m.firmwareInhibitor
	m.firmwareInhibitor = nil
	m.inhibitMu.Unlock()
	if handle == nil {
		return
	}
	_ = handle.Release()
	m.logfSafe("firmware: 已释放系统关机 inhibitor")
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
