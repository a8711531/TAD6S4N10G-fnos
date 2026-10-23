//go:build !linux

package powerguard

// 非 Linux 平台没有 systemd/logind：空实现，保证跨平台编译与本地测试可跑。
func (m *Manager) holdSystemInhibitor(reason string) {}

func (m *Manager) releaseSystemInhibitor() {}
