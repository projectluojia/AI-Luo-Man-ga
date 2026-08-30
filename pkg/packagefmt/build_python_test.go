package packagefmt_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagefmt"
)

// TestBuildPythonUVCreatesVirtualEnvironment 验证 python-uv 构建器：在包目录
// 内按 uv.lock 创建 .venv，且未知构建器 fail-closed。
func TestBuildPythonUVCreatesVirtualEnvironment(t *testing.T) {
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv 不可用: %v", err)
	}
	sourceDir := t.TempDir()
	pyproject := "[project]\n" +
		"name = \"python-uv-test\"\n" +
		"version = \"0.1.0\"\n" +
		"requires-python = \">=3.10\"\n" +
		"dependencies = []\n"
	if err := os.WriteFile(filepath.Join(sourceDir, "pyproject.toml"), []byte(pyproject), 0o640); err != nil {
		t.Fatal(err)
	}
	// 作者侧先锁定依赖（无依赖项目离线生成 uv.lock）；构建器要求 --locked 可复现。
	lock := exec.Command("uv", "lock")
	lock.Dir = sourceDir
	if output, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("uv lock: %v\n%s", err, output)
	}
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "py.test", Version: "1.0.0",
		Components: []packagecontract.Component{{
			ID: "main", Mode: packagecontract.ModeIsolated, Entrypoint: ".venv",
		}},
	}
	if err := packagefmt.Build(context.Background(), sourceDir, manifest,
		packagefmt.BuildSpec{Tool: packagefmt.BuildToolPythonUV}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	python := filepath.Join(sourceDir, ".venv", "bin", "python")
	if runtime.GOOS == "windows" {
		python = filepath.Join(sourceDir, ".venv", "Scripts", "python.exe")
	}
	if _, err := os.Stat(python); err != nil {
		t.Fatalf("venv 解释器缺失: %v", err)
	}
	if err := packagefmt.Build(context.Background(), sourceDir, manifest,
		packagefmt.BuildSpec{Tool: "unknown-tool"}); err == nil {
		t.Fatal("未知构建器被接受")
	}
}
