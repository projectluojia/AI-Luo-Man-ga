//go:build windows

package sqlite

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func hardenAndSyncFile(path string) (resultErr error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current process user: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return fmt.Errorf("build restricted file ACL: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read restricted file ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("apply restricted file ACL: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open restricted file for synchronization: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	return file.Sync()
}
