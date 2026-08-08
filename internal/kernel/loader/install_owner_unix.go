//go:build unix

package loader

import (
	"os"
	"syscall"
)

func ownerMatchesProcess(info os.FileInfo) bool {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(metadata.Uid) == os.Geteuid()
}
