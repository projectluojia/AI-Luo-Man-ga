//go:build !unix && !windows

package sqlite

import "fmt"

func hardenAndSyncFile(string) error {
	return fmt.Errorf("secure backup file permissions are not supported on this platform")
}
