package packmgr_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	packageiotest "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio/testutil"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
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
	if _, err := packmgr.Install(context.Background(), packageiotest.TempDir(t), malicious); err == nil {
		t.Fatal("unpack traversal tarball = nil, want error")
	}
}

func TestInstallRejectsNonPackageSource(t *testing.T) {
	ctx := context.Background()
	plainFile := filepath.Join(t.TempDir(), "readme.txt")
	if err := os.WriteFile(plainFile, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := packmgr.Install(ctx, packageiotest.TempDir(t), plainFile); err == nil || !strings.Contains(err.Error(), "目录或 .tgz") {
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
	record, err := packmgr.Install(ctx, packageiotest.TempDir(t), tarball)
	if err != nil {
		t.Fatalf("Install from tarball: %v", err)
	}
	if record.Manifest.ID != "demo.pkg" || record.Manifest.Version != "1.4.2" {
		t.Fatalf("installed = %s@%s, want demo.pkg@1.4.2", record.Manifest.ID, record.Manifest.Version)
	}
}

func TestPackFromSourceRejectsMismatchedManifestBytes(t *testing.T) {
	source := t.TempDir()
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "demo.pkg", Version: "1.0.0",
		Components: []packagecontract.Component{{ID: "core", Mode: packagecontract.ModeHosted, Role: packagecontract.RoleProvider, Entrypoint: "app.wasm"}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "app.wasm"), []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	mismatched := append([]byte(nil), manifestBytes...)
	mismatched = bytes.Replace(mismatched, []byte(`"demo.pkg"`), []byte(`"other.pkg"`), 1)
	if _, err := packmgr.PackFromSource(context.Background(), source, t.TempDir(), manifest, mismatched); err == nil {
		t.Fatal("PackFromSource accepted mismatched manifest bytes")
	}
}

func TestInstallRejectsArchiveLockDigestMismatch(t *testing.T) {
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "demo.pkg", Version: "1.0.0",
		Components: []packagecontract.Component{{ID: "main", Mode: packagecontract.ModeHosted, Entrypoint: "app.wasm"}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("actual artifact")
	manifestDigest := sha256.Sum256(manifestBytes)
	wrongDigest := sha256.Sum256([]byte("different artifact"))
	lockBytes, err := json.Marshal(packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: manifest.ID,
		PackageVersion: manifest.Version, ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: []packagecontract.LockedArtifact{{
			ComponentID: "main", Path: "app.wasm", SHA256: hex.EncodeToString(wrongDigest[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "demo.pkg-1.0.0.tgz")
	writeTestArchive(t, archive, manifestBytes, artifact, lockBytes)
	if _, err := packmgr.Install(context.Background(), packageiotest.TempDir(t), archive); err == nil {
		t.Fatal("Install accepted archive lock with a mismatched artifact digest")
	}
}

func TestInspectAndInstallRejectArchiveManifestDigestMismatch(t *testing.T) {
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "demo.pkg", Version: "1.0.0",
		Components: []packagecontract.Component{{ID: "main", Mode: packagecontract.ModeHosted, Entrypoint: "app.wasm"}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("actual artifact")
	artifactDigest := sha256.Sum256(artifact)
	wrongManifestDigest := sha256.Sum256([]byte("different manifest"))
	lockBytes, err := json.Marshal(packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: manifest.ID,
		PackageVersion: manifest.Version, ManifestSHA256: hex.EncodeToString(wrongManifestDigest[:]),
		Artifacts: []packagecontract.LockedArtifact{{
			ComponentID: "main", Path: "app.wasm", SHA256: hex.EncodeToString(artifactDigest[:]),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "demo.pkg-1.0.0.tgz")
	writeTestArchive(t, archive, manifestBytes, artifact, lockBytes)
	if _, _, err := packmgr.Inspect(context.Background(), archive); err == nil {
		t.Fatal("Inspect accepted archive with a mismatched manifest digest")
	}
	if _, err := packmgr.Install(context.Background(), packageiotest.TempDir(t), archive); err == nil {
		t.Fatal("Install accepted archive with a mismatched manifest digest")
	}
}

func writeTestArchive(t *testing.T, path string, manifest, artifact, lock []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, data := range map[string][]byte{
		"manifest.json": manifest,
		"app.wasm":      artifact,
		"lock.json":     lock,
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o640, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
