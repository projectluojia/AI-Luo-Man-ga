//go:build windows

package packageio

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func validatePlatformPath(path string, _ os.FileInfo) error {
	securityDescriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || securityDescriptor == nil || !securityDescriptor.IsValid() {
		return ErrInsecurePath
	}
	owner, _, err := securityDescriptor.Owner()
	if err != nil || owner == nil || !owner.IsValid() {
		return ErrInsecurePath
	}
	processOwner, err := currentProcessOwner()
	if err != nil || !windows.EqualSid(owner, processOwner) {
		return ErrInsecurePath
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return ErrInsecurePath
	}
	control, _, err := securityDescriptor.Control()
	if err != nil || control&windows.SE_DACL_PRESENT == 0 {
		return ErrInsecurePath
	}
	sidOffset := uint32(unsafe.Offsetof(windows.ACCESS_ALLOWED_ACE{}.SidStart))
	const minimumSIDLength = 8
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil || ace == nil ||
			uint32(ace.Header.AceSize) < sidOffset+minimumSIDLength {
			return ErrInsecurePath
		}
		allowed := false
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			allowed = true
		default:
			return ErrInsecurePath
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !aceSID.IsValid() {
			return ErrInsecurePath
		}
		sidLength := windows.GetLengthSid(aceSID)
		if sidLength < minimumSIDLength || sidOffset+sidLength > uint32(ace.Header.AceSize) {
			return ErrInsecurePath
		}
		if allowed && hasUntrustedWriteRights(ace.Mask) && !trustedWriter(aceSID, processOwner) {
			return ErrInsecurePath
		}
	}
	return nil
}

func currentProcessOwner() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer func() { _ = token.Close() }()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, windows.ERROR_INVALID_SID
	}
	return user.User.Sid.Copy()
}

func trustedWriter(sid, processOwner *windows.SID) bool {
	return windows.EqualSid(sid, processOwner) ||
		sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

func hasUntrustedWriteRights(mask windows.ACCESS_MASK) bool {
	const fileDeleteChild windows.ACCESS_MASK = 0x00000040
	const writeRights = windows.ACCESS_MASK(
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
			windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED |
			windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
			windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | fileDeleteChild,
	)
	return mask&writeRights != 0
}
