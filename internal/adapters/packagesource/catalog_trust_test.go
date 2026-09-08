package packagesource

import (
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	packageiotest "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio/testutil"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
)

// writeIsolatedFixture 在安装根写入一个单 isolated 组件的包目录。signed 为真
// 时用一次性密钥签署 lock 并把公钥记入信任集合，否则留空签名。
func writeIsolatedFixture(t *testing.T, root, pkgID string, signed bool) map[string]struct{} {
	t.Helper()
	directory := filepath.Join(root, pkgID)
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifactBody := []byte("#!/bin/sh\nexit 0\n")
	artifact := filepath.Join(directory, "runtime-artifact")
	if err := os.WriteFile(artifact, artifactBody, 0o750); err != nil {
		t.Fatal(err)
	}
	installed := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: pkgID, Version: "1.0.0",
		Capabilities: []capability.CapabilitySpec{{
			ID: "extension.query", Version: "1.0.0", Name: "扩展查询",
			Description: "查询扩展", InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			Authorization: capability.AuthorizationSpec{ResourceType: "capability.resource"},
			Execution:     capability.ExecutionSpec{EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe, ConfirmationFloor: capability.ConfirmationPolicy},
		}},
		Components: []packagecontract.Component{{
			ID: pkgID, Mode: packagecontract.ModeIsolated, Role: packagecontract.RoleProvider, Entrypoint: "runtime-artifact",
			Process: &packagecontract.ProcessTemplate{Path: "runtime-artifact", Address: "127.0.0.1:50051"},
			Exports: []string{"extension.query"},
		}},
	}
	manifest, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	artifactDigest := sha256.Sum256(artifactBody)
	lock := packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: pkgID,
		PackageVersion: "1.0.0",
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: []packagecontract.LockedArtifact{{
			ComponentID: pkgID, Path: artifact, SHA256: hex.EncodeToString(artifactDigest[:]),
			Process: &packagecontract.ProcessSpec{
				Path: artifact, WorkDir: directory, Address: "127.0.0.1:50051",
			},
		}},
	}
	trusted := map[string]struct{}{}
	if signed {
		publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signature, err := packagecontract.Sign(lock, privateKey)
		if err != nil {
			t.Fatal(err)
		}
		lock.Signature = &signature
		trusted[hex.EncodeToString(publicKey)] = struct{}{}
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	return trusted
}

// discoverFixtureProjectLock 构造锁定 root 内全部包目录的项目锁。
func discoverFixtureProjectLock(t *testing.T, root string) projectcontract.Lock {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "ailuo.toml")
	if err := os.WriteFile(manifestPath, []byte("[project]\nid = \"test\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := packageio.HashFile(t.Context(), manifestPath, packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lock := projectcontract.Lock{
		SchemaVersion: projectcontract.SchemaVersion, ProjectID: "test",
		ProjectManifestSHA256: manifestSHA,
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(root, entry.Name())
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
		packageManifestSHA, err := packageio.HashFile(t.Context(), filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
		if err != nil {
			t.Fatal(err)
		}
		lockSHA, err := packageio.CanonicalLockDigest(t.Context(), directory, packageLock)
		if err != nil {
			t.Fatal(err)
		}
		lock.Packages = append(lock.Packages, projectcontract.LockedPackage{
			ID: manifest.ID, Version: manifest.Version, Source: "path:packages/" + manifest.ID,
			ManifestSHA256: packageManifestSHA, LockSHA256: lockSHA,
		})
	}
	return lock
}

func TestDiscoverLockedEnforcesSignatureTrustChain(t *testing.T) {
	ctx := t.Context()
	t.Run("signed isolated package loads with trusted signer", func(t *testing.T) {
		root := packageiotest.TempDir(t)
		trusted := writeIsolatedFixture(t, root, "isolated.signed", true)
		packageiotest.SecureTree(t, root)
		catalog, err := NewCatalog(root, trusted)
		if err != nil {
			t.Fatal(err)
		}
		records, err := catalog.DiscoverLocked(ctx, discoverFixtureProjectLock(t, root))
		if err != nil || len(records) != 1 {
			t.Fatalf("records=%d err=%v", len(records), err)
		}
	})
	t.Run("unsigned isolated package is rejected with empty trust", func(t *testing.T) {
		root := packageiotest.TempDir(t)
		writeIsolatedFixture(t, root, "isolated.unsigned", false)
		packageiotest.SecureTree(t, root)
		catalog, err := NewCatalog(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = catalog.DiscoverLocked(ctx, discoverFixtureProjectLock(t, root))
		if !errors.Is(err, ErrInvalidCatalog) {
			t.Fatalf("unsigned discovery error = %v, want ErrInvalidCatalog", err)
		}
	})
	t.Run("untrusted signer is rejected", func(t *testing.T) {
		root := packageiotest.TempDir(t)
		writeIsolatedFixture(t, root, "isolated.signed", true)
		packageiotest.SecureTree(t, root)
		catalog, err := NewCatalog(root, map[string]struct{}{"aa": {}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = catalog.DiscoverLocked(ctx, discoverFixtureProjectLock(t, root))
		if !errors.Is(err, ErrInvalidCatalog) {
			t.Fatalf("untrusted discovery error = %v, want ErrInvalidCatalog", err)
		}
	})
}
