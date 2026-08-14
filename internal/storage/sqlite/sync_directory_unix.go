//go:build unix

package sqlite

import (
	"errors"
	"os"
)

func syncDirectory(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close())
	}()
	return directory.Sync()
}
