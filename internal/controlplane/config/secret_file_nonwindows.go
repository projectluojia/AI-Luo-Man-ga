//go:build !windows

package config

import "os"

// regularNonEmptyFile 校验秘密文件：常规文件、仅属主可读写（0600）、非空且不超限。
// Unix 平台执行完整权限语义；Windows 无 Unix 权限位，见 secret_file_windows.go。
func regularNonEmptyFile(path string, maximum int) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && info.Size() > 0 && info.Size() <= int64(maximum)
}
