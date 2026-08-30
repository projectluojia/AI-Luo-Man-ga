//go:build windows

package loader

import "os"

// executableFile 在 Windows 恒为 true：Go 模拟的权限位无执行语义，可执行性
// 由扩展名与 ACL 决定（属主一致性由安装目录 ACL 校验强制）。
func executableFile(os.FileInfo) bool { return true }

// unsafePermissions 在 Windows 恒为 false：权限由 ACL 治理，Go 模拟的
// 0666 权限位在此平台无意义。
func unsafePermissions(os.FileInfo) bool { return false }
