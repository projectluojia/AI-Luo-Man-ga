//go:build windows

package config

import "os"

// regularNonEmptyFile 校验秘密文件：常规文件、非空且不超限。
// Windows 不提供 Unix 权限位（os.FileMode 恒报 0666），0600 语义无法由 Go 标准库
// 表达；写入仍走 writePrivateFile 的 0600 意图。Windows 文件访问的强制收窄
// （受限 ACL）作为后续安全强化，读取校验在此仅检查常规性与非空。
func regularNonEmptyFile(path string, maximum int) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= int64(maximum)
}
