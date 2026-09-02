//go:build !unix && !windows

package loader

import "os"

// executableFile 在无法验证执行权限的平台 fail-closed。
func executableFile(os.FileInfo) bool { return false }
