package packageio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRecoverInstallRootRejectsInsecureEmptyBackup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("平台 %s 不使用 Unix 安全路径策略", runtime.GOOS)
	}
	root := t.TempDir()
	backup := filepath.Join(root, BackupPrefix+"empty")
	if err := os.Mkdir(backup, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backup, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInstallRoot(context.Background(), root); !errors.Is(err, ErrInsecurePath) {
		t.Fatalf("RecoverInstallRoot(insecure empty backup) = %v, want ErrInsecurePath", err)
	}
}
