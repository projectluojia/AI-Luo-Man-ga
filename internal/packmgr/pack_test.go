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
	cases := []struct {
		name   string
		header *tar.Header
		want   string
	}{
		{name: "absolute path", header: &tar.Header{Name: "/etc/passwd", Mode: 0o640, Size: 4, Typeflag: tar.TypeReg}, want: "条目路径非法"},
		{name: "traversal", header: &tar.Header{Name: "../escape", Mode: 0o640, Size: 4, Typeflag: tar.TypeReg}, want: "条目路径非法"},
		{name: "symlink", header: &tar.Header{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, want: "不支持条目"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := &bytes.Buffer{}
			gzipWriter := gzip.NewWriter(payload)
			tarWriter := tar.NewWriter(gzipWriter)
			if err := tarWriter.WriteHeader(tc.header); err != nil {
				t.Fatal(err)
			}
			if tc.header.Size > 0 {
				if _, err := tarWriter.Write([]byte("evil")); err != nil {
					t.Fatal(err)
				}
			}
			if err := tarWriter.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gzipWriter.Close(); err != nil {
				t.Fatal(err)
			}
			malicious := filepath.Join(t.TempDir(), "evil.tgz")
			if err := os.WriteFile(malicious, payload.Bytes(), 0o640); err != nil {
				t.Fatal(err)
			}
			if _, err := packmgr.Install(context.Background(), t.TempDir(), malicious); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Install error = %v, want %q", err, tc.want)
			}
		})
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
