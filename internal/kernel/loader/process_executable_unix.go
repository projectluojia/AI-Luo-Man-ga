//go:build unix

package loader

import "os"

// executableFile 报告文件是否带可执行位（Unix 权限位语义）。
func executableFile(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
