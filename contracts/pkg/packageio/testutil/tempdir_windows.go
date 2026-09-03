//go:build windows

package testutil

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func secureDirectory(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return windows.ERROR_INVALID_SID
	}
	owner, err := tokenOwner(token)
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;GA;;;%s)(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)",
		owner.String(),
		user.User.Sid.String(),
	))
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil,
	)
}

type tokenOwnerInformation struct {
	Owner *windows.SID
}

func tokenOwner(token windows.Token) (*windows.SID, error) {
	var length uint32
	err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &length)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || length < uint32(unsafe.Sizeof(tokenOwnerInformation{})) {
		if err == nil {
			return nil, windows.ERROR_INVALID_SID
		}
		return nil, err
	}
	buffer := make([]byte, length)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], length, &length); err != nil {
		return nil, err
	}
	owner := (*tokenOwnerInformation)(unsafe.Pointer(&buffer[0])).Owner
	if owner == nil || !owner.IsValid() {
		return nil, windows.ERROR_INVALID_SID
	}
	return owner.Copy()
}
