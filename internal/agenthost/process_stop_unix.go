//go:build unix

package agenthost

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func interruptProcess(process *os.Process) error {
	return process.Signal(os.Interrupt)
}

func normalizeExpectedStopError(err error) error {
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal := status.Signal()
			if signal == syscall.SIGINT || signal == syscall.SIGTERM {
				return nil
			}
		}
	}
	return err
}
