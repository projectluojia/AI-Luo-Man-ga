//go:build unix

package sqlite

import (
	"errors"
	"os"
)

func hardenAndSyncFile(path string) (resultErr error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	return file.Sync()
}
