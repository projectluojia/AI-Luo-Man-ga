//go:build windows

package app

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

// prepareSecureTestDirectory 为 Windows 测试目录移除 runner 临时目录的继承写权限。
func prepareSecureTestDirectory(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)",
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
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		t.Fatal(err)
	}
}
