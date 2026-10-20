package powerguard

import (
	"path/filepath"
	"sort"
	"strings"
)

// mobile_devices.go：硬盘面板「移动设备」区块的数据来源。
//
// 判据与存储插入提示音完全一致（storage_block.go）：容量为 0 的空读卡器不算
// ——用户插了卡才想在面板上看到一颗 SD 卡图标。类型（SD 卡 / U 盘）只能尽力
// 猜：内核统一把它们报成 USB 大容量存储，这里按「mmc 总线 / 型号关键词 /
// 已知读卡器 VID:PID」判断，猜不准时退成 U 盘图标，名称列里仍有真实型号可辨。

const (
	// MobileKindSD / MobileKindUSB 是移动设备类型，前端据此选 SD 卡 / U 盘图标。
	MobileKindSD  = "sd"
	MobileKindUSB = "usb"
)

// MobileDevice 是一台当前插着的移动设备（U 盘 / SD 卡 / 移动硬盘）。
type MobileDevice struct {
	Device     string `json:"device"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	KindLabel  string `json:"kind_label"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	SizeLabel  string `json:"size_label,omitempty"`
	Label      string `json:"label,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`
}

// mobileSDModelKeywords 出现在型号/厂商里的「这是张卡」线索（小写比较）。
var mobileSDModelKeywords = []string{
	"sd card", "sdcard", "sd/mmc", "sd/mmc/ms", "mmc", "card reader", "cardreader",
	"flash card", "multi-card", "multi card", "multi-card reader", "读卡器",
}

// mobileSDReaderIDs 是明确做读卡器的 USB VID:PID（05e3:0764 = Genesys Logic
// GL827L，TAD6S4N 模块自带的 SD 卡槽就是它）。不按厂商整体匹配：同厂的
// USB-SATA 桥接（移动硬盘盒）不该被当成 SD 卡。
var mobileSDReaderIDs = map[string]bool{
	"05e3:0764": true,
	"05e3:0765": true,
	"05e3:0766": true,
	"05e3:0767": true,
}

// MobileDevices 列出当前插着的移动设备，按类型与设备名排序。
func (m *Manager) MobileDevices() []MobileDevice {
	base := m.rooted("/sys/block")
	matches, _ := filepath.Glob(filepath.Join(base, "*"))
	if len(matches) == 0 {
		return nil
	}
	var devices []MobileDevice
	for _, match := range matches {
		name := filepath.Base(match)
		if storageBlockIsVirtual(name) {
			continue
		}
		if !m.storageBlockIsRemovable(match) {
			continue
		}
		sectors, err := readInt(filepath.Join(match, "size"))
		if err != nil || sectors <= 0 {
			// 空读卡器的块设备容量是 0：还没插卡，不占一个图标位。
			continue
		}
		model, _ := readTrim(filepath.Join(match, "device", "model"))
		vendor, _ := readTrim(filepath.Join(match, "device", "vendor"))
		resolved := storageBlockResolvedPath(match)
		usbVendor, usbProduct, usbName := m.usbDeviceIdentity(resolved)
		kind := mobileDeviceKind(name, resolved, model+" "+vendor, usbVendor+":"+usbProduct)
		label, mountPoint := m.mobilePartitionInfo(name)
		display := label
		if display == "" {
			display = usbName
		}
		if display == "" {
			display = strings.TrimSpace(model)
		}
		kindLabel := "U 盘"
		if kind == MobileKindSD {
			kindLabel = "SD 卡"
		}
		if display == "" {
			display = kindLabel
		}
		size := sectors * 512
		devices = append(devices, MobileDevice{
			Device:     "/dev/" + name,
			Name:       display,
			Kind:       kind,
			KindLabel:  kindLabel,
			SizeBytes:  size,
			SizeLabel:  formatBlockCapacity(sectors),
			Label:      label,
			MountPoint: mountPoint,
		})
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Kind != devices[j].Kind {
			return devices[i].Kind < devices[j].Kind // sd 排在 usb 前面
		}
		return devices[i].Device < devices[j].Device
	})
	return devices
}

// mobileDeviceKind 猜设备类型：mmc 总线一定是卡，型号关键词与已知读卡器
// VID:PID 作为补充证据，其余一律按 U 盘（外接硬盘盒也落在这里，图标无害）。
func mobileDeviceKind(name, resolved, modelText, usbID string) string {
	lowerName := strings.ToLower(name)
	lowerPath := strings.ToLower(resolved)
	if strings.HasPrefix(lowerName, "mmcblk") || strings.Contains(lowerPath, "/mmc") {
		return MobileKindSD
	}
	text := strings.ToLower(modelText)
	for _, keyword := range mobileSDModelKeywords {
		if strings.Contains(text, keyword) {
			return MobileKindSD
		}
	}
	if mobileSDReaderIDs[strings.ToLower(usbID)] {
		return MobileKindSD
	}
	return MobileKindUSB
}

// usbDeviceIdentity 从块设备的 sysfs 路径反查所属 USB 设备的 VID/PID 与产品名。
// 先从路径里认出 USB 设备名（如 2-1.4），再读 /sys/bus/usb/devices/<name>；
// 认不出来就沿真实路径向上找 idVendor 兜底。
func (m *Manager) usbDeviceIdentity(resolved string) (vendorID, productID, product string) {
	name := usbDeviceNameFromPath(resolved)
	if name != "" {
		dir := m.rooted(filepath.Join("/sys/bus/usb/devices", name))
		vendorID, _ = readTrim(filepath.Join(dir, "idVendor"))
		productID, _ = readTrim(filepath.Join(dir, "idProduct"))
		product, _ = readTrim(filepath.Join(dir, "product"))
		if vendorID != "" {
			return vendorID, productID, product
		}
	}
	dir := resolved
	for depth := 0; depth < 8; depth++ {
		if value, err := readTrim(filepath.Join(dir, "idVendor")); err == nil {
			vendorID = value
			productID, _ = readTrim(filepath.Join(dir, "idProduct"))
			product, _ = readTrim(filepath.Join(dir, "product"))
			return vendorID, productID, product
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", ""
}

// usbDeviceNameFromPath 在 sysfs 路径里找出 USB 设备名：从右往左第一个
// 「数字开头且含 -」的路径段（2-1.4、1-1.2.3 这种），避开 0000:00:0d.0 这类 PCI BDF。
func usbDeviceNameFromPath(resolved string) string {
	parts := strings.Split(filepath.ToSlash(resolved), "/")
	for index := len(parts) - 1; index >= 0; index-- {
		part := parts[index]
		if part == "" || part[0] < '0' || part[0] > '9' {
			continue
		}
		if strings.Contains(part, ":") || !strings.Contains(part, "-") {
			continue
		}
		return part
	}
	return ""
}

// mobilePartitionInfo 找出这块设备（或其分区）的卷标与挂载点：卷标优先给
// 图标悬停名称，挂载点让用户知道盘挂到哪了。
func (m *Manager) mobilePartitionInfo(name string) (label, mountPoint string) {
	labels, _ := m.volumeIdentifiers()
	label = labels[name]
	for _, entry := range m.parseMountInfo() {
		base := filepath.Base(entry.device)
		if base != name {
			rest := strings.TrimPrefix(base, name)
			rest = strings.TrimPrefix(rest, "p")
			if rest == "" || !isAllDigits(rest) {
				continue
			}
		}
		if label == "" {
			label = labels[base]
		}
		if mountPoint == "" {
			mountPoint = entry.mountPoint
		}
	}
	return label, mountPoint
}
