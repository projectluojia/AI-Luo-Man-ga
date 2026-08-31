//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

const unixSecurityAvailable = true

func validateSecretFile(path string) error {
	return validateSecretFileNamed(path, "AILUO_MODEL_API_KEY_FILE")
}

func validateSecretFileNamed(path, envName string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("configuration error: cannot inspect %s: %w", envName, err)
	}
	metadata, ownershipAvailable := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > 16<<10 ||
		!ownershipAvailable || int(metadata.Uid) != os.Geteuid() {
		return fmt.Errorf("configuration error: %s must be an owner-held non-empty regular file no larger than 16 KiB with no group or other permissions", envName)
	}
	return nil
}
