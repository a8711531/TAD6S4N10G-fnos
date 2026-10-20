package powerguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 位置列表里的文字从硬盘测试工具（fnos-diskbench）一并移植过来：同一台机器、
// 同一批挂载，两个套件在界面上应该显示同样的名字与容量，否则用户会以为读错了。
var (
	backupVolNumRe    = regexp.MustCompile(`^/vol([0-9]+)$`)
	backupDsmVolNumRe = regexp.MustCompile(`^/volume([0-9]+)$`)
	// backupNumericDirRe 匹配飞牛的用户目录名（卷根下纯数字，如 1000）。
	backupNumericDirRe = regexp.MustCompile(`^[0-9]+$`)
)

// backupVolumeLabel 给内置卷一个可读名字：飞牛「存储空间 N」、群晖「存储空间 N」、
// Unraid 的联合视图/缓存池各有叫法，其余退回目录名。
func backupVolumeLabel(root string) string {
	root = filepath.ToSlash(root)
	switch {
	case backupVolNumRe.MatchString(root):
		return "存储空间 " + backupVolNumRe.FindStringSubmatch(root)[1]
	case backupDsmVolNumRe.MatchString(root):
		return "存储空间 " + backupDsmVolNumRe.FindStringSubmatch(root)[1]
	case root == "/mnt/user":
		return "Unraid 共享"
	case root == "/mnt/cache":
		return "Unraid 缓存池"
	case strings.HasPrefix(root, "/mnt/disk"):
		return "Unraid 磁盘 " + strings.TrimPrefix(root, "/mnt/disk")
	default:
		base := filepath.Base(root)
		if base == "" || base == "." || base == "/" {
			return root
		}
		return base
	}
}

// backupRemoteSub 推导远程挂载的第二行：SMB 是「登录名@地址」，NFS 是服务器地址，
// 网盘没有这些字段（由调用方换成账号名）。
func backupRemoteSub(mount mountRecord) string {
	kv := map[string]string{}
	for _, part := range strings.Split(mount.options, ",") {
		if i := strings.IndexByte(part, '='); i > 0 {
			kv[strings.TrimSpace(part[:i])] = strings.TrimSpace(part[i+1:])
		}
	}
	user, addr := kv["username"], kv["addr"]
	if user == "" {
		user = kv["user"]
	}
	switch {
	case user != "" && addr != "":
		return user + "@" + addr
	case addr != "":
		return addr
	case strings.HasPrefix(mount.fsType, "nfs") && mount.source != "":
		host := mount.source
		if i := strings.IndexByte(host, ':'); i > 0 {
			host = host[:i]
		}
		return host
	}
	return ""
}

// backupRemoteFallback 在配置里查不到名字时的兜底：SMB 用共享名
// （//host/share → share），其余退回「远程挂载 · <挂载目录名>」。
func backupRemoteFallback(source, mount string) string {
	if strings.HasPrefix(source, "//") {
		if i := strings.LastIndexByte(source, '/'); i > 0 && i < len(source)-1 {
			return source[i+1:]
		}
	}
	return "远程挂载 · " + filepath.Base(mount)
}

// backupRemoteDisplay 给出远程挂载的两行文字：
// 第一行 = 用户手动改的名字 → 网盘品牌 → 官方名字 → rclone 段名 → 兜底；
// 第二行 = 账号名（网盘）或 登录名@地址（SMB/NFS）。
func backupRemoteDisplay(mount mountRecord, aliases, friendly, names, brands map[string]string) (string, string) {
	base := filepath.Base(mount.mountPoint)
	sub := backupRemoteSub(mount)
	name := lookupRemoteName(names, base)
	brand := lookupRemoteName(brands, base)
	// 网盘没有 SMB/NFS 那种 user@addr 挂载参数；配置里的 comment 存的是账号名，
	// SMB/NFS 已取到地址时优先保留地址。
	if sub == "" {
		sub = name
	}
	if alias := strings.TrimSpace(aliases[mount.mountPoint]); alias != "" {
		// 用户在备份设置里改过名：第一行听用户的，第二行照常给账号/地址
		return alias, sub
	}
	switch {
	case brand != "" && name != "" && brand != name:
		// 第一行网盘品牌，第二行用户账号
		return brand, sub
	case name != "":
		return name, sub
	case strings.TrimSpace(friendly[mount.mountPoint]) != "":
		return strings.TrimSpace(friendly[mount.mountPoint]), sub
	}
	return backupRemoteFallback(mount.source, mount.mountPoint), sub
}

// backupUsage 把容量读成界面用的字节数；读不到就留 0（前端不显示容量）。
func (m *Manager) backupUsage(path string) (total, free uint64) {
	total, free, err := filesystemUsage(m.rooted(path))
	if err != nil {
		return 0, 0
	}
	return total, free
}

// backupPlaceSubtitle 拼第二行的身份信息，空片段自动省略。
func backupPlaceSubtitle(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, " · ")
}

// backupReadOnlyText 只读介质的提示（非只读返回空串，条目就只占一行）。
func backupReadOnlyText(readOnly bool) string {
	if readOnly {
		return "只读"
	}
	return ""
}

// backupExternalDeviceName 外接存储的设备名：优先卷标，缺失时退回挂载目录名。
func backupExternalDeviceName(label, mountPoint string) string {
	name := strings.TrimSpace(label)
	if name == "" {
		name = filepath.Base(mountPoint)
	}
	if name == "" {
		name = "未知设备"
	}
	return name
}

// backupVolumeHome 找出卷根下的用户目录（编号最小的纯数字目录，如 /vol1/1000）。
// 与硬盘测试工具一致：点「存储空间 N」直接进用户目录，不停在卷根。
func (m *Manager) backupVolumeHome(root string) string {
	entries, err := os.ReadDir(m.rooted(root))
	if err != nil {
		return ""
	}
	best := -1
	found := ""
	for _, entry := range entries {
		if !entry.IsDir() || !backupNumericDirRe.MatchString(entry.Name()) {
			continue
		}
		n, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if found == "" || n < best {
			best, found = n, filepath.ToSlash(filepath.Join(root, entry.Name()))
		}
	}
	return found
}
