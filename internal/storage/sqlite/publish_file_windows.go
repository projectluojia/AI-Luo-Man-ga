//go:build windows

package sqlite

import "golang.org/x/sys/windows"

func publishDatabaseFile(temporary, destination string) error {
	source, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, windows.MOVEFILE_WRITE_THROUGH)
}
