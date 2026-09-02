package packageio_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

func TestValidateSecureTreeAcceptsOwnedTree(t *testing.T) {
	if unsupportedSecurityPlatform() {
		t.Skipf("平台 %s 没有安全路径验证实现", runtime.GOOS)
	}
	root := newSecureTestDir(t)
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "artifact"), []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecureTree(context.Background(), root); err != nil {
		t.Fatalf("ValidateSecureTree(owned tree) = %v", err)
	}
}

func TestValidateSecureTreeRejectsSymlink(t *testing.T) {
	if unsupportedSecurityPlatform() {
		t.Skipf("平台 %s 没有安全路径验证实现", runtime.GOOS)
	}
	root := newSecureTestDir(t)
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("target"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("当前 Windows 环境不允许创建符号链接: %v", err)
	}
	if err := packageio.ValidateSecureTree(context.Background(), root); !errors.Is(err, packageio.ErrInsecurePath) {
		t.Fatalf("ValidateSecureTree(symlink) = %v, want ErrInsecurePath", err)
	}
}

func TestValidateSecurePathRejectsGroupWritableUnixFile(t *testing.T) {
	if runtime.GOOS == "windows" || unsupportedSecurityPlatform() {
		t.Skipf("平台 %s 使用其他安全策略", runtime.GOOS)
	}
	root := newSecureTestDir(t)
	path := filepath.Join(root, "writable")
	if err := os.WriteFile(path, []byte("writable"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := packageio.ValidateSecurePath(path); !errors.Is(err, packageio.ErrInsecurePath) {
		t.Fatalf("ValidateSecurePath(group-writable file) = %v, want ErrInsecurePath", err)
	}
}

func newSecureTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := secureTestDirectory(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func unsupportedSecurityPlatform() bool {
	return runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin"
}
