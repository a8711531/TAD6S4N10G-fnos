//go:build linux

package powerguard

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// inhibitorReadyDelay 就绪确认窗口：systemd-inhibit 若因 logind/D-Bus 错误
// 拿不到锁，会在启动后立即退出；窗口内未退出才认为保护已生效。启动子进程
// 不等于拿到锁，确认不了就视为获取失败，调用方不得进入刷写。
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
		"--who=fnos-powerguard",
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
	return &processInhibitor{cmd: cmd, done: done}, nil
}
