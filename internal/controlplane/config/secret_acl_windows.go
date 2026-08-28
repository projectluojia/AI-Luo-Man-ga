//go:build windows

package config

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// restrictPrivateFileACL 对 Windows 文件施加受限 DACL：仅当前用户完全控制。
// 与 sqlite 包 hardenAndSyncFile 同模式，确保写入侧与校验侧语义一致。
func restrictPrivateFileACL(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current process user: %w", err)
	}
	// 构建仅允许当前用户完全控制的 DACL。
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return fmt.Errorf("build restricted file ACL: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read restricted file DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("apply restricted file ACL: %w", err)
	}
	return nil
}

// fileACLRestrictedToCurrentUser 校验文件 DACL 的每条 ACE 均为当前用户的
// ACCESS_ALLOWED，不存在授予其他用户的访问权限。DACL 为空或含其他 ACE
// （DENY/其他 SID/AUDIT 等）均视为未受限制。
func fileACLRestrictedToCurrentUser(path string) bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil ||
			ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			uint32(ace.Header.AceSize) < uint32(unsafe.Sizeof(*ace)) {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.String() != user.User.Sid.String() {
			return false
		}
	}
	return true
}
