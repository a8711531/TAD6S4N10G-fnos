// 从硬盘测试工具（fnos-diskbench）移植：飞牛把远程挂载（网盘 / NFS / SMB）的
// 官方显示名存在 root-only 配置里，备份弹窗的位置列表要和它显示同一套文字。
package powerguard

// 飞牛把远程挂载（网盘 / NFS / SMB）的官方显示名存在 root-only 的配置里：
//
//	/etc/mountmgr/rclone/<uid>.conf    rclone 远端配置（INI，[section] 名就是挂载 id）
//	/usr/trim/etc/cloud_storage_dav/   云盘 DAV 服务配置（JSON / INI，格式无公开文档）
//
// diskbench 装机后以 root 运行可以直接读；这里做通用解析兜底：
// 能认出 INI section 与 JSON 对象里的 name 类字段，就把官方名字取回来；
// 认不出就退回「远程挂载 · <id>」，继续靠前端手动改名。
// 本地调试（非 root）读不了这些目录 → 静默返回空表，行为与从前一致。

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// remoteConfigDirs 是可能存放远程挂载名的配置目录。测试里会替换成临时目录。
var remoteConfigDirs = []string{
	"/etc/mountmgr",                   // mountmgr：rclone 远端配置（下一层 rclone/<uid>.conf）
	"/usr/trim/etc/cloud_storage_dav", // 云盘 DAV 服务配置
}

// remoteNameKeys 是各配置里可能表示「显示名」的字段，按优先级排列（比较时转小写）。
// comment / cloudStorageTypeStr 是飞牛 mount_info.json 里的实际字段：
// comment 是用户给挂载起的名字（文件管理器显示的就是它），
// cloudStorageTypeStr 是网盘类型名（夸克网盘/百度网盘/…），comment 为空时兜底。
var remoteNameKeys = []string{
	"name", "alias", "title", "display_name", "displayname",
	"label", "remark", "mount_name", "mountname", "server_name", "nickname",
	"comment", "cloudstoragetypestr", "desc", "description",
}

// remoteIDKeys 是 JSON 对象里可能表示「挂载 id」的字段。
var remoteIDKeys = []string{
	"id", "uuid", "mount_id", "mountid", "remote_id", "remoteid", "mount_point_id",
}

const (
	remoteTextLimit   = 512 << 10 // 参与解析的单文件大小上限
	remoteScanLimit   = 4 << 20   // 二进制粗扫的单文件大小上限
	remoteWalkDepth   = 3         // 配置目录递归层数（/etc/mountmgr/rclone/x.conf 正好两层）
	nameValueMaxRunes = 64        // 显示名最长字符数，防止抓到一整段乱码
)

// crudeNameRe 用于对未知格式（如 sqlite 内嵌 JSON）做粗扫：找 name 类字段，
// 再在其邻近窗口里找 id 字段配对。
var crudeNameRe = regexp.MustCompile(
	`"(?:name|alias|title|display_name|displayName|remark|mount_name)"\s*:\s*"([^"\n]{1,64})"`)

// crudeIDFieldRe 在粗扫窗口里找 id 字段的值。
var crudeIDFieldRe = regexp.MustCompile(
	`"(?:mount_?id|mountid|remote_?id|remoteid|uuid|id)"\s*:\s*"([0-9A-Za-z][0-9A-Za-z._-]{7,63})"`)

// remoteInfos 同时返回「挂载 id → 显示名」与「挂载 id → 网盘品牌」。
// 显示名是用户给挂载起的名字（comment），品牌是网盘类型名
// （cloudStorageTypeStr，夸克网盘/百度网盘/…）。界面上品牌在前、
// 用户名做第二行小字，两个信息分开存。
func remoteInfos() (names, brands map[string]string) {
	names = map[string]string{}
	brands = map[string]string{}
	for _, dir := range remoteConfigDirs {
		walkRemoteConfigDir(dir, remoteWalkDepth, names, brands)
	}
	return names, brands
}

func walkRemoteConfigDir(dir string, depth int, m, brands map[string]string) {
	if depth <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // 权限不足（本地调试）或目录不存在：静默跳过
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			walkRemoteConfigDir(p, depth-1, m, brands)
			continue
		}
		parseRemoteConfigFile(p, m, brands)
	}
}

func parseRemoteConfigFile(path string, m, brands map[string]string) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	limit := int64(remoteTextLimit)
	if isBinaryName(path) {
		limit = remoteScanLimit
	}
	if st.Size() > limit {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	fileBase := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	trimmed := strings.TrimSpace(string(data))

	// JSON 配置：树里找 id + name 成对出现的对象
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		if json.Unmarshal(data, &v) == nil {
			jsonCollectNames(v, fileBase, m, brands)
			return
		}
	}
	// INI 配置（rclone conf 等）：[section] 是挂载 id
	if iniCollectNames(string(data), m, brands) {
		return
	}
	// 未知格式（sqlite / 自定义）：按 id 出现位置粗扫邻近的 name 字段
	scanTextForName(string(data), fileBase, m, brands)
}

// isBinaryName 按扩展名判断是否按二进制粗扫处理。
func isBinaryName(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".db", ".db3", ".sqlite", ".sqlite3", ".bin":
		return true
	}
	return false
}

