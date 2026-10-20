package powerguard

import (
	"path/filepath"
	"strings"
)

// storage_block.go：「块设备算不算移动存储」这组判据。
//
// 原本长在 storage_notify_linux.go 里（存储插入提示音用），后来「硬盘面板」
// 的移动设备图标也要用同一套判据，于是抽成跨平台文件：判据本身只读
// /sys/block 与符号链接，不含任何 Linux 系统调用，放在无构建标签的文件里
// 才能在开发机上直接跑单测。

// storageBlockVirtualPrefixes 是内核提供的「非物理盘」名字前缀：loop（镜像挂载）、
// ram/zram（内存盘）、dm-（LVM / 加密层）、md（软 RAID 阵列）、sr（光驱）、
// nbd/rbd（网络块设备）、fd（软驱）。它们的出现不该被当成「插了一块盘」。
var storageBlockVirtualPrefixes = []string{
	"loop", "ram", "zram", "dm-", "md", "sr", "nbd", "rbd", "fd",
}

func storageBlockIsVirtual(name string) bool {
	for _, prefix := range storageBlockVirtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// storageBlockIsRemovable 判断块设备算不算「移动存储」：内核 removable 标志为 1
// （读卡器与多数 U 盘），或者它挂在 USB 总线下面——很多 USB 硬盘盒把 removable
// 报成 0，只能靠总线路径认。
//
// 注意 sysfs 里 /sys/block/<dev> 是符号链接；这里的符号链接目标是相对路径，
// 所以在 --sys-root 假根下也能解析成功。
func (m *Manager) storageBlockIsRemovable(blockDir string) bool {
	if flag, err := readTrim(filepath.Join(blockDir, "removable")); err == nil && flag == "1" {
		return true
	}
	resolved, err := filepath.EvalSymlinks(blockDir)
	if err != nil {
		return false
	}
	return strings.Contains(filepath.ToSlash(resolved), "/usb")
}

// storageBlockResolvedPath 取块设备在 /sys/devices 下的真实路径（假根下解析失败
// 时退回原路径），USB 设备名与类型判定都从这里取。
func storageBlockResolvedPath(blockDir string) string {
	if resolved, err := filepath.EvalSymlinks(blockDir); err == nil {
		return filepath.ToSlash(resolved)
	}
	return filepath.ToSlash(blockDir)
}
