package sdkgen

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeGenerated 把生成产物写到 dir（按相对路径建目录），写失败直接终止用例。
func writeGenerated(t *testing.T, dir string, files []Generated) {
	t.Helper()
	for _, f := range files {
		path := filepath.Join(dir, f.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, f.Code, 0644); err != nil {
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
