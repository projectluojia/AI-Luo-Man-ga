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

	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
)

func TestPackAndInstallFromTarball(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.0.0", packmgr.ModeHosted, "app.wasm", nil)

	output := t.TempDir()
	tarballPath, err := packmgr.Pack(ctx, source, output)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if filepath.Base(tarballPath) != "demo.pkg-1.0.0.tgz" {
		t.Fatalf("tarball name = %s, want demo.pkg-1.0.0.tgz", filepath.Base(tarballPath))
	}
	// 发布物可安装（解压 → 走完整安装流程）。
	root := t.TempDir()
	record, err := packmgr.Install(ctx, root, tarballPath)
	if err != nil {
		t.Fatalf("Install from tarball: %v", err)
	}
	if record.Manifest.Version != "1.0.0" {
		t.Fatalf("installed version = %s", record.Manifest.Version)
	}
	reloaded, err := packmgr.ReadInstalled(ctx, filepath.Join(root, "demo.pkg"))
	if err != nil {
		t.Fatalf("ReadInstalled: %v", err)
	}
	if reloaded.ArtifactPath != filepath.Join(root, "demo.pkg", "app.wasm") {
		t.Fatalf("artifact path = %s", reloaded.ArtifactPath)
	}
}

func TestUnpackTarballRejectsTraversal(t *testing.T) {
	// 恶意 tarball：路径穿越条目必须被拒绝。
	payload := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(payload)
	tarWriter := tar.NewWriter(gzipWriter)
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
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	malicious := filepath.Join(t.TempDir(), "evil.tgz")
	if err := os.WriteFile(malicious, payload.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	// 恶意 tarball 经 Install 的严格解压被拒绝。
	if _, err := packmgr.Install(context.Background(), t.TempDir(), malicious); err == nil ||
		!strings.Contains(err.Error(), "条目路径非法") {
		t.Fatalf("unpack traversal tarball error = %v, want path rejection", err)
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
