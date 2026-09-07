// Package testutil 提供 packageio 使用的跨平台测试目录。
package testutil

import (
	"io/fs"
	"path/filepath"
	"testing"
)

// TempDir 创建符合安装目录安全策略的测试目录。
func TempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := secureDirectory(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// SecureTree 重新固定测试安装树中后来创建的节点权限。
func SecureTree(t *testing.T, path string) {
	t.Helper()
	if err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return secureDirectory(current)
	}); err != nil {
		t.Fatal(err)
	}
}
