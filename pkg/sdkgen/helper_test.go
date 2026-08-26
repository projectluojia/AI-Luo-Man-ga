package sdkgen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeGoModule 写 Go 临时模块（go.mod + 生成文件），校验 go.mod 版本。
func writeGoModule(t *testing.T, dir string, files []Generated) error {
	t.Helper()
	modPath := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(modPath, []byte("module gen\n\ngo 1.23\n"), 0644); err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.Code, 0644); err != nil {
			return err
		}
	}
	return nil
}

func writeGenerated(t *testing.T, dir string, files []Generated) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, file.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, file.Code, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runGo 在临时模块目录执行 go 命令，返回输出与错误。
func runGo(t *testing.T, dir, command string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append([]string{command}, args...)...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return string(output), err
}
