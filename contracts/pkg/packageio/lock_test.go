package packageio_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

func TestCanonicalLockDigestIgnoresInstallRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	lock := func(root string) packagecontract.Lock {
		return packagecontract.Lock{
			SchemaVersion: packagecontract.SchemaVersion, PackageID: "demo.pkg", PackageVersion: "1.0.0",
			ManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Artifacts: []packagecontract.LockedArtifact{{
				ComponentID: "runtime", Path: filepath.Join(root, "runtime"),
				SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Process: &packagecontract.ProcessSpec{
					Path:    filepath.Join(root, "runtime", "bin", "runner"),
					WorkDir: filepath.Join(root, "runtime"), Address: "127.0.0.1:9000",
				},
			}},
		}
	}
	firstDigest, err := packageio.CanonicalLockDigest(context.Background(), first, lock(first))
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := packageio.CanonicalLockDigest(context.Background(), second, lock(second))
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("digests differ across install roots: %s != %s", firstDigest, secondDigest)
	}
}

func TestCanonicalLockDigestRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	lock := packagecontract.Lock{
		Artifacts: []packagecontract.LockedArtifact{{
			ComponentID: "runtime", Path: filepath.Join(t.TempDir(), "outside"),
		}},
	}
	if _, err := packageio.CanonicalLockDigest(context.Background(), root, lock); err == nil {
		t.Fatal("CanonicalLockDigest accepted path outside install root")
	}
}
