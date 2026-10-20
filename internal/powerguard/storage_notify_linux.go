//go:build linux

package powerguard

import (
	"fmt"
	"path/filepath"
)

// storage_notify_linux.go：在飞牛（Linux）上枚举块设备并区分「移动存储 / 内置存储」。
//
// 判据只看内核已经认到的块设备与容量，不去探 USB 设备树：容量为 0 的空读卡器
// 不算「插入了存储」，等真正读到介质的那一拍才提示——用户插卡本来想要的就是这个。

func (m *Manager) storageDeviceSnapshot() (storageDeviceSnapshot, error) {
	snapshot := storageDeviceSnapshot{
		removable: make(map[string]int64),
		internal:  make(map[string]int64),
	}
	base := m.rooted("/sys/block")
	matches, _ := filepath.Glob(filepath.Join(base, "*"))
	if len(matches) == 0 {
		return snapshot, fmt.Errorf("没有找到块设备（%s）", base)
	}
	for _, match := range matches {
		name := filepath.Base(match)
		if storageBlockIsVirtual(name) {
			continue
		}
		sectors, err := readInt(filepath.Join(match, "size"))
		if err != nil || sectors <= 0 {
			// 空读卡器的块设备容量是 0，还不算有存储介质。
			continue
		}
		if m.storageBlockIsRemovable(match) {
			snapshot.removable[name] = sectors
		} else {
			snapshot.internal[name] = sectors
		}
	}
	return snapshot, nil
}
