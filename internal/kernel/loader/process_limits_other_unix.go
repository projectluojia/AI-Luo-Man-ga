//go:build unix && !linux

package loader

import (
	"fmt"
	"os"
)

// applyProcessLimits 在没有 prlimit 的 Unix 平台（macOS/BSD）对携带非零限额的
// isolated 包 fail-closed；零限额包照常运行。
func applyProcessLimits(_ *os.Process, limits ProcessLimits) error {
	if limits != (ProcessLimits{}) {
		return fmt.Errorf("isolated runtime resource limits are unsupported on this platform: %w", ErrInvalidProcessSpec)
	}
	return nil
}
