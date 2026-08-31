//go:build !unix

package main

import (
	"fmt"
	"os"
)

const unixSecurityAvailable = false

func validateSecretFile(path string) error {
	return validateSecretFileNamed(path, "AILUO_MODEL_API_KEY_FILE")
}

func validateSecretFileNamed(path, envName string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("configuration error: cannot inspect %s: %w", envName, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<10 {
		return fmt.Errorf("configuration error: %s must be a non-empty regular file no larger than 16 KiB", envName)
	}
	return fmt.Errorf("configuration error: %s owner-only permission verification is not supported on this platform; use a governed secret source", envName)
}
