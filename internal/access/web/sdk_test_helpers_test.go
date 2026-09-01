package web_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// generateSDKWithCLI 通过独立 package-manager module 的 CLI 生成 SDK，消费方
// 测试不直接链接作者工具实现，仍验证真实 ailuo.toml 到 SDK 产物的链路。
func generateSDKWithCLI(t *testing.T, commandName, outputDir string) {
	t.Helper()
	repositoryRoot := findRepositoryRoot(t)
	command := exec.Command("go", "run", "./cmd/ailuo-pm", commandName,
		filepath.Join(repositoryRoot, "packages", "campus-bus"), outputDir)
	command.Dir = filepath.Join(repositoryRoot, "package-manager")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ailuo-pm %s 失败: %v\n%s", commandName, err, output)
	}
}

// findRepositoryRoot 从测试工作目录向上寻找仓库 workspace 文件。
func findRepositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.work")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("repository root not found")
		}
		directory = parent
	}
}
