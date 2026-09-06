//go:build unix

package packageio

import (
	"os"
	"syscall"
)

func validatePlatformPath(_ string, info os.FileInfo) error {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(metadata.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
		return ErrInsecurePath
	}
	return nil
}
