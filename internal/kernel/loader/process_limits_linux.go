//go:build linux

package loader

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// applyProcessLimits 在子进程启动后立即用 prlimit 应用资源限额。
// exec 完成到 prlimit 生效之间只有启动初始化窗口，之后运行期资源严格受限。
func applyProcessLimits(process *os.Process, limits ProcessLimits) error {
	if limits.MaxAddressBytes > 0 {
		if err := unix.Prlimit(process.Pid, unix.RLIMIT_AS,
			&unix.Rlimit{Cur: limits.MaxAddressBytes, Max: limits.MaxAddressBytes}, nil); err != nil {
			return fmt.Errorf("set RLIMIT_AS: %w", err)
		}
	}
	if limits.MaxCPUSeconds > 0 {
		if err := unix.Prlimit(process.Pid, unix.RLIMIT_CPU,
			&unix.Rlimit{Cur: limits.MaxCPUSeconds, Max: limits.MaxCPUSeconds}, nil); err != nil {
			return fmt.Errorf("set RLIMIT_CPU: %w", err)
		}
	}
	if limits.MaxOpenFiles > 0 {
		if err := unix.Prlimit(process.Pid, unix.RLIMIT_NOFILE,
			&unix.Rlimit{Cur: limits.MaxOpenFiles, Max: limits.MaxOpenFiles}, nil); err != nil {
			return fmt.Errorf("set RLIMIT_NOFILE: %w", err)
		}
	}
	if limits.MaxFileBytes > 0 {
		if err := unix.Prlimit(process.Pid, unix.RLIMIT_FSIZE,
			&unix.Rlimit{Cur: limits.MaxFileBytes, Max: limits.MaxFileBytes}, nil); err != nil {
			return fmt.Errorf("set RLIMIT_FSIZE: %w", err)
		}
	}
	return nil
}
