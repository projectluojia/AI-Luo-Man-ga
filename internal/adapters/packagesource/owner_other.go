//go:build !unix && !windows

package packagesource

import "os"

// ownerMatchesProcess 在既非 Unix 亦非 Windows 的平台 fail-closed：
// 无法验证属主即拒绝安装目录。
func ownerMatchesProcess(string, os.FileInfo) bool {
	return false
}

// groupOrWorldWritable 在无法验证属主的平台 fail-closed（恒拒绝）。
func groupOrWorldWritable(os.FileInfo) bool {
	return true
}
