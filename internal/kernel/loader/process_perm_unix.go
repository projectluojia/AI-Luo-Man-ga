//go:build unix

package loader

import "os"

// executableFile 报告文件是否带可执行位（Unix 权限位语义）。
func executableFile(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}

// unsafePermissions 报告组/其他可写位：Unix 下不允许运行时文件对同组或
// 任意用户可写。
func unsafePermissions(info os.FileInfo) bool {
	return info.Mode().Perm()&0o022 != 0
}
