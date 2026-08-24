package packmgr_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

func TestUnpackTarballRejectsTraversal(t *testing.T) {
	// 恶意 tarball：路径穿越条目必须被拒绝。
	payload := &bytes.Buffer{}
	tarWriter := tar.NewWriter(gzip.NewWriter(payload))
	evil := []byte("evil")
	// 直接绝对路径。
	_ = tarWriter.WriteHeader(&tar.Header{Name: "/etc/passwd", Mode: 0o640, Size: int64(len(evil)), Typeflag: tar.TypeReg})
	_, _ = tarWriter.Write(evil)
	// .. 穿越。
	_ = tarWriter.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o640, Size: int64(len(evil)), Typeflag: tar.TypeReg})
	_, _ = tarWriter.Write(evil)
	// 符号链接条目。
	_ = tarWriter.WriteHeader(&tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/tmp"})
	_ = tarWriter.Close()

	malicious := filepath.Join(t.TempDir(), "evil.tgz")
	if err := os.WriteFile(malicious, payload.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	// 恶意 tarball 经 Install 的严格解压被拒绝。
	if _, err := packmgr.Install(context.Background(), t.TempDir(), malicious); err == nil {
		t.Fatal("unpack traversal tarball = nil, want error")
	}
}

func TestInstallRejectsNonPackageSource(t *testing.T) {
	ctx := context.Background()
	plainFile := filepath.Join(t.TempDir(), "readme.txt")
	if err := os.WriteFile(plainFile, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := packmgr.Install(ctx, t.TempDir(), plainFile); err == nil || !strings.Contains(err.Error(), "目录或 .tgz") {
		t.Fatalf("Install non-package source error = %v, want directory-or-tgz error", err)
	}
}
