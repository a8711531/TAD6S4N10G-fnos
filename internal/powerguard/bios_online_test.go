package powerguard

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 与 BIOS-FID-Tool 生成的 $FID 布局完全一致的合成镜像：
// $FID + 3 字节 + 8 字节主板 ID + 16 字节 + 4 组"dd\0"版本 + 6 字节日期。
func buildFIDImage(boardID string, version [4]string, year, month, day, hour, minute int) []byte {
	image := make([]byte, 256)
	copy(image[0:], []byte("$FID"))
	image[4], image[5], image[6] = 0x04, 0x78, 0x00
	copy(image[7:15], boardID)
	p := 7 + 8 + 16 // 31
	for g := 0; g < 4; g++ {
		image[p+g*3] = version[g][0]
		image[p+g*3+1] = version[g][1]
		image[p+g*3+2] = 0
	}
	dp := p + 12
	image[dp] = byte(year & 0xff)
	image[dp+1] = byte(year >> 8)
	image[dp+2] = byte(month)
	image[dp+3] = byte(day)
	image[dp+4] = byte(hour)
	image[dp+5] = byte(minute)
	return image
}

func TestParseBIOSFID(t *testing.T) {
	image := buildFIDImage("TADSN410", [4]string{"05", "27", "04", "10"}, 2026, 6, 24, 22, 23)
	fid, ok := parseBIOSFID(image)
	if !ok {
		t.Fatal("应能解析 $FID")
	}
	if fid.BoardID != "TADSN410" {
		t.Fatalf("主板 ID 错误: %q", fid.BoardID)
	}
	if fid.Version != "05.27.04.10" {
		t.Fatalf("版本号错误: %q", fid.Version)
	}
	if fid.BuildText != "2026-06-24 22:23" {
		t.Fatalf("构建日期错误: %q", fid.BuildText)
	}
	if _, ok := parseBIOSFID(make([]byte, 256)); ok {
		t.Fatal("无 $FID 的镜像不应解析成功")
	}
}

const biosIndexFixture = `BIOS说明
2026_06_25
1.修复对 4K 显示器兼容性差问题
2.新增 win 下更新工具
============================================================
BIOS文件解析
压缩包: TAD6S4N10G-N305-2026_06_25.zip
提取时间: 2026-10-23 00:32:39

固件文件: TADSN3V410.bin
主板ID:   TADSN410
版本号:   05.27.04.10
构建日期: 2026-06-24 22:23
文件大小: 16777216 字节
SHA256:   C0570702EDD622159308BEACE384D5796DD422CCD7AC01D4C4F181C61FEBC1C6

[upgrade]
file=TADSN3V410.bin
board_id=TADSN410
version=05.27.04.10
build_date=2026-06-24 22:23
size=16777216
sha256=C0570702EDD622159308BEACE384D5796DD422CCD7AC01D4C4F181C61FEBC1C6

============================================================
BIOS文件解析
压缩包: TAD6S4N10G-N150-2026_06_25.zip
提取时间: 2026-10-23 00:32:48

[upgrade]
file=TADSN15V410.bin
board_id=TADSN410
version=05.27.04.10
build_date=2026-06-24 22:50
size=16777216
sha256=DF0F4ADCF15578B840145AE97575AB8AB234A79A7F783F002F2795B483FDEEA1

============================================================
BIOS文件解析
压缩包: TAD6S4N10G-N100-2026_06_25.zip
提取时间: 2026-10-23 00:32:54

[upgrade]
file=TADSN1V510.bin
board_id=TADSN510
version=05.27.05.10
build_date=2026-06-24 21:55
size=16777216
sha256=A18642714DBE77615522BF5401179F6B8EEF31BA67D54BCC6465C028677A1798
`

