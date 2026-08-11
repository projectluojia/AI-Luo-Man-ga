//go:build !unix

package main

import (
	"fmt"
	"os"
)

const unixSecurityAvailable = false

func validateSecretFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("configuration error: cannot inspect AILUO_MODEL_API_KEY_FILE: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<10 {
		return errorsNew("AILUO_MODEL_API_KEY_FILE must be a non-empty regular file no larger than 16 KiB")
	}
	return errorsNew("AILUO_MODEL_API_KEY_FILE owner-only permission verification is not supported on this platform; use a governed secret source")
}
