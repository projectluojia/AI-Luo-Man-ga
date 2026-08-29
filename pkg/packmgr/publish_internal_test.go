package packmgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
)

func TestPublishStageRestoresOldDirectoryWhenPublishRenameFails(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "demo.pkg")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(target, "old.txt")
	if err := os.WriteFile(old, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, ".stage-missing")
	if _, err := publishStage(context.Background(), root, target, stage); err == nil {
		t.Fatal("publishStage succeeded for missing stage")
	}
	content, err := os.ReadFile(old)
	if err != nil {
		t.Fatalf("old installation was not restored: %v", err)
	}
	if string(content) != "old" {
		t.Fatalf("old installation content = %q", content)
	}
}

// publishStage 的失败必须可回滚：旧安装先移到备份目录，rename 或发布后回读失败都
// 恢复原安装。白盒测试直接喂一个无效阶段目录，这是"发布后回读失败"的唯一可控触发
// 点——正常路径下 Install 已在源阶段拦掉所有非法输入。
func TestPublishStageRestoresPreviousInstallOnVerifyFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "app.wasm"), []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":"` + packagecontract.SchemaVersion + `","id":"demo.pkg","version":"1.0.0",` +
		`"components":[{"id":"core","mode":"hosted","entrypoint":"app.wasm"}]}`
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(ctx, root, source); err != nil {
		t.Fatalf("Install: %v", err)
	}
	targetDir := filepath.Join(root, "demo.pkg")

	// 阶段目录只有一个垃圾文件：rename 会成功，回读必然失败。
	stageDir, err := os.MkdirTemp(root, stagePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "junk"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := publishStage(ctx, root, targetDir, stageDir); err == nil {
		t.Fatal("publishStage with invalid stage = nil, want error")
	}
	// 原安装必须完好，且不留备份/阶段目录残骸（阶段目录已被 rename 走并删除）。
	record, err := ReadInstalled(ctx, targetDir)
	if err != nil {
		t.Fatalf("回滚后原安装不可读: %v", err)
	}
	if record.Manifest.Version != "1.0.0" {
		t.Fatalf("回滚后版本 = %s, want 1.0.0", record.Manifest.Version)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "demo.pkg" {
		t.Fatalf("安装根条目 = %+v, want 仅 demo.pkg", entries)
	}
}

func TestListInstalledRecoversInterruptedPublication(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "app.wasm"), []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":"` + packagecontract.SchemaVersion + `","id":"demo.pkg","version":"1.0.0",` +
		`"components":[{"id":"core","mode":"hosted","entrypoint":"app.wasm"}]}`
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(ctx, root, source); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := reserveBackupDir(root, filepath.Join(root, "demo.pkg")); err != nil {
		t.Fatalf("reserveBackupDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "demo.pkg")); !os.IsNotExist(err) {
		t.Fatalf("canonical package still exists: %v", err)
	}
	records, err := ListInstalled(ctx, root)
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(records) != 1 || records[0].Manifest.ID != "demo.pkg" {
		t.Fatalf("recovered records = %#v", records)
	}
}

func TestPublishStageRestoresPreviousInstallOnRenameFailure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "pkg")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "app.wasm"), []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema_version":"` + packagecontract.SchemaVersion + `","id":"demo.pkg","version":"1.0.0",` +
		`"components":[{"id":"core","mode":"hosted","entrypoint":"app.wasm"}]}`
	if err := os.WriteFile(filepath.Join(source, "manifest.json"), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(context.Background(), root, source); err != nil {
		t.Fatalf("Install: %v", err)
	}
	targetDir := filepath.Join(root, "demo.pkg")
	if _, err := publishStage(context.Background(), root, targetDir, filepath.Join(root, stagePrefix+"missing")); err == nil {
		t.Fatal("publishStage with missing stage = nil, want error")
	}
	if record, err := ReadInstalled(context.Background(), targetDir); err != nil || record.Manifest.Version != "1.0.0" {
		t.Fatalf("rollback record=%#v err=%v, want original install", record, err)
	}
}
