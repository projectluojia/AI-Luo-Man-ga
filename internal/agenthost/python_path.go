package agenthost

import (
	"path/filepath"
	"runtime"
)

func DefaultPythonPath(projectRoot string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(projectRoot, "agent", ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(projectRoot, "agent", ".venv", "bin", "python")
}
