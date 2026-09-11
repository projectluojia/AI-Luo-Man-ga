//go:build !unix

package processhost

import (
	"os"
	"os/exec"
)

func configureProcessGroup(*exec.Cmd) {}

func terminateCommandProcess(process *os.Process) error {
	return process.Signal(os.Interrupt)
}

func killCommandProcess(process *os.Process) error {
	return process.Kill()
}
