//go:build windows

package packagesource

import (
	"os"

	"golang.org/x/sys/windows"
)

// ownerMatchesProcess 校验文件属主与当前进程用户为同一 SID（Windows ACL 语义）：
// 安装目录及其内容必须由运行内核的同一账户所有，防止其他账户替换安装内容。
// 拿不到属主信息一律 fail-closed（返回 false）。
func ownerMatchesProcess(path string, _ os.FileInfo) bool {
	securityDescriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil || owner == nil {
		return false
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer func() { _ = token.Close() }()
	processUser, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	return windows.EqualSid(owner, processUser.User.Sid)
}

// groupOrWorldWritable 报告 Unix 组/其他可写位检查在 Windows 的等价结果：
// Windows 的权限由 ACL 治理（ownerMatchesProcess 已强制属主一致），
// Go 模拟的 0666 权限位在此平台无意义，恒返回 false。
func groupOrWorldWritable(_ os.FileInfo) bool {
	return false
}
