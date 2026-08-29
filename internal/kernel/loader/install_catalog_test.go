//go:build unix

package loader_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

func TestInstalledCatalogDiscoversVerifiesAndRegistersHostedRuntime(t *testing.T) {
	root := t.TempDir()
	artifact := writeInstalledFixture(t, root, "extension.test", loader.ModeHosted, false)
	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := catalog.Discover(t.Context())
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	record := records[0]
	if record.Runtime.ID != "extension.test.extension.test" || record.Runtime.Mode != loader.ModeHosted ||
		record.Process != nil || record.Service.ID != "extension" ||
		len(record.Tools) != 1 || len(record.Capabilities) != 1 {
		t.Fatalf("record=%#v", record)
	}
	runtime := &fakeRuntime{description: loader.Description{
		ID: record.Runtime.ID, Version: record.Runtime.Version, Mode: record.Runtime.Mode,
	}}
	manager, err := loader.New(&fakeHost{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := loader.RegisterInstalled(t.Context(), manager, reg, records); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.ResolveCapability("extension.query"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.ResolveTool("extension", "extension.read"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.VerifyRuntime(t.Context(), record.Runtime); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := catalog.VerifyRuntime(t.Context(), record.Runtime); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
		t.Fatalf("篡改后的校验错误=%v", err)
	}
}

func TestInstalledCatalogResolvesIsolatedProcessAndRejectsCatalogTampering(t *testing.T) {
	root := t.TempDir()
	writeInstalledFixture(t, root, "isolated.test", loader.ModeIsolated, false)
	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := catalog.Discover(t.Context())
	if err != nil || len(records) != 1 || records[0].Process == nil {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	process, err := catalog.ResolveProcess(t.Context(), records[0].Runtime)
	if err != nil || process.Path == "" || process.WorkDir == "" ||
		process.Address != "unix:"+filepath.Join(records[0].Directory, "runtime.sock") {
		t.Fatalf("process=%#v err=%v", process, err)
	}
	process.Args = []string{"mutated"}
	if err := catalog.VerifyProcess(t.Context(), records[0].Runtime, process); !errors.Is(err, loader.ErrInstallChanged) {
		t.Fatalf("变更 ProcessSpec 后的校验错误=%v", err)
	}

	manifestPath := filepath.Join(records[0].Directory, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest[:len(manifest)-1], []byte(`,"unknown":true}`)...)
	if err := os.WriteFile(manifestPath, manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	rewriteManifestDigest(t, records[0].Directory, manifest)
	if _, err := catalog.Discover(t.Context()); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
		t.Fatalf("未知字段目录错误=%v", err)
	}
}

func TestInstalledRegistrationRollsBackLoaderOnRegistryConflict(t *testing.T) {
	root := t.TempDir()
	writeInstalledFixture(t, root, "extension.test", loader.ModeHosted, false)
	catalog, _ := packagesource.NewCatalog(root)
	records, err := catalog.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(&fakeHost{})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	existing := records[0].Tools[0]
	if err := reg.RegisterTool(registry.ToolRegistration{Spec: existing, Handler: noopCatalogHandler}); err != nil {
		t.Fatal(err)
	}
	if err := loader.RegisterInstalled(t.Context(), manager, reg, records); !errors.Is(err, registry.ErrDuplicateID) {
		t.Fatalf("冲突注册错误=%v", err)
	}
	if err := manager.EnsureLoaded(t.Context(), records[0].Runtime.ID); !errors.Is(err, loader.ErrNotFound) {
		t.Fatalf("失败注册后 Loader 残留=%v", err)
	}
	if len(reg.Services()) != 0 || len(reg.Capabilities()) != 0 || len(reg.Tools()) != 1 {
		t.Fatalf("失败注册污染 Registry：tools=%#v services=%#v capabilities=%#v",
			reg.Tools(), reg.Services(), reg.Capabilities())
	}
}

func TestInstalledCatalogRejectsDuplicateJSONAndWritableDirectory(t *testing.T) {
	root := t.TempDir()
	writeInstalledFixture(t, root, "extension.test", loader.ModeHosted, false)
	directory := filepath.Join(root, "extension.test")
	manifestPath := filepath.Join(directory, "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append(manifest[:len(manifest)-1], []byte(`,"schema_version":"ailuo.package.v2"}`)...)
	if err := os.WriteFile(manifestPath, duplicate, 0o640); err != nil {
		t.Fatal(err)
	}
	rewriteManifestDigest(t, directory, duplicate)
	catalog, _ := packagesource.NewCatalog(root)
	if _, err := catalog.Discover(t.Context()); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
		t.Fatalf("重复 JSON 键错误=%v", err)
	}

	root = t.TempDir()
	writeInstalledFixture(t, root, "extension.test", loader.ModeHosted, false)
	if err := os.Chmod(filepath.Join(root, "extension.test"), 0o770); err != nil {
		t.Fatal(err)
	}
	catalog, _ = packagesource.NewCatalog(root)
	if _, err := catalog.Discover(t.Context()); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
		t.Fatalf("可写安装目录错误=%v", err)
	}
}

func writeInstalledFixture(t *testing.T, root, pkgID, mode string, unknown bool) string {
	t.Helper()
	directory := filepath.Join(root, pkgID)
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(directory, "runtime-artifact")
	artifactMode := os.FileMode(0o640)
	artifactBody := []byte("hosted artifact")
	if mode == loader.ModeIsolated {
		artifactMode = 0o750
		artifactBody = []byte("#!/bin/sh\nexit 0\n")
	}
	if err := os.WriteFile(artifact, artifactBody, artifactMode); err != nil {
		t.Fatal(err)
	}
	extensions, err := json.Marshal(map[string]any{
		"tools": []capability.ToolSpec{{
			ID: "extension.read", Version: "1.0.0", Description: "读取扩展数据",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
		"service": capability.ServiceSpec{
			ID: "extension", Version: "1.0.0", Description: "测试扩展",
			ToolDependencies: []string{"extension.read"},
		},
		"capabilities": []capability.CapabilitySpec{{
			ID: "extension.query", Version: "1.0.0", Name: "扩展查询",
			Description: "查询扩展", ServiceID: "extension",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: pkgID, Version: "1.0.0",
		IdleTTLMS: 1000, Extensions: extensions,
		Components: []packmgr.Component{{
			ID: pkgID, Mode: mode, Entrypoint: "runtime-artifact",
			Exports: []string{"extension.query"},
		}},
	}
	manifest, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if unknown {
		manifest = append(manifest[:len(manifest)-1], []byte(`,"unknown":true}`)...)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	artifactDigest := sha256.Sum256(artifactBody)
	lockedArtifact := packmgr.LockedArtifact{
		ComponentID: pkgID, Path: artifact, SHA256: hex.EncodeToString(artifactDigest[:]),
	}
	if mode == loader.ModeIsolated {
		lockedArtifact.Process = &packmgr.ProcessSpec{
			Path: artifact, WorkDir: directory, Address: "unix:" + filepath.Join(directory, "runtime.sock"),
		}
	}
	lock := packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion, PackageID: pkgID,
		PackageVersion: "1.0.0",
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts:      []packmgr.LockedArtifact{lockedArtifact},
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	return artifact
}

func rewriteManifestDigest(t *testing.T, directory string, manifest []byte) {
	t.Helper()
	lockPath := filepath.Join(directory, "lock.json")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock packmgr.Lock
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	lock.ManifestSHA256 = hex.EncodeToString(digest[:])
	lockBytes, err = json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
}

var noopCatalogHandler registry.Handler = func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// writeDeclaredFixture 写入携带宿主函数/storage 声明的 hosted fixture。
func writeDeclaredFixture(t *testing.T, root, runtimeID string, decls []packmgr.HostedFunctionDecl, storage *packmgr.Storage) loader.InstalledRecord {
	t.Helper()
	directory := filepath.Join(root, runtimeID)
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(directory, "runtime-artifact")
	if err := os.WriteFile(artifact, []byte("hosted artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	extensions, err := json.Marshal(map[string]any{
		"tools": []capability.ToolSpec{{
			ID: "extension.read", Version: "1.0.0", Description: "读取扩展数据",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
		"service": capability.ServiceSpec{
			ID: "extension", Version: "1.0.0", Description: "测试扩展",
			ToolDependencies: []string{"extension.read"},
		},
		"capabilities": []capability.CapabilitySpec{{
			ID: "extension.query", Version: "1.0.0", Name: "扩展查询",
			Description: "查询扩展", ServiceID: "extension",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: runtimeID, Version: "1.0.0",
		IdleTTLMS: 1000, Storage: storage, Extensions: extensions,
		Components: []packmgr.Component{{
			ID: runtimeID, Mode: loader.ModeHosted, Entrypoint: "runtime-artifact",
			Exports: []string{"extension.query"}, HostFunctions: decls,
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
	artifactDigest := sha256.Sum256([]byte("hosted artifact"))
	lock := packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion, PackageID: runtimeID,
		PackageVersion: "1.0.0",
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: []packmgr.LockedArtifact{{
			ComponentID: runtimeID, Path: artifact, SHA256: hex.EncodeToString(artifactDigest[:]),
		}},
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := catalog.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Runtime.ID != runtimeID+"."+runtimeID || records[0].PackageID != runtimeID {
		t.Fatalf("Discover records = %+v, want single %q record", records, runtimeID)
	}
	return records[0]
}

func TestInstalledCatalogAcceptsHostFunctionAndStorageDeclarations(t *testing.T) {
	root := t.TempDir()
	record := writeDeclaredFixture(t, root, "extension.decl", []packmgr.HostedFunctionDecl{
		{Module: "ailuo.extension", Name: "query", Purpose: "查询扩展权威存储"},
	}, &packmgr.Storage{
		Namespace: "ext/data", SchemaVersion: 1,
		Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent,
	})
	if len(record.Runtime.HostFunctions) != 1 ||
		record.Runtime.HostFunctions[0].Module != "ailuo.extension" || record.Runtime.HostFunctions[0].Name != "query" {
		t.Fatalf("record host functions = %+v, want ailuo.extension.query", record.Runtime.HostFunctions)
	}
	if record.Storage == nil || record.Storage.Namespace != "ext/data" || record.Storage.SchemaVersion != 1 {
		t.Fatalf("record storage = %+v, want ext/data v1", record.Storage)
	}
	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.VerifyRuntime(t.Context(), record.Runtime); err != nil {
		t.Fatalf("VerifyRuntime: %v", err)
	}
}

func TestInstalledCatalogRejectsInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name     string
		decls    []packmgr.HostedFunctionDecl
		storage  *packmgr.Storage
		hostedOK bool
	}{
		{name: "duplicate host function", decls: []packmgr.HostedFunctionDecl{
			{Module: "ailuo.x", Name: "one"}, {Module: "ailuo.x", Name: "one"},
		}, hostedOK: false},
		{name: "wasi module reserved", decls: []packmgr.HostedFunctionDecl{
			{Module: "wasi_snapshot_preview1", Name: "fd_write"},
		}, hostedOK: true},
		{name: "invalid module", decls: []packmgr.HostedFunctionDecl{
			{Module: "Ailuo.X", Name: "query"},
		}, hostedOK: true},
		{name: "zero schema version", storage: &packmgr.Storage{
			Namespace: "ext/data", SchemaVersion: 0,
			Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent,
		}, hostedOK: false},
		{name: "invalid sensitivity", storage: &packmgr.Storage{
			Namespace: "ext/data", SchemaVersion: 1,
			Sensitivity: "top_secret", Retention: packmgr.RetentionPermanent,
		}, hostedOK: true},
		{name: "invalid retention", storage: &packmgr.Storage{
			Namespace: "ext/data", SchemaVersion: 1,
			Sensitivity: packmgr.SensitivityPrivate, Retention: "forever",
		}, hostedOK: true},
		{name: "invalid namespace", storage: &packmgr.Storage{
			Namespace: "Ext/data", SchemaVersion: 1,
			Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent,
		}, hostedOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := packagesource.NewCatalog(root); err != nil {
				t.Fatal(err)
			}
			if tc.hostedOK {
				directory := filepath.Join(root, "extension.bad")
				if err := os.Mkdir(directory, 0o750); err != nil {
					t.Fatal(err)
				}
				artifact := filepath.Join(directory, "runtime-artifact")
				if err := os.WriteFile(artifact, []byte("hosted artifact"), 0o640); err != nil {
					t.Fatal(err)
				}
				extensions, err := json.Marshal(map[string]any{
					"tools": []capability.ToolSpec{{
						ID: "extension.read", Version: "1.0.0", Description: "读取扩展数据",
						InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
						SideEffect:      capability.SideEffectRead,
					}},
					"service": capability.ServiceSpec{ID: "extension", Version: "1.0.0", Description: "测试扩展"},
					"capabilities": []capability.CapabilitySpec{{
						ID: "extension.query", Version: "1.0.0", Name: "扩展查询",
						Description: "查询扩展", ServiceID: "extension",
						InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
						SideEffect:      capability.SideEffectRead,
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				installed := packmgr.Manifest{
					SchemaVersion: packmgr.SchemaVersion, ID: "extension.bad", Version: "1.0.0",
					Storage: tc.storage, Extensions: extensions,
					Components: []packmgr.Component{{
						ID: "extension.bad", Mode: loader.ModeHosted, Entrypoint: "runtime-artifact",
						Exports: []string{"extension.query"}, HostFunctions: tc.decls,
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
				artifactDigest := sha256.Sum256([]byte("hosted artifact"))
				lock := packmgr.Lock{
					SchemaVersion: packmgr.SchemaVersion, PackageID: "extension.bad",
					PackageVersion: "1.0.0",
					ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
					Artifacts: []packmgr.LockedArtifact{{
						ComponentID: "extension.bad", Path: artifact, SHA256: hex.EncodeToString(artifactDigest[:]),
					}},
				}
				lockBytes, err := json.Marshal(lock)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "lock.json"), lockBytes, 0o640); err != nil {
					t.Fatal(err)
				}
				catalog, err := packagesource.NewCatalog(root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := catalog.Discover(t.Context()); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
					t.Fatalf("Discover with invalid declarations error = %v, want ErrInstallCatalogInvalid", err)
				}
				return
			}
			// 通过 RegisterInstalled 路径校验内置包（无目录记录）。
			manager, err := loader.New(&fakeHost{})
			if err != nil {
				t.Fatal(err)
			}
			reg := registry.New()
			record := loader.InstalledRecord{
				Runtime: loader.Manifest{
					ID: "extension.builtin", Version: "1.0.0", Mode: loader.ModeHosted,
					Role: loader.RoleCapability, LockedDigest: digest,
					HostFunctions: tc.decls,
				},
				Tools: []capability.ToolSpec{{
					ID: "extension.read", Version: "1.0.0", Description: "读取扩展数据",
					InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
					SideEffect:      capability.SideEffectRead,
				}},
				Service: capability.ServiceSpec{ID: "extension", Version: "1.0.0", Description: "测试扩展"},
				Capabilities: []capability.CapabilitySpec{{
					ID: "extension.query", Version: "1.0.0", Name: "扩展查询",
					Description: "查询扩展", ServiceID: "extension",
					InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
					SideEffect:      capability.SideEffectRead,
				}},
				Storage: tc.storage,
			}
			if err := loader.RegisterInstalled(t.Context(), manager, reg, []loader.InstalledRecord{record}); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
				t.Fatalf("RegisterInstalled with invalid declarations error = %v, want ErrInstallCatalogInvalid", err)
			}
		})
	}
}
