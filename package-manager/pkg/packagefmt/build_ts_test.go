package packagefmt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
)

// TestBuildAssemblyScript 验证 ts-as 构建器：AssemblyScript guest 编译为 wasm
// 工件。仅在 npx 不可用时跳过；工具链存在但构建失败必须让测试失败。
func TestBuildAssemblyScript(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx 不可用，跳过 ts-as 构建测试")
	}
	dir := t.TempDir()
	source := `export function hello(name: string): string {
  return "hello, " + name;
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "ts.pkg", Version: "0.1.0",
		Components: []packagecontract.Component{{ID: "main", Mode: packagecontract.ModeHosted, Entrypoint: "main.wasm"}},
	}
	if err := Build(context.Background(), dir, manifest, BuildSpec{Tool: BuildToolAssemblyScript}); err != nil {
		t.Fatalf("AssemblyScript 编译失败: %v", err)
	}
	artifact, err := os.Stat(filepath.Join(dir, "main.wasm"))
	if err != nil {
		t.Fatalf("main.wasm 未生成: %v", err)
	}
	if artifact.Size() == 0 {
		t.Fatal("main.wasm 为空")
	}
}
