//go:build !unix && !windows

package sqlite

import (
	"os"
	"path/filepath"
)

func publishDatabaseFile(temporary, destination string) error {
	if err := os.Link(temporary, destination); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}
