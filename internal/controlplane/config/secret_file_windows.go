//go:build windows

package config

import "os"

// regularNonEmptyFile 校验秘密文件：常规文件、非空、不超限，且 DACL 仅授予
// 当前用户（Windows 无 Unix 权限位，0600 由受限 ACL 承担；无法校验或未受限制
// 的文件不会被当作密钥，fail-closed）。
func regularNonEmptyFile(path string, maximum int) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > int64(maximum) {
		return false
	}
	return fileACLRestrictedToCurrentUser(path)
}
