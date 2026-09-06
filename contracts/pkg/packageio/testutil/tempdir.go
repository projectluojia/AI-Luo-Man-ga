// Package testutil 提供 packageio 使用的跨平台测试目录。
package testutil

import "testing"

// TempDir 创建符合安装目录安全策略的测试目录。
func TempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := secureDirectory(path); err != nil {
		t.Fatal(err)
	}
	return path
}