func TestParseBIOSIndex(t *testing.T) {
	records, changelogs := parseBIOSIndex(biosIndexFixture)
	if len(records) != 3 {
		t.Fatalf("应解析出 3 条固件记录，实际 %d", len(records))
	}
	n305 := records[0]
	if n305.ZipName != "TAD6S4N10G-N305-2026_06_25.zip" || n305.File != "TADSN3V410.bin" {
		t.Fatalf("N305 记录解析错误: %+v", n305)
	}
	if n305.BuildText != "2026-06-24 22:23" || n305.BuildDate.IsZero() {
		t.Fatalf("构建日期解析错误: %+v", n305)
	}
	if len(changelogs) != 1 {
		t.Fatalf("应解析出 1 条更新日志，实际 %d", len(changelogs))
	}
	if changelogs[0].Date != "2026_06_25" {
		t.Fatalf("更新日志日期错误: %q", changelogs[0].Date)
	}
	if !strings.Contains(changelogs[0].Text, "4K") || !strings.Contains(changelogs[0].Text, "win") {
		t.Fatalf("更新日志正文缺失: %q", changelogs[0].Text)
	}
	if strings.Contains(changelogs[0].Text, "BIOS说明") {
		t.Fatalf("更新日志不应包含标题行: %q", changelogs[0].Text)
	}
	// Windows 工具写出的 zip.txt 带 UTF-8 BOM，解析必须剥掉，否则会混进首条日志标题
	withBOM, bomChangelogs := parseBIOSIndex("\uFEFF" + biosIndexFixture)
	if len(withBOM) != 3 || len(bomChangelogs) != 1 {
		t.Fatalf("带 BOM 的索引解析条数错误: records=%d changelogs=%d", len(withBOM), len(bomChangelogs))
	}
	if strings.ContainsRune(bomChangelogs[0].Text, '\uFEFF') {
		t.Fatalf("BOM 不得混入更新日志: %q", bomChangelogs[0].Text)
	}
}

func TestDetectCPUModel(t *testing.T) {
	cases := map[string]string{
		"model name\t: Intel(R) N100":             "N100",
		"model name\t: Intel(R) Core(TM) i3-N305": "N305",
		"model name\t: Intel(R) N150":             "N150",
		"model name\t: Intel(R) Celeron(R) J4125": "",
	}
	for input, want := range cases {
		if got := detectCPUModel(input); got != want {
			t.Fatalf("detectCPUModel(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestBIOSZipModel(t *testing.T) {
	if got := biosZipModel("TAD6S4N10G-N305-2026_06_25.zip"); got != "N305" {
		t.Fatalf("机型段提取错误: %q", got)
	}
	if got := biosZipModel("TAD6S4N10G-N00-2026_06_25.zip"); got != "" {
		t.Fatalf("无机型段应为空: %q", got)
	}
}

// TestCheckBIOSOnline 全链路：索引下载注入 + 当前 flash 注入 + cpuinfo 注入。
func TestCheckBIOSOnline(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: root + "/state.json", Root: root}
	writeTestFile(t, root+"/proc/cpuinfo", "model name\t: Intel(R) N100\n")

	// 当前 flash 是 N100 旧版（V4.10，比索引里的 V5.10 旧一天）
	current := buildFIDImage("TADSN410", [4]string{"05", "27", "04", "10"}, 2026, 6, 23, 10, 0)
	biosFIDReadHook = func(ctx context.Context, m *Manager) ([]byte, error) { return current, nil }
	defer func() { biosFIDReadHook = nil }()
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		if address != biosIndexURL() {
			t.Fatalf("索引地址错误: %s", address)
		}
		return []byte(biosIndexFixture), nil
	}
	defer func() { firmwareDownloadHook = nil }()

	status := manager.CheckBIOSOnline(context.Background())
	if status.Error != "" {
		t.Fatalf("检查不应失败: %s", status.Error)
	}
	if status.Model != "N100" {
		t.Fatalf("机型识别错误: %q", status.Model)
	}
	// 必须选 N100 的包而不是 N305/N150 的（严格同机型）。
	if status.CandidateZip != "TAD6S4N10G-N100-2026_06_25.zip" {
		t.Fatalf("候选包选择错误: %q", status.CandidateZip)
	}
	if !status.UpdateAvailable {
		t.Fatal("索引构建日期更新，应判定可升级")
	}
	if status.CurrentVersion != "05.27.04.10" || status.CandidateVersion != "05.27.05.10" {
		t.Fatalf("版本展示错误: 当前 %s 候选 %s", status.CurrentVersion, status.CandidateVersion)
	}
	// 更新日志只显示日期与压缩包名匹配的段落。
	if !strings.Contains(status.Changelog, "4K") {
		t.Fatalf("更新日志缺失: %q", status.Changelog)
	}
	if !strings.Contains(status.DownloadURL, "TAD6S4N10G-N100-2026_06_25.zip") {
		t.Fatalf("下载地址错误: %s", status.DownloadURL)
	}

	// 当前已是新版（构建日期相同）→ 不可升级。
	newer := buildFIDImage("TADSN510", [4]string{"05", "27", "05", "10"}, 2026, 6, 24, 21, 55)
	biosFIDReadHook = func(ctx context.Context, m *Manager) ([]byte, error) { return newer, nil }
	status = manager.CheckBIOSOnline(context.Background())
	if status.UpdateAvailable {
		t.Fatal("构建日期相同不应提示升级")
	}
	if err := manager.StartBIOSOnlineFlash(); err == nil {
		t.Fatal("无可升级版本时应拒绝启动刷写")
	}
}

// TestCheckBIOSOnlineRejectsCrossModel 机型在索引中不存在时不得回退到其他机型。
func TestCheckBIOSOnlineRejectsCrossModel(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: root + "/state.json", Root: root}
	writeTestFile(t, root+"/proc/cpuinfo", "model name\t: Intel(R) Celeron(R) J4125\n")
	biosFIDReadHook = func(ctx context.Context, m *Manager) ([]byte, error) {
		return buildFIDImage("TADSN410", [4]string{"05", "27", "04", "10"}, 2026, 6, 23, 10, 0), nil
	}
	defer func() { biosFIDReadHook = nil }()
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		return []byte(biosIndexFixture), nil
	}
	defer func() { firmwareDownloadHook = nil }()

	status := manager.CheckBIOSOnline(context.Background())
	if status.Error == "" {
		t.Fatal("无法识别机型时必须报错并停止判定")
	}
	if status.CandidateZip != "" || status.UpdateAvailable {
		t.Fatalf("未识别机型不得给出候选包: %+v", status)
	}
}

