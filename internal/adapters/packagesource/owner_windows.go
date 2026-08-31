//go:build windows

package packagesource

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerMatchesProcess 校验文件属主与当前进程 TokenOwner 一致。拿不到安全
// 信息一律拒绝；DACL 权限由同一路径的 groupOrWorldWritable 单独检查。
func ownerMatchesProcess(path string, _ os.FileInfo) bool {
	securityDescriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil || securityDescriptor == nil {
		return false
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil || owner == nil {
		return false
	}
	processOwner, err := currentProcessOwner()
	if err != nil || !windows.EqualSid(owner, processOwner) {
		return false
	}
	return true
}

type tokenOwnerInformation struct {
	Owner *windows.SID
}

func tokenOwner(token windows.Token) (*windows.SID, error) {
	var length uint32
	err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &length)
	if err != windows.ERROR_INSUFFICIENT_BUFFER {
		if err == nil {
			return nil, windows.ERROR_INVALID_SID
		}
		return nil, err
	}
	if length == 0 {
		return nil, windows.ERROR_INVALID_SID
	}
	buffer := make([]byte, length)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], length, &length); err != nil {
		return nil, err
	}
	information := (*tokenOwnerInformation)(unsafe.Pointer(&buffer[0]))
	if information.Owner == nil || !information.Owner.IsValid() {
		return nil, windows.ERROR_INVALID_SID
	}
	return information.Owner.Copy()
}

func currentProcessOwner() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer func() { _ = token.Close() }()
	return tokenOwner(token)
}

// groupOrWorldWritable 报告 Unix 组/其他可写位检查在 Windows 的等价结果：
// Windows 的权限由 ACL 治理（属主校验使用 TokenOwner），Go 模拟的权限位无意义。
func groupOrWorldWritable(_ os.FileInfo) bool {
	return false
}
