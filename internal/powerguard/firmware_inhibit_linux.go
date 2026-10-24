//go:build linux

package powerguard

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const inhibitorWho = "fnos-powerguard"

// inhibitorReadyDelay 就绪确认窗口：systemd-inhibit 若因 logind/D-Bus 错误
// 拿不到锁，会在启动后立即退出；窗口内未退出后还要用 --list 确认登记，
// 仅「子进程还活着」不构成拿锁确认。确认不了就视为获取失败，调用方不得进入刷写。
const inhibitorReadyDelay = 200 * time.Millisecond

// processInhibitor 真实 systemd-inhibit 子进程句柄：Done 在子进程退出
// （无论主动 Kill 还是异常退出）时关闭。
type processInhibitor struct {
	cmd  *exec.Cmd
	done chan struct{} // Wait goroutine 收尸后关闭
}

func (p *processInhibitor) Done() <-chan struct{} { return p.done }

func (p *processInhibitor) Release() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	<-p.done // 等 Wait goroutine 收尸，避免留下僵尸进程
	return nil
}

// defaultAcquireInhibitor 在固件刷写期间持有 block 型 shutdown/sleep inhibitor：
// 刷写窗口内的系统关机（fnOS 网页、电源键、其他应用、计划任务）会被 logind
// 推迟到刷写结束，而不是直接断电，避免写到一半变砖。本模块自己的计划关机
// 由 power_schedule.go 的刷写/关机互斥跳过，这里兜住所有外部关机来源。
//
// inhibitor 随 systemd-inhibit 子进程生命周期释放。子进程设 Pdeathsig，
// 本进程被强杀时一并结束，绝不会因为一次异常而永久挡住用户关机。
func (m *Manager) defaultAcquireInhibitor(reason string) (inhibitorHandle, error) {
	cmd := exec.Command("systemd-inhibit",
		"--what=shutdown:sleep",
		"--mode=block",
		"--who="+inhibitorWho,
		"--why="+reason,
		"sleep", "infinity")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil, errors.New("systemd-inhibit 启动后立即退出，未能持有关机锁")
	case <-time.After(inhibitorReadyDelay):
	}
	// 子进程存活不等于 logind 已登记抑制锁：再查一次活跃 inhibitor 列表，
	// 必须命中 who + 本进程 PID + why。查询失败（无 systemd/无权限）同样
	// 按无法确认处理（fail closed）。
	if !m.inhibitorRegistered(inhibitorWho, reason, cmd.Process.Pid) {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return nil, errors.New("systemd-inhibit 已启动但未登记有效关机锁（--list 未命中），未能持有关机锁")
	}
	return &processInhibitor{cmd: cmd, done: done}, nil
}

// inhibitorRegistered 查询当前活跃 inhibitor，确认指定 PID 的登记存在。
func (m *Manager) inhibitorRegistered(who, why string, pid int) bool {
	output, err := exec.Command("systemd-inhibit", "--list", "--no-pager").Output()
	if err != nil {
		return false
	}
	return inhibitorListShowsLock(string(output), who, why, pid)
}
