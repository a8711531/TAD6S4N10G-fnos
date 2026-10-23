//go:build linux

package powerguard

import (
	"os/exec"
	"syscall"
)

// holdSystemInhibitor 在固件刷写期间持有 block 型 shutdown/sleep inhibitor：
// 刷写窗口内的系统关机（fnOS 网页、电源键、其他应用、计划任务）会被 logind
// 推迟到刷写结束，而不是直接断电，避免写到一半变砖。本模块自己的计划关机
// 由 power_schedule.go 跳过，这里兜住所有外部关机来源。
//
// inhibitor 随 systemd-inhibit 子进程生命周期释放。子进程设 Pdeathsig，
// 本进程被强杀时一并结束，绝不会因为一次异常而永久挡住用户关机。
func (m *Manager) holdSystemInhibitor(reason string) {
	m.inhibitMu.Lock()
	defer m.inhibitMu.Unlock()
	if m.firmwareInhibitor != nil {
		return
	}
	cmd := exec.Command("systemd-inhibit",
		"--what=shutdown:sleep",
		"--mode=block",
		"--who=fnos-powerguard",
		"--why="+reason,
		"sleep", "infinity")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		// 拿不到 inhibitor 只是少了这层保护，刷写本身照常进行。
		m.logfSafe("firmware: systemd-inhibit 启动失败，刷写期间无法拦截系统关机: %v", err)
		return
	}
	m.firmwareInhibitor = cmd
	m.logfSafe("firmware: 已持有系统关机 inhibitor（%s）", reason)
}

// releaseSystemInhibitor 结束 inhibitor 子进程，系统关机恢复正常。
func (m *Manager) releaseSystemInhibitor() {
	m.inhibitMu.Lock()
	defer m.inhibitMu.Unlock()
	if m.firmwareInhibitor == nil {
		return
	}
	cmd := m.firmwareInhibitor
	m.firmwareInhibitor = nil
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
	m.logfSafe("firmware: 已释放系统关机 inhibitor")
}
