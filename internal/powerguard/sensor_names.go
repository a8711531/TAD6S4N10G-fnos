package powerguard

// sensor_names.go：传感器显示名的内置默认值。
//
// 这台机器的 hwmon 标签本身没有语义（it8613:temp2、mlx5#2:Module1），历史温度
// 图例里全是一串芯片名。把这些常见位置的中文名固定写进配置，用户打开调试页
// 看到的就是「PCB温度 / 光模块2」，需要时仍可自行改名。

// defaultSensorNames 是键 → 默认显示名。键与 /api/history 的 sensors[].key 一致。
var defaultSensorNames = map[string]string{
	"it8613:temp1":   "BIOS虚拟温度",
	"it8613:temp2":   "主板PCB",
	"it8613:temp3":   "BIOS虚拟温度",
	"mlx5:sensor0":   "CX-4 LX#1",
	"mlx5#2:sensor0": "CX-4 LX#2",
	"mlx5:Module1":   "光模块1",
	"mlx5#2:Module1": "光模块2",
}

// 0.10.46 的默认名调整：主板 PCB 的探针从「PCB温度」改叫「主板PCB」。
// 旧值有两种：0.10.45 定的「PCB温度」，以及更早用户手填的「PCB」——都算旧名。
const sensorNameMainboard = "主板PCB"

var legacySensorNamePCB = map[string]bool{
	"PCB温度": true,
	"PCB":   true,
}

// applyDefaultSensorNames 只在第一次（sensor_name_defaults_applied 为 false）把
// 默认名写进配置：这一次是「固定保存」，同名键会被默认值覆盖（用户明确要求
// 主板PCB / 光模块2 这些名字）；之后再改名或清空都不受这里影响。
func applyDefaultSensorNames(cfg *Config) bool {
	if cfg.SensorNameDefaultsApplied {
		return false
	}
	cfg.SensorNameDefaultsApplied = true
	if cfg.SensorNames == nil {
		cfg.SensorNames = make(map[string]string, len(defaultSensorNames))
	}
	for key, name := range defaultSensorNames {
		if cfg.SensorNames[key] == name {
			continue
		}
		cfg.SensorNames[key] = name
	}
	return true
}

// applySensorNameRenames 做 0.10.46 的一次性重命名：默认名早已按 v0.10.45 固定过
// 的机器不会重跑 applyDefaultSensorNames，所以单独把仍叫旧名（「PCB温度」/「PCB」）
// 的那一项改成「主板PCB」。用户自己起的其它名字一律不动（那才是他的意思）。
func applySensorNameRenames(cfg *Config) bool {
	if cfg.SensorNamePCBRenamed {
		return false
	}
	cfg.SensorNamePCBRenamed = true
	if cfg.SensorNames == nil {
		return false
	}
	if !legacySensorNamePCB[cfg.SensorNames["it8613:temp2"]] {
		return false
	}
	cfg.SensorNames["it8613:temp2"] = sensorNameMainboard
	return true
}
