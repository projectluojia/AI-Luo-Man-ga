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

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

func TestUnpackTarballRejectsTraversal(t *testing.T) {
	// 恶意 tarball：路径穿越条目必须被拒绝。gzip writer 必须显式关闭，否则
	// 压缩尾块不落盘，payload 是个截断的 gzip 流——测试就变成在验证"解压失败"
	// 而不是在验证"穿越条目被拒绝"。
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

// PackFromSource 产出的 tarball 必须能被 Install 直接消费：条目扁平（manifest.json
// + lock.json + 工件），且发布物 lock 的工件摘要与实际字节一致。
func TestPackFromSourceRoundTripsThroughInstall(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "pkg")
	writeSourcePackage(t, source, "demo.pkg", "1.4.2", packagecontract.ModeHosted, "app.wasm", nil)
	manifestBytes, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest packagecontract.Manifest
	if err := packagecontract.DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	tarball, err := packmgr.PackFromSource(ctx, source, outputDir, manifest, manifestBytes)
	if err != nil {
		t.Fatalf("PackFromSource: %v", err)
	}
	if filepath.Base(tarball) != "demo.pkg-1.4.2.tgz" {
		t.Fatalf("tarball = %q, want demo.pkg-1.4.2.tgz", filepath.Base(tarball))
	}
	record, err := packmgr.Install(ctx, t.TempDir(), tarball)
	if err != nil {
		t.Fatalf("Install from tarball: %v", err)
	}
	if record.Manifest.ID != "demo.pkg" || record.Manifest.Version != "1.4.2" {
		t.Fatalf("installed = %s@%s, want demo.pkg@1.4.2", record.Manifest.ID, record.Manifest.Version)
	}
}
