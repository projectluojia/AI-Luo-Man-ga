//go:build !unix && !windows

package sqlite

import "fmt"

func syncDirectory(string) error {
	return fmt.Errorf("directory synchronization is not supported on this platform")
}
