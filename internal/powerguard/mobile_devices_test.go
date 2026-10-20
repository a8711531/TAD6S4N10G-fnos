package powerguard

import (
	"os"
	"path/filepath"
	"testing"
)

// mobile_devices_test.go：硬盘面板「移动设备」的枚举与类型判定。
// 全部走假根 sysfs，不需要 root，也不需要真插卡。

func writeMobileFixture(t *testing.T, root, path, value string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 块设备骨架：size（512 字节扇区）与 removable 决定算不算移动设备。
func addMobileBlock(t *testing.T, root, name, size, removable, model, usbID string) {
	t.Helper()
	base := "/sys/block/" + name
	writeMobileFixture(t, root, base+"/size", size)
	writeMobileFixture(t, root, base+"/removable", removable)
	if model != "" {
		writeMobileFixture(t, root, base+"/device/model", model)
	}
	if usbID != "" {
		parts := splitVendorProduct(usbID)
		writeMobileFixture(t, root, base+"/idVendor", parts[0])
		writeMobileFixture(t, root, base+"/idProduct", parts[1])
	}
}

func splitVendorProduct(id string) [2]string {
	for index := 0; index < len(id); index++ {
		if id[index] == ':' {
			return [2]string{id[:index], id[index+1:]}
		}
	}
	return [2]string{id, ""}
}

func TestMobileDevicesOnlyListsInsertedMedia(t *testing.T) {
	root := t.TempDir()
	// 内置 SATA 盘：不算移动设备
	addMobileBlock(t, root, "sda", "7814037168", "0", "WDC WD40PURX", "")
	// 空读卡器：removable=1 但容量 0，还没插卡
	addMobileBlock(t, root, "sdf", "0", "1", "MassStorageClass", "05e3:0764")
	// 插了卡的读卡器
	addMobileBlock(t, root, "sdg", "124735488", "1", "MassStorageClass", "05e3:0764")
	// U 盘
	addMobileBlock(t, root, "sdh", "62521344", "1", "USB DISK 3.0", "")

	manager := &Manager{Root: root}
	devices := manager.MobileDevices()
	if len(devices) != 2 {
		t.Fatalf("应只列出 2 个已插介质，实际 %d：%+v", len(devices), devices)
	}
	if devices[0].Device != "/dev/sdg" || devices[0].Kind != MobileKindSD {
		t.Fatalf("读卡器应识别为 SD 卡：%+v", devices[0])
	}
	if devices[1].Device != "/dev/sdh" || devices[1].Kind != MobileKindUSB {
		t.Fatalf("U 盘应识别为 usb：%+v", devices[1])
	}
	if devices[0].KindLabel != "SD 卡" || devices[1].KindLabel != "U 盘" {
		t.Fatalf("类型文案不对：%+v", devices)
	}
	if devices[0].SizeLabel == "" || devices[0].Name == "" {
		t.Fatalf("容量与名称不能为空：%+v", devices[0])
	}
}

func TestMobileDeviceKindUsesBusAndModelHints(t *testing.T) {
	cases := []struct {
		name     string
		resolved string
		model    string
		usbID    string
		want     string
	}{
		{"mmcblk0", "/sys/devices/platform/mmc0/mmcblk0", "SD32G", "", MobileKindSD},
		{"sdb", "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host6/target6:0:0/6:0:0:0/block/sdb", "SD/MMC Card Reader", "", MobileKindSD},
		{"sdc", "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-3/1-3:1.0/host7/target7:0:0/7:0:0:0/block/sdc", "Ultra USB 3.0", "", MobileKindUSB},
		{"sdd", "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-4/1-4:1.0/host8/target8:0:0/8:0:0:0/block/sdd", "MassStorageClass", "05e3:0764", MobileKindSD},
		{"sde", "/sys/devices/pci0000:00/0000:00:14.0/usb1/1-5/1-5:1.0/host9/target9:0:0/9:0:0:0/block/sde", "ASM1153", "174c:1153", MobileKindUSB},
	}
	for _, item := range cases {
		got := mobileDeviceKind(item.name, item.resolved, item.model, item.usbID)
		if got != item.want {
			t.Fatalf("%s（%s / %s）：want %s, got %s", item.name, item.model, item.usbID, item.want, got)
		}
	}
}

func TestUSBDeviceNameFromPath(t *testing.T) {
	cases := map[string]string{
		"/sys/devices/pci0000:00/0000:00:0d.0/usb2/2-1/2-1.4/2-1.4:1.0/host32/target32:0:0/32:0:0:0/block/sdf": "2-1.4",
		"/sys/devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/host6/target6:0:0/6:0:0:0/block/sdb":            "1-2",
		"/sys/devices/platform/mmc0/mmcblk0": "",
	}
	for path, want := range cases {
		if got := usbDeviceNameFromPath(path); got != want {
			t.Fatalf("%s：want %q, got %q", path, want, got)
		}
	}
}

// 传感器默认显示名只补一次，而且只补「没有名字」的键：配置里已经有名字的
// （旧版本写过的内置默认、用户自己改的）首次升级也不能被默认值覆盖；旧内置名
// 的收敛（PCB温度/PCB → 主板PCB）单独走改名逻辑。补过之后清空或改名不再被
// 塞回来（sensor_name_defaults_applied 记住了）。
func TestDefaultSensorNamesAppliedOnce(t *testing.T) {
	cfg := Config{SensorNames: map[string]string{
		"it8613:temp2": "PCB",        // 旧内置名 → 由改名逻辑收敛成主板PCB
		"mlx5:sensor0": "万兆网卡1", // 用户自己起的：绝不能被默认值覆盖
		"acpitz:temp1": "机箱",
	}}
	if !normalizeConfig(&cfg) {
		t.Fatal("首次归一化应写入默认显示名")
	}
	if got := cfg.SensorNames["it8613:temp2"]; got != "主板PCB" {
		t.Fatalf("旧内置名应由改名逻辑收敛成主板PCB，got %q", got)
	}
	if got := cfg.SensorNames["mlx5:sensor0"]; got != "万兆网卡1" {
		t.Fatalf("用户已保存的名字不能被默认值覆盖，got %q", got)
	}
	if got := cfg.SensorNames["acpitz:temp1"]; got != "机箱" {
		t.Fatalf("默认表之外的键不能被改动，got %q", got)
	}
	if got := cfg.SensorNames["mlx5#2:Module1"]; got != "光模块2" {
		t.Fatalf("没有名字的键才补默认值，got %q", got)
	}
	if !cfg.SensorNameDefaultsApplied {
		t.Fatal("应记下已补过默认名")
	}

	// 用户清空 / 改名后：归一化不再覆盖
	cfg.SensorNames = map[string]string{}
	normalizeConfig(&cfg)
	if _, exists := cfg.SensorNames["mlx5:sensor0"]; exists {
		t.Fatal("补过一次之后，用户清空不应被重新写入")
	}
}

// 0.10.46：默认名早就固定过的老机器，只把仍叫旧名（「PCB温度」/ 用户早先手填的
// 「PCB」）的那一项改成「主板PCB」；用户自己起的其它名字一律不动，而且只做一次。
func TestSensorNamePCBRenamedOnce(t *testing.T) {
	for _, legacy := range []string{"PCB温度", "PCB"} {
		cfg := Config{
			SensorNameDefaultsApplied: true,
			SensorNames: map[string]string{
				"it8613:temp2": legacy,
				"acpitz:temp1": "机箱",
			},
		}
		if !normalizeConfig(&cfg) {
			t.Fatalf("%q：仍停在旧名时应当改名", legacy)
		}
		if got := cfg.SensorNames["it8613:temp2"]; got != sensorNameMainboard {
			t.Fatalf("%q：旧名应当改成主板PCB，got %q", legacy, got)
		}
		if got := cfg.SensorNames["acpitz:temp1"]; got != "机箱" {
			t.Fatalf("%q：用户改过的名字不能被覆盖，got %q", legacy, got)
		}
	}

	// 用户事后又把名字改回旧名，同样不该被再改一次
	cfg := Config{
		SensorNameDefaultsApplied: true,
		SensorNames:               map[string]string{"it8613:temp2": "主板PCB"},
	}
	normalizeConfig(&cfg)
	cfg.SensorNames["it8613:temp2"] = "PCB"
	normalizeConfig(&cfg)
	if got := cfg.SensorNames["it8613:temp2"]; got != "PCB" {
		t.Fatalf("改名只做一次，got %q", got)
	}
}
