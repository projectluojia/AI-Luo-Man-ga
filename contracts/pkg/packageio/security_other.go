//go:build !unix && !windows

package packageio

import "os"

func validatePlatformPath(string, os.FileInfo) error {
	return ErrInsecurePath
}

func secureCreatedDirectory(string) error {
	return ErrInsecurePath
}
