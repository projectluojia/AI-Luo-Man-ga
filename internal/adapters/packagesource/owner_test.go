package packagesource

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOwnerMatchesProcessForOwnedFiles 验证属主校验：本进程创建的文件属主
// 必然匹配（Windows 走 ACL SID 比较，Unix 走 eUID）。无法验证属主的平台
// fail-closed（恒 false），此时跳过正向断言。
func TestOwnerMatchesProcessForOwnedFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("平台 %s 无属主验证实现，fail-closed", runtime.GOOS)
	}
	if !ownerMatchesProcess(path, info) {
		t.Fatal("本进程创建的文件属主未通过校验")
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !ownerMatchesProcess(directory, dirInfo) {
		t.Fatal("本进程创建的目录属主未通过校验")
	}
}

// TestGroupOrWorldWritableFollowsPlatform 顺带锁定平台行为：Unix 上 0666 的
// 文件可写位为不安全；Windows 上权限位无意义恒安全（ACL 治理）。
func TestGroupOrWorldWritableFollowsPlatform(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "writable.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o666); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "windows":
		if groupOrWorldWritable(info) {
			t.Fatal("Windows 权限位不应参与判断")
		}
	case "linux", "darwin":
		if !groupOrWorldWritable(info) {
			t.Fatal("0666 文件应判定为组/其他可写")
		}
	}
}
