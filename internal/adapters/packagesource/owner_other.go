//go:build !unix

package packagesource

import "os"

func ownerMatchesProcess(os.FileInfo) bool {
	return false
}