func TestExtractBIOSFromZip(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("TAD6S4N10G-N100-2026_06_25/TADSN1V510.bin")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("fake-bios-image")
	if _, err := entry.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	image, err := extractBIOSFromZip(buffer.Bytes(), "TADSN1V510.bin")
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if !bytes.Equal(image, payload) {
		t.Fatalf("解包内容不一致: %q", image)
	}
	if _, err := extractBIOSFromZip(buffer.Bytes(), "not-exist.bin"); err == nil {
		t.Fatal("找不到文件时必须报错")
	}
}

// TestRunBIOSOnlineFlashVerifiesSHA256 下载内容与索引 SHA256 不一致时必须停止。
func TestRunBIOSOnlineFlashVerifiesSHA256(t *testing.T) {
	root := t.TempDir()
	manager := &Manager{StatePath: root + "/state.json", Root: root}
	writeTestFile(t, root+"/proc/cpuinfo", "model name\t: Intel(R) N100\n")

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, _ := writer.Create("pkg/TADSN1V510.bin")
	_, _ = entry.Write([]byte("tampered-image"))
	_ = writer.Close()
	firmwareDownloadHook = func(ctx context.Context, address string) ([]byte, error) {
		return buffer.Bytes(), nil
	}
	defer func() { firmwareDownloadHook = nil }()

	if err := manager.beginFirmwareJob(firmwareJobKindBIOS); err != nil {
		t.Fatal(err)
	}
	manager.runBIOSOnlineFlash(biosIndexRecord{
		ZipName: "TAD6S4N10G-N100-2026_06_25.zip",
		File:    "TADSN1V510.bin",
		SHA256:  "0000000000000000000000000000000000000000000000000000000000000000",
	})
	status := manager.FirmwareStatus()
	if status.Job.State != firmwareJobFailed {
		t.Fatalf("SHA256 不一致必须失败，实际 %s", status.Job.State)
	}
	if !strings.Contains(status.Job.Error, "SHA256") {
		t.Fatalf("失败原因应说明 SHA256: %q", status.Job.Error)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
