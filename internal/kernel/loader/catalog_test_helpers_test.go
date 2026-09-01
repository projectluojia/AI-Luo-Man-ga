//go:build unix

package loader_test

import (
	"context"
	"encoding/json"
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