// iniCollectNames 解析 INI 文本，把 [section] + name 类键值收进表里。
// 返回是否有任何一条命中。
func iniCollectNames(text string, m, brands map[string]string) bool {
	added := false
	section := ""
	kv := map[string]string{}
	flush := func() {
		if section == "" {
			return
		}
		if name := pickNameFromKV(kv); name != "" {
			if _, dup := m[section]; !dup {
				m[section] = name
				added = true
			}
			if b := validName(kv["cloudstoragetypestr"]); b != "" {
				if _, dup := brands[section]; !dup {
					brands[section] = b
				}
			}
		}
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			section = strings.TrimSpace(line[1 : len(line)-1])
			kv = map[string]string{}
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			k := strings.ToLower(strings.TrimSpace(line[:i]))
			v := strings.Trim(strings.TrimSpace(line[i+1:]), `"'`)
			kv[k] = v
		}
	}
	flush()
	return added
}

// pickNameFromKV 按优先级从键值对里挑显示名。
func pickNameFromKV(kv map[string]string) string {
	for _, key := range remoteNameKeys {
		v, ok := kv[key]
		if !ok {
			continue
		}
		if name := validName(v); name != "" {
			return name
		}
	}
	return ""
}

// jsonCollectNames 递归遍历 JSON 树，收集「id 字段 + name 字段」成对出现的对象。
// 根对象没有 id 字段但文件名本身是 id 时，用文件名兜底。
func jsonCollectNames(v any, fileBase string, m, brands map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		// 飞牛 mount_info.json 风格：挂载 id 是父对象的键，名字在子对象里
		//（{"1000-1-2056118d": {"comment": "夸父9660", ...}}）
		for k, val := range t {
			if sub, ok := val.(map[string]any); ok && looksLikeID(k) {
				if name := pickNameFromAny(sub); name != "" {
					if _, dup := m[k]; !dup {
						m[k] = name
						if b, ok := sub["cloudStorageTypeStr"].(string); ok {
							if brand := validName(b); brand != "" {
								brands[k] = brand
							}
						}
					}
				}
			}
		}
		id := ""
		for _, k := range remoteIDKeys {
			if s, ok := t[k].(string); ok && looksLikeID(s) {
				id = s
				break
			}
		}
		if name := pickNameFromAny(t); name != "" {
			target := id
			if target == "" && looksLikeID(fileBase) {
				target = fileBase
			}
			if target != "" {
				if _, dup := m[target]; !dup {
					m[target] = name
					if b, ok := t["cloudStorageTypeStr"].(string); ok {
						if brand := validName(b); brand != "" {
							brands[target] = brand
						}
					}
				}
			}
		}
		for _, val := range t {
			jsonCollectNames(val, fileBase, m, brands)
		}
	case []any:
		for _, val := range t {
			jsonCollectNames(val, fileBase, m, brands)
		}
	}
}

// pickNameFromAny 按优先级从 JSON 对象里挑显示名（键比较转小写）。
func pickNameFromAny(obj map[string]any) string {
	for _, key := range remoteNameKeys {
		for k, val := range obj {
			if !strings.EqualFold(k, key) {
				continue
			}
			s, ok := val.(string)
			if !ok {
				continue
			}
			if name := validName(s); name != "" {
				return name
			}
		}
	}
	return ""
}

// scanTextForName 对未知格式做粗扫：每个 name 类字段出现的位置，
// 在邻近窗口里找 id 字段（"mount_id":"..." 之类）配对成表项。
func scanTextForName(text, fileBase string, m, brands map[string]string) {
	for _, loc := range crudeNameRe.FindAllStringSubmatchIndex(text, -1) {
		name := validName(text[loc[2]:loc[3]])
		if name == "" {
			continue
		}
		lo := loc[0] - 200
		if lo < 0 {
			lo = 0
		}
		hi := loc[1] + 200
		if hi > len(text) {
			hi = len(text)
		}
		window := text[lo:hi]
		if mm := crudeIDFieldRe.FindStringSubmatch(window); mm != nil {
			if id := strings.TrimSpace(mm[1]); id != "" {
				if _, dup := m[id]; !dup {
					m[id] = name
				}
				continue
			}
		}
		// 窗口里没有 id 字段：退一步看文件名本身是不是挂载 id
		for _, tok := range idTokens(fileBase) {
			if strings.Contains(window, tok) {
				if _, dup := m[tok]; !dup {
					m[tok] = name
				}
				break
			}
		}
	}
}

// idTokens 给出可用于匹配的 id 片段：完整 base + 长度 ≥8 的 '-' 分段
// （1000-1-2056118d → [1000-1-2056118d 2056118d]）。
func idTokens(base string) []string {
	if base == "" {
		return nil
	}
	tokens := []string{base}
	for _, seg := range strings.Split(base, "-") {
		if len(seg) >= 8 && seg != base {
			tokens = append(tokens, seg)
		}
	}
	return tokens
}

// looksLikeID 判断一个字符串像不像挂载 id：8~64 字符、无空白、
// 只含字母数字和 ._-。
func looksLikeID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// validName 清洗显示名：去空白、去控制字符、限长、要求合法 UTF-8。
func validName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	s = b.String()
	runes := []rune(s)
	if len(runes) > nameValueMaxRunes {
		s = string(runes[:nameValueMaxRunes])
	}
	return s
}

// lookupRemoteName 从名字表里查挂载点的显示名：
// 先精确匹配挂载点目录名，再允许 id 片段互相包含（配置里可能只写 uuid 段）。
func lookupRemoteName(m map[string]string, base string) string {
	if name, ok := m[base]; ok {
		return name
	}
	for k, v := range m {
		if matchID(k, base) {
			return v
		}
	}
	return ""
}

// matchID 判断两个 id 片段是否指同一挂载：相等，或其中较长者在另一个里出现
// 且长度 ≥8（避免 "1000" 这类短段误配）。
func matchID(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) >= 8 && strings.Contains(b, a) {
		return true
	}
	if len(b) >= 8 && strings.Contains(a, b) {
		return true
	}
	return false
}
