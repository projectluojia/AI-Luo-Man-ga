//go:build unix

package loader_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	multiCoreRuntimeID    = "test.multi.multi.core"
	multiAdapterRuntimeID = "test.multi.multi.adapter"
)

// multiModeKeyedHost 按 mode 过滤、按 组件ID@版本 返回运行时。
type multiModeKeyedHost struct {
	mode     string
	runtimes map[string]loader.Runtime
}

func (h *multiModeKeyedHost) Mode() string { return h.mode }

func (h *multiModeKeyedHost) Verify(_ context.Context, manifest loader.Manifest) error {
	if manifest.Mode != h.mode {
		return loader.ErrUnsupportedMode
	}
	return nil
}

func (h *multiModeKeyedHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	runtime, ok := h.runtimes[manifest.ID+"@"+manifest.Version]
	if !ok {
		return nil, loader.ErrUnavailable
	}
	return runtime, nil
}

// writeMultiComponentFixture 构造 test.multi 两组件包目录：
// multi.adapter（isolated，Provider，导出 transport）先于 multi.core（hosted，导出
// query）启动——组件顺序由 Package 清单显式声明。
func writeMultiComponentFixture(t *testing.T, root, version string) string {
	t.Helper()
	directory := filepath.Join(root, "test.multi")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{
		"multi-adapter":   []byte("adapter-" + version),
		"multi-core.wasm": []byte("core-" + version),
	}
	for name, body := range artifacts {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	installed := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "test.multi", Version: version,
		Capabilities: []capability.CapabilitySpec{
			{ID: "test.multi.query", Version: version, Name: "基础查询", Description: "多组件基础查询",
				InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
				SideEffect:      capability.SideEffectRead},
			{ID: "test.multi.transport", Version: version, Name: "传输", Description: "多组件传输",
				InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
				SideEffect:      capability.SideEffectRead},
		},
		Components: []packagecontract.Component{
			{ID: "multi.adapter", Mode: loader.ModeIsolated, Role: packagecontract.RoleProvider, Entrypoint: "multi-adapter",
				Process: &packagecontract.ProcessTemplate{Path: "multi-adapter", Address: "127.0.0.1:50051"},
				Exports: []string{"test.multi.transport"}},
			{ID: "multi.core", Mode: loader.ModeHosted, Role: packagecontract.RoleProvider, Entrypoint: "multi-core.wasm",
				Exports: []string{"test.multi.query"}},
		},
	}
	manifest, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), manifest, 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(manifest)
	locked := make([]packagecontract.LockedArtifact, 0, 2)
	for name, body := range artifacts {
		componentID := "multi.core"
		if name == "multi-adapter" {
			componentID = "multi.adapter"
		}
		path := filepath.Join(directory, name)
		digest := sha256.Sum256(body)
		artifact := packagecontract.LockedArtifact{
			ComponentID: componentID, Path: path, SHA256: hex.EncodeToString(digest[:]),
		}
		if componentID == "multi.adapter" {
			// 地址必须与组件声明的 ProcessTemplate 一致：安装器原样保留模板地址。
			artifact.Process = &packagecontract.ProcessSpec{
				Path: path, WorkDir: directory, Address: "127.0.0.1:50051",
			}
		}
		locked = append(locked, artifact)
	}
	lock, err := json.Marshal(packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: "test.multi",
		PackageVersion: version, ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts: locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "lock.json"), lock, 0o640); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestMultiComponentPackageRoutesCapabilitiesAndUpgradesGroup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeMultiComponentFixture(t, root, "1.0.0")

	catalog, err := packagesource.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := discoverCatalogLocked(t, catalog, root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("Discover records = %d, want 2 components", len(records))
	}
	orderByID := make(map[string]int, 2)
	for _, record := range records {
		if record.PackageID != "test.multi" {
			t.Fatalf("record package = %q, want test.multi", record.PackageID)
		}
		orderByID[record.ComponentID] = record.ComponentOrder
	}
	// 清单顺序：Provider（adapter）先于 core。
	if orderByID["multi.adapter"] >= orderByID["multi.core"] {
		t.Fatalf("topo order adapter=%d core=%d, want adapter < core", orderByID["multi.adapter"], orderByID["multi.core"])
	}
	// Capability 按 exports 映射：query → core，transport → adapter。
	for _, record := range records {
		exported := map[string]bool{}
		for _, capability := range record.Runtime.Capabilities {
			exported[capability.ID] = true
		}
		switch record.ComponentID {
		case "multi.core":
			if !exported["test.multi.query"] || exported["test.multi.transport"] {
				t.Fatalf("core capabilities = %v, want only test.multi.query", record.Runtime.Capabilities)
			}
		case "multi.adapter":
			if !exported["test.multi.transport"] || exported["test.multi.query"] {
				t.Fatalf("adapter capabilities = %v, want only test.multi.transport", record.Runtime.Capabilities)
			}
		}
	}

	// 注册：hosted/isolated 各一个宿主，同时提供 v1/v2 运行时。
	recorder := &stopRecorder{}
	core1 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: multiCoreRuntimeID, Version: "1.0.0", Mode: loader.ModeHosted}},
		recorder:    recorder, id: "multi.core",
	}
	adapter1 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: multiAdapterRuntimeID, Version: "1.0.0", Mode: loader.ModeIsolated}},
		recorder:    recorder, id: "multi.adapter",
	}
	core2 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: multiCoreRuntimeID, Version: "2.0.0", Mode: loader.ModeHosted}},
		recorder:    recorder, id: "multi.core",
	}
	adapter2 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: multiAdapterRuntimeID, Version: "2.0.0", Mode: loader.ModeIsolated}},
		recorder:    recorder, id: "multi.adapter",
	}
	manager, err := loader.New(
		&multiModeKeyedHost{mode: loader.ModeHosted, runtimes: map[string]loader.Runtime{
			multiCoreRuntimeID + "@1.0.0": core1, multiCoreRuntimeID + "@2.0.0": core2,
		}},
		&multiModeKeyedHost{mode: loader.ModeIsolated, runtimes: map[string]loader.Runtime{
			multiAdapterRuntimeID + "@1.0.0": adapter1, multiAdapterRuntimeID + "@2.0.0": adapter2,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	if err := loader.RegisterInstalled(ctx, manager, reg, records); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}

	// 能力路由：query 落到 core，transport 落到 adapter。
	invoke := func(capabilityID string) {
		t.Helper()
		_, handler, err := reg.ResolveCapability(capabilityID)
		if err != nil {
			t.Fatalf("ResolveCapability(%s): %v", capabilityID, err)
		}
		if _, err := handler(ctx, contracts.RequestContext{AppID: "app.test"}, []byte(`{}`)); err != nil {
			t.Fatalf("invoke %s: %v", capabilityID, err)
		}
	}
	invoke("test.multi.query")
	invoke("test.multi.transport")
	if core1.invokes.Load() != 1 || adapter1.invokes.Load() != 1 {
		t.Fatalf("invokes core=%d adapter=%d, want both 1", core1.invokes.Load(), adapter1.invokes.Load())
	}

	// 组升级：持有旧组件租约 → 候选全绿后原子切换 → 旧版本反序 drain。
	leaseCore, err := manager.Acquire(ctx, multiCoreRuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	leaseAdapter, err := manager.Acquire(ctx, multiAdapterRuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpgradePackage(ctx, loader.PackageSpec{
		ID: "test.multi",
		Components: []loader.ComponentSpec{
			{Runtime: loader.Manifest{ID: multiAdapterRuntimeID, Version: "2.0.0", Mode: loader.ModeIsolated,
				Role: loader.RoleProvider, LockedDigest: digest}},
			{Runtime: loader.Manifest{ID: multiCoreRuntimeID, Version: "2.0.0", Mode: loader.ModeHosted,
				Role: loader.RoleProvider, LockedDigest: digest}},
		},
	}); err != nil {
		t.Fatalf("UpgradePackage: %v", err)
	}
	// 新租约打到 v2。
	for id, runtime := range map[string]loader.Runtime{multiCoreRuntimeID: core2, multiAdapterRuntimeID: adapter2} {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lease.Invoke(ctx, hostedTestRequest("test.read"), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		if runtime.(*recordingRuntime).invokes.Load() != 1 {
			t.Fatalf("%s v2 invokes = %d, want 1", id, runtime.(*recordingRuntime).invokes.Load())
		}
	}
	// 旧版本在途未释放前都不停止。
	if core1.stops.Load() != 0 || adapter1.stops.Load() != 0 {
		t.Fatalf("old runtimes stopped before group drain")
	}
	leaseCore.Release()
	leaseAdapter.Release()
	waitForCount(t, "core stops", func() int64 { return int64(core1.stops.Load()) }, 1)
	waitForCount(t, "adapter stops", func() int64 { return int64(adapter1.stops.Load()) }, 1)
	// 反序：消费者 core 先停，Provider adapter 后停。
	seq := recorder.snapshot()
	if len(seq) != 2 || seq[0] != "multi.core" || seq[1] != "multi.adapter" {
		t.Fatalf("stop order = %v, want [multi.core multi.adapter]", seq)
	}
}
