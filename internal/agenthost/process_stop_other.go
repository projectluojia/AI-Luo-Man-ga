//go:build !unix

package agenthost

import (
	"errors"
	"os"
	"os/exec"
)

func interruptProcess(process *os.Process) error {
	return process.Kill()
}

func normalizeExpectedStopError(err error) error {
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return nil
	}
	return err
}
