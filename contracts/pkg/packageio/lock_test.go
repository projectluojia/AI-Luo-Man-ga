package packageio_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
					Args:    []string{"--listen", "unix:" + filepath.Join(root, "runtime", "runtime.sock")},
					WorkDir: filepath.Join(root, "runtime"),
					Address: "unix:" + filepath.Join(root, "runtime", "runtime.sock"),
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

func TestCanonicalLockDigestPreservesExplicitExternalSocketAddress(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	lock := func(root string) packagecontract.Lock {
		return packagecontract.Lock{
			SchemaVersion: packagecontract.SchemaVersion, PackageID: "demo.pkg", PackageVersion: "1.0.0",
			ManifestSHA256: strings.Repeat("a", 64),
			Artifacts: []packagecontract.LockedArtifact{{
				ComponentID: "runtime", Path: filepath.Join(root, "runtime"),
				SHA256: strings.Repeat("b", 64),
				Process: &packagecontract.ProcessSpec{
					Path:    filepath.Join(root, "runtime", "bin", "runner"),
					WorkDir: filepath.Join(root, "runtime"), Address: "unix:/var/run/shared-runtime.sock",
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
		t.Fatalf("digests differ for explicit external socket: %s != %s", firstDigest, secondDigest)
	}
	changed := lock(first)
	changed.Artifacts[0].Process.Address = "unix:/var/run/other-runtime.sock"
	changedDigest, err := packageio.CanonicalLockDigest(context.Background(), first, changed)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == changedDigest {
		t.Fatal("digests match for different explicit external sockets")
	}
}

func TestCanonicalLockDigestRejectsDuplicateComponentIDs(t *testing.T) {
	root := t.TempDir()
	lock := packagecontract.Lock{Artifacts: []packagecontract.LockedArtifact{
		{ComponentID: "runtime", Path: filepath.Join(root, "one")},
		{ComponentID: "runtime", Path: filepath.Join(root, "two")},
	}}
	if _, err := packageio.CanonicalLockDigest(context.Background(), root, lock); err == nil {
		t.Fatal("CanonicalLockDigest accepted duplicate component IDs")
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

func TestCanonicalLockDigestHonorsCancellationWithEmptyLock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := packageio.CanonicalLockDigest(ctx, t.TempDir(), packagecontract.Lock{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CanonicalLockDigest error = %v, want context.Canceled", err)
	}
}
