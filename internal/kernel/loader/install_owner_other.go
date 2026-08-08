//go:build !unix

package loader

import "os"

func ownerMatchesProcess(os.FileInfo) bool {
	return false
}
