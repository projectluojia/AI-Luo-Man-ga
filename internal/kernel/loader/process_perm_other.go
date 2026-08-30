//go:build !unix && !windows

package loader

import "os"

// 其余平台 fail-closed：无法验证可执行性与权限位即拒绝启动运行时进程。
func executableFile(os.FileInfo) bool { return false }

func unsafePermissions(os.FileInfo) bool { return true }
