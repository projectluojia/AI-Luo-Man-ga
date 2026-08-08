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

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestInstalledCatalogDiscoversVerifiesAndRegistersHostedRuntime(t *testing.T) {
	root := t.TempDir()
	artifact := writeInstalledFixture(t, root, "extension.test", loader.ModeHosted, false)
	catalog, err := loader.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := catalog.Discover(t.Context())
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	record := records[0]
	if record.Runtime.ID != "extension.test" || record.Runtime.Mode != loader.ModeHosted ||
		record.Process != nil || record.Service.ID != "extension" ||
		len(record.Tools) != 1 || len(record.Capabilities) != 1 {
		t.Fatalf("record=%#v", record)
	}
	runtime := &fakeRuntime{description: loader.Description{
		ID: record.Runtime.ID, Version: record.Runtime.Version, Mode: record.Runtime.Mode,
	}}
	manager, err := loader.New(map[string]loader.Host{
		loader.ModeHosted: &fakeHost{runtime: runtime},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := loader.RegisterInstalled(manager, reg, records); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := manager.Snapshot(record.Runtime.ID); err != nil || snapshot.State != loader.StateRegistered {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
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
	catalog, err := loader.NewCatalog(root)
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
	catalog, _ := loader.NewCatalog(root)
	records, err := catalog.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(map[string]loader.Host{
		loader.ModeHosted: &fakeHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	existing := records[0].Tools[0]
	if err := reg.RegisterTool(registry.ToolRegistration{Spec: existing, Handler: noopCatalogHandler}); err != nil {
		t.Fatal(err)
	}
	if err := loader.RegisterInstalled(manager, reg, records); !errors.Is(err, registry.ErrDuplicateID) {
		t.Fatalf("冲突注册错误=%v", err)
	}
	if _, err := manager.Snapshot(records[0].Runtime.ID); !errors.Is(err, loader.ErrNotFound) {
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
	duplicate := append(manifest[:len(manifest)-1], []byte(`,"schema_version":"ailuo.install.v2"}`)...)
	if err := os.WriteFile(manifestPath, duplicate, 0o640); err != nil {
		t.Fatal(err)
	}
	rewriteManifestDigest(t, directory, duplicate)
	catalog, _ := loader.NewCatalog(root)
	if _, err := catalog.Discover(t.Context()); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
		t.Fatalf("重复 JSON 键错误=%v", err)
	}

	root = t.TempDir()
	writeInstalledFixture(t, root, "extension.test", loader.ModeHosted, false)
	if err := os.Chmod(filepath.Join(root, "extension.test"), 0o770); err != nil {
		t.Fatal(err)
	}
	catalog, _ = loader.NewCatalog(root)
	if _, err := catalog.Discover(t.Context()); !errors.Is(err, loader.ErrInstallCatalogInvalid) {
		t.Fatalf("可写安装目录错误=%v", err)
	}
}

func writeInstalledFixture(t *testing.T, root, runtimeID, mode string, unknown bool) string {
	t.Helper()
	directory := filepath.Join(root, runtimeID)
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
	installed := loader.InstalledManifest{
		SchemaVersion: loader.InstallSchemaVersion,
		Runtime: loader.InstalledRuntimeSpec{
			ID: runtimeID, Version: "1.0.0", Mode: mode, IdleTTLMS: 1000,
		},
		Tools: []registry.ToolSpec{{
			ID: "extension.read", Version: "1.0.0", Description: "读取扩展数据",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      registry.SideEffectRead,
		}},
		Service: registry.ServiceSpec{
			ID: "extension", Version: "1.0.0", Description: "测试扩展",
			ToolDependencies: []string{"extension.read"},
		},
		Capabilities: []registry.CapabilitySpec{{
			ID: "extension.query", Version: "1.0.0", Name: "扩展查询",
			Description: "查询扩展", ServiceID: "extension",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      registry.SideEffectRead,
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
	lock := loader.InstalledLock{
		SchemaVersion: loader.InstallSchemaVersion, RuntimeID: runtimeID,
		RuntimeVersion: "1.0.0", Mode: mode,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		ArtifactSHA256: hex.EncodeToString(artifactDigest[:]), ArtifactPath: artifact,
	}
	if mode == loader.ModeIsolated {
		lock.Process = &loader.InstalledProcessSpec{
			Path: artifact, WorkDir: directory, Address: "unix:" + filepath.Join(directory, "runtime.sock"),
		}
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
	var lock loader.InstalledLock
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
