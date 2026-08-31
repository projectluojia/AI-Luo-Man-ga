package packagefmt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeVirtualEnvironmentLinks(t *testing.T) {
	root := t.TempDir()
	fileTarget := filepath.Join(root, "target.txt")
	fileLink := filepath.Join(root, "link.txt")
	if err := os.WriteFile(fileTarget, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fileTarget, fileLink); err != nil {
		t.Skipf("当前平台不允许创建符号链接: %v", err)
	}
	directoryTarget := filepath.Join(root, "target-dir")
	directoryLink := filepath.Join(root, "link-dir")
	if err := os.Mkdir(directoryTarget, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directoryTarget, "nested.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(directoryTarget, directoryLink); err != nil {
		t.Fatal(err)
	}

	if err := materializeVirtualEnvironment(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fileLink, directoryLink, filepath.Join(directoryLink, "nested.txt")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("链接未被实体化: %s", path)
		}
	}
	if content, err := os.ReadFile(fileLink); err != nil || string(content) != "file" {
		t.Fatalf("file link content=%q err=%v", content, err)
	}
}

func TestMaterializeVirtualEnvironmentDropsPosixLib64Alias(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "lib"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lib", filepath.Join(root, "lib64")); err != nil {
		t.Skipf("当前平台不允许创建符号链接: %v", err)
	}
	if err := materializeVirtualEnvironment(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "lib64")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lib64 alias error=%v, want alias removed", err)
	}
}
