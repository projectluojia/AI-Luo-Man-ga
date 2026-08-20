//go:build !windows

package config

// restrictPrivateFileACL 在非 Windows 平台无需操作：Unix 权限位由 Chmod(0600) 保证。
func restrictPrivateFileACL(path string) error { return nil }
