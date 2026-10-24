//go:build !linux

package powerguard

// noopInhibitor 非 Linux 平台没有 systemd/logind，无关机锁可拿：返回 no-op
// 句柄不算失败（平台本来就没有这层保护概念），保证跨平台编译与本地测试可跑。
// Done 返回 nil 表示不支持提前退出监测。
type noopInhibitor struct{}

func (noopInhibitor) Done() <-chan struct{} { return nil }
func (noopInhibitor) Release() error        { return nil }

func (m *Manager) defaultAcquireInhibitor(reason string) (inhibitorHandle, error) {
	return noopInhibitor{}, nil
}
