//go:build windows

package loader

import "os"

// executableFile 在 Windows 上由文件类型和 ACL 决定，Go 权限位没有执行语义。
func executableFile(os.FileInfo) bool { return true }
