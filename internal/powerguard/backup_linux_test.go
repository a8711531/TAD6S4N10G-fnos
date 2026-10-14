//go:build linux

package powerguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 归档模式依赖系统 rsync，只能在 Linux 上做真实端到端验证。

func backupWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func backupArchiveJob() GPIOBackupJob {
	return GPIOBackupJob{
		ID: "arc", Name: "归档测试", Mode: BackupModeArchive, Keep: 2,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}
}

func TestRunBackupJobArchiveUsesHardlinksAndKeepsCount(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过归档模式端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "alpha-0")
	backupWrite(t, filepath.Join(root, "src", "sub", "b.txt"), "beta")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := backupArchiveJob()

	var snapshots []string
	for index := 0; index < 3; index++ {
		// 每次让 a.txt 的长度也变化：rsync 的快速校验按「大小 + 修改时间」判断
		// 是否需要传输，同一秒内写入且长度相同的内容会被视为未变化而跳过。
		backupWrite(t, filepath.Join(root, "src", "a.txt"), strings.Repeat("x", index+1))
		result, err := manager.RunBackupJob(context.Background(), job, false)
		if err != nil {
			t.Fatalf("第 %d 次归档失败: %v", index, err)
		}
		if result.Files == 0 {
			t.Fatalf("第 %d 次归档没有复制任何文件", index)
		}
		snapshots = manager.snapshotEntries("/dst")
	}
	// keep=2：跑三次之后只应留下最近两份快照（这一项同时守住「清理误用快照
	// 目录而非目标根」的回归——传错目录时这里会剩 3 份）。
	if len(snapshots) != 2 {
		t.Fatalf("keep=2 应保留 2 份快照，实际 %d 份: %v", len(snapshots), snapshots)
	}

	// 最后两份快照之间，内容未变的 sub/b.txt 应是硬链接（同 inode）。
	first := snapshots[len(snapshots)-2]
	second := snapshots[len(snapshots)-1]
	unchangedA, err := os.Stat(filepath.Join(root, first, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	unchangedB, err := os.Stat(filepath.Join(root, second, "sub", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(unchangedA, unchangedB) {
		t.Fatal("内容未变化的文件没有复用硬链接，硬链接增量失效")
	}
	// 内容变化的 a.txt 必须是重新复制的独立文件。
	changedA, err := os.Stat(filepath.Join(root, first, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	changedB, err := os.Stat(filepath.Join(root, second, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(changedA, changedB) {
		t.Fatal("内容已变化的文件被错误地硬链接复用")
	}
}

// 标识文件必须写进源介质、并且不被复制到备份里；换了一块内容不同的介质时
// 必须拒绝执行，这正是「防止认错盘」的核心行为。
func TestRunBackupJobMarkerGuardsWrongMedium(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync 未安装，跳过标识文件端到端测试")
	}
	root := t.TempDir()
	backupWrite(t, filepath.Join(root, "src", "a.txt"), "alpha")
	if err := os.MkdirAll(filepath.Join(root, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{Root: root}
	job := GPIOBackupJob{
		ID: "marker", Name: "标识", Mode: BackupModeCopy,
		Source: BackupLocation{Kind: "path", Path: "/src"},
		Target: BackupLocation{Kind: "path", Path: "/dst"},
	}

	marker, written, err := manager.WriteBackupMarker(job)
	if err != nil {
		t.Fatalf("写入标识文件失败: %v", err)
	}
	if written != "/src/"+BackupMetaDirName+"/"+backupMarkerFileName {
		t.Fatalf("标识文件路径不正确: %q", written)
	}
	job.Marker = marker
	// 写新位置的同时要把旧版写在源根的 .tad-backup-source 清掉，别让两处标记并存。
	backupWrite(t, filepath.Join(root, "src", backupMarkerLegacyName), "old-marker")
	if _, _, err := manager.WriteBackupMarker(job); err != nil {
		t.Fatalf("重复写入标识文件失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src", backupMarkerLegacyName)); !os.IsNotExist(err) {
		t.Fatal("旧版标识文件应当被清理")
	}

	result, err := manager.RunBackupJob(context.Background(), job, false)
	if err != nil {
		t.Fatalf("带标识的备份应能跑通: %v", err)
	}
	if result.Files == 0 {
		t.Fatal("备份没有复制任何文件")
	}
	// 元数据目录（以及旧版两个标记名）都不该被复制进备份。
	for _, name := range []string{BackupMetaDirName, backupMarkerLegacyName, BackupAutoMarkerLegacyDirName} {
		if _, err := os.Stat(filepath.Join(root, "dst", name)); err == nil {
			t.Fatalf("元数据 %s 不该被复制进备份", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "dst", "a.txt")); err != nil {
		t.Fatalf("正常文件应被复制: %v", err)
	}

	// 换了一块盘：源目录里没有标识文件（或内容不同）时必须拒绝。
	wrong := t.TempDir()
	backupWrite(t, filepath.Join(wrong, "src", "b.txt"), "beta")
	if err := os.MkdirAll(filepath.Join(wrong, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	wrongManager := &Manager{Root: wrong}
	if _, err := wrongManager.RunBackupJob(context.Background(), job, false); err == nil {
		t.Fatal("缺少标识文件的介质应当被拒绝")
	}
	backupWrite(t, filepath.Join(wrong, "src", BackupMetaDirName, backupMarkerFileName), "someone-else-disk")
	if _, err := wrongManager.RunBackupJob(context.Background(), job, false); err == nil {
		t.Fatal("标识内容不一致的介质应当被拒绝")
	}

	// 旧版位置（源根的 .tad-backup-source）继续认，升级过来的盘不用重新标。
	legacy := t.TempDir()
	backupWrite(t, filepath.Join(legacy, "src", "c.txt"), "gamma")
	if err := os.MkdirAll(filepath.Join(legacy, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, filepath.Join(legacy, "src", backupMarkerLegacyName), marker)
	legacyManager := &Manager{Root: legacy}
	if _, err := legacyManager.RunBackupJob(context.Background(), job, false); err != nil {
		t.Fatalf("旧版位置的标识文件应当继续通过校验: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "dst", backupMarkerLegacyName)); err == nil {
		t.Fatal("旧版标识文件同样不该被复制进备份")
	}
}
