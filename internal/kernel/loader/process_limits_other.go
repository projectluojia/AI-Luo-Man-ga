//go:build !unix && !windows

package loader

import (
	"fmt"
	"os"
)

// applyProcessLimits 在既无 rlimit 也无 Job Object 的平台（Plan 9、js/wasm 等）
// 对携带非零限额的 isolated 包 fail-closed；零限额包照常运行。
func applyProcessLimits(_ *os.Process, limits ProcessLimits) (func() error, error) {
	if limits != (ProcessLimits{}) {
		return nil, fmt.Errorf("isolated runtime resource limits are unsupported on this platform: %w", ErrInvalidProcessSpec)
	}
	return nil, nil
}
