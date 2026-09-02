//go:build windows

package app

import (
	"fmt"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// prepareSecureTestDirectory 为 Windows 测试目录移除 runner 临时目录的继承写权限。
func prepareSecureTestDirectory(t *testing.T, path string) {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := currentTestTokenOwner(token)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;GA;;;%s)(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)",
		owner.String(),
		user.User.Sid.String(),
	))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil,
	); err != nil {
		t.Fatal(err)
	}
}

type testTokenOwnerInformation struct {
	Owner *windows.SID
}

func currentTestTokenOwner(token windows.Token) (*windows.SID, error) {
	var length uint32
	err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &length)
	if err != windows.ERROR_INSUFFICIENT_BUFFER || length < uint32(unsafe.Sizeof(testTokenOwnerInformation{})) {
		if err == nil {
			return nil, windows.ERROR_INVALID_SID
		}
		return nil, err
	}
	buffer := make([]byte, length)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], length, &length); err != nil {
		return nil, err
	}
	owner := (*testTokenOwnerInformation)(unsafe.Pointer(&buffer[0])).Owner
	if owner == nil || !owner.IsValid() {
		return nil, windows.ERROR_INVALID_SID
	}
	return owner.Copy()
}
