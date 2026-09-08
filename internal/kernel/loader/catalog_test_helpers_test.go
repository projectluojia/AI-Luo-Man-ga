//go:build unix

package loader_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// fixtureTrustedSigners 是 writeInstalledFixture 为最近签署的 isolated fixture
// 记录的部署信任集合：fixture 构造与目录源构造不在同一函数，用测试局部变量
// 传递，不引入全局态。
var fixtureTrustedSigners map[string]struct{}

// setFixtureTrustedSigners 记录当前测试的信任集合，并在测试结束时清空。
func setFixtureTrustedSigners(t testing.TB, signers map[string]struct{}) {
	t.Helper()
	fixtureTrustedSigners = signers
	t.Cleanup(func() { fixtureTrustedSigners = nil })
}

// trustedSignerCatalog 构造装载当前 fixture 所需的目录源。
func trustedSignerCatalog(t testing.TB, root string) *packagesource.Catalog {
	t.Helper()
	catalog, err := packagesource.NewCatalog(root, fixtureTrustedSigners)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func discoverCatalogLocked(t testing.TB, catalog *packagesource.Catalog, root string) ([]loader.InstalledRecord, error) {
	t.Helper()
	return catalog.DiscoverLocked(t.Context(), catalogProjectLock(t, root))
}

func catalogProjectLock(t testing.TB, root string) projectcontract.Lock {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !packageio.IsTransientInstallDirectory(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	return catalogProjectLockForIDs(t, root, ids...)
}

func catalogProjectLockForIDs(t testing.TB, root string, ids ...string) projectcontract.Lock {
	t.Helper()
	projectRoot := t.TempDir()
	manifestPath := filepath.Join(projectRoot, "ailuo.toml")
	if err := os.WriteFile(manifestPath, []byte("[project]\nid = \"test\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	projectManifestSHA, err := packageio.HashFile(context.Background(), manifestPath, packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lock := projectcontract.Lock{
		SchemaVersion: projectcontract.SchemaVersion, ProjectID: "test",
		ProjectManifestSHA256: projectManifestSHA, Packages: make([]projectcontract.LockedPackage, 0, len(ids)),
	}
	for _, id := range ids {
		directory := filepath.Join(root, id)
		manifestBytes, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest packagecontract.Manifest
		if err := packagecontract.DecodeStrictJSON(manifestBytes, &manifest); err != nil {
			t.Fatal(err)
		}
		lockBytes, err := os.ReadFile(filepath.Join(directory, "lock.json"))
		if err != nil {
			t.Fatal(err)
		}
		var packageLock packagecontract.Lock
		if err := packagecontract.DecodeStrictJSON(lockBytes, &packageLock); err != nil {
			t.Fatal(err)
		}
		manifestSHA, err := packageio.HashFile(context.Background(), filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		lockSHA, err := packageio.CanonicalLockDigest(context.Background(), directory, packageLock)
		if err != nil {
			t.Fatal(err)
		}
		lock.Packages = append(lock.Packages, projectcontract.LockedPackage{
			ID: manifest.ID, Version: manifest.Version, Source: "path:packages/" + manifest.ID,
			ManifestSHA256: manifestSHA, LockSHA256: lockSHA,
		})
	}
	return lock
}
