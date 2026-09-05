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

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

const (
	busCoreRuntimeID    = "campus.bus.bus.core"
	busAdapterRuntimeID = "campus.bus.bus.adapter"
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

// writeMultiComponentFixture 构造 campus.bus 两组件包目录：
// bus.adapter（isolated，Provider，导出 transport）先于 bus.core（hosted，导入
// transport，导出 query）启动——依赖拓扑要求 Provider 在前。
func writeMultiComponentFixture(t *testing.T, root, version string) string {
	t.Helper()
	directory := filepath.Join(root, "campus.bus")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{
		"bus-adapter":   []byte("adapter-" + version),
		"bus-core.wasm": []byte("core-" + version),
	}
	for name, body := range artifacts {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	extensions, err := json.Marshal(map[string]any{
		"service": registry.ServiceSpec{
			ID: "campus", Version: version, Description: "校园服务",
		},
		"capabilities": []registry.CapabilitySpec{
			{ID: "campus.bus.query", Version: version, Name: "校巴查询", Description: "查询校巴",
				ServiceID: "campus", InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
				SideEffect: registry.SideEffectRead},
			{ID: "campus.bus.transport", Version: version, Name: "实时交通", Description: "实时校巴位置",
				ServiceID: "campus", InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
				SideEffect: registry.SideEffectRead},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: "campus.bus", Version: version,
		Extensions: extensions,
		Components: []packmgr.Component{
			{ID: "bus.core", Mode: loader.ModeHosted, Entrypoint: "bus-core.wasm",
				Exports: []string{"campus.bus.query"}, Imports: []string{"campus.bus.transport"}},
			{ID: "bus.adapter", Mode: loader.ModeIsolated, Entrypoint: "bus-adapter",
				Exports: []string{"campus.bus.transport"}},
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
	locked := make([]packmgr.LockedArtifact, 0, 2)
	for name, body := range artifacts {
		componentID := "bus.core"
		if name == "bus-adapter" {
			componentID = "bus.adapter"
		}
		path := filepath.Join(directory, name)
		digest := sha256.Sum256(body)
		artifact := packmgr.LockedArtifact{
			ComponentID: componentID, Path: path, SHA256: hex.EncodeToString(digest[:]),
		}
		if componentID == "bus.adapter" {
			artifact.Process = &packmgr.ProcessSpec{
				Path: path, WorkDir: directory, Address: "unix:" + filepath.Join(directory, "adapter.sock"),
			}
		}
		locked = append(locked, artifact)
	}
	lock, err := json.Marshal(packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion, PackageID: "campus.bus",
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

func TestCampusBusMultiComponentRoutesCapabilitiesAndUpgradesGroup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeMultiComponentFixture(t, root, "1.0.0")

	catalog, err := loader.NewCatalog(root)
	if err != nil {
		t.Fatal(err)
	}
	records, err := catalog.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("Discover records = %d, want 2 components", len(records))
	}
	orderByID := make(map[string]int, 2)
	for _, record := range records {
		if record.PackageID != "campus.bus" {
			t.Fatalf("record package = %q, want campus.bus", record.PackageID)
		}
		orderByID[record.ComponentID] = record.ComponentOrder
	}
	// 依赖拓扑：Provider（adapter）先于 consumer（core）。
	if orderByID["bus.adapter"] >= orderByID["bus.core"] {
		t.Fatalf("topo order adapter=%d core=%d, want adapter < core", orderByID["bus.adapter"], orderByID["bus.core"])
	}
	// Capability 按 exports 映射：query → core，transport → adapter。
	for _, record := range records {
		exported := map[string]bool{}
		for _, capability := range record.Capabilities {
			exported[capability.ID] = true
		}
		switch record.ComponentID {
		case "bus.core":
			if !exported["campus.bus.query"] || exported["campus.bus.transport"] {
				t.Fatalf("core capabilities = %v, want only campus.bus.query", record.Capabilities)
			}
		case "bus.adapter":
			if !exported["campus.bus.transport"] || exported["campus.bus.query"] {
				t.Fatalf("adapter capabilities = %v, want only campus.bus.transport", record.Capabilities)
			}
		}
	}

	// 注册：hosted/isolated 各一个宿主，同时提供 v1/v2 运行时。
	recorder := &stopRecorder{}
	core1 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: busCoreRuntimeID, Version: "1.0.0", Mode: loader.ModeHosted}},
		recorder:    recorder, id: "bus.core",
	}
	adapter1 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: busAdapterRuntimeID, Version: "1.0.0", Mode: loader.ModeIsolated}},
		recorder:    recorder, id: "bus.adapter",
	}
	core2 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: busCoreRuntimeID, Version: "2.0.0", Mode: loader.ModeHosted}},
		recorder:    recorder, id: "bus.core",
	}
	adapter2 := &recordingRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: busAdapterRuntimeID, Version: "2.0.0", Mode: loader.ModeIsolated}},
		recorder:    recorder, id: "bus.adapter",
	}
	manager, err := loader.New(
		&multiModeKeyedHost{mode: loader.ModeHosted, runtimes: map[string]loader.Runtime{
			busCoreRuntimeID + "@1.0.0": core1, busCoreRuntimeID + "@2.0.0": core2,
		}},
		&multiModeKeyedHost{mode: loader.ModeIsolated, runtimes: map[string]loader.Runtime{
			busAdapterRuntimeID + "@1.0.0": adapter1, busAdapterRuntimeID + "@2.0.0": adapter2,
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
		if _, err := handler(ctx, contracts.RequestContext{AppID: "app.campus"}, []byte(`{}`)); err != nil {
			t.Fatalf("invoke %s: %v", capabilityID, err)
		}
	}
	invoke("campus.bus.query")
	invoke("campus.bus.transport")
	if core1.invokes.Load() != 1 || adapter1.invokes.Load() != 1 {
		t.Fatalf("invokes core=%d adapter=%d, want both 1", core1.invokes.Load(), adapter1.invokes.Load())
	}

	// 组升级：持有旧组件租约 → 候选全绿后原子切换 → 旧版本反序 drain。
	leaseCore, err := manager.Acquire(ctx, busCoreRuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	leaseAdapter, err := manager.Acquire(ctx, busAdapterRuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpgradePackage(ctx, loader.PackageSpec{
		ID: "campus.bus",
		Components: []loader.ComponentSpec{
			{Runtime: loader.Manifest{ID: busAdapterRuntimeID, Version: "2.0.0", Mode: loader.ModeIsolated,
				Role: loader.RoleCapability, LockedDigest: digest}},
			{Runtime: loader.Manifest{ID: busCoreRuntimeID, Version: "2.0.0", Mode: loader.ModeHosted,
				Role: loader.RoleCapability, LockedDigest: digest}},
		},
	}); err != nil {
		t.Fatalf("UpgradePackage: %v", err)
	}
	// 新租约打到 v2。
	for id, runtime := range map[string]loader.Runtime{busCoreRuntimeID: core2, busAdapterRuntimeID: adapter2} {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lease.Invoke(ctx, hostedTestRequest("campus.read"), []byte(`{}`)); err != nil {
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
	if len(seq) != 2 || seq[0] != "bus.core" || seq[1] != "bus.adapter" {
		t.Fatalf("stop order = %v, want [bus.core bus.adapter]", seq)
	}
}
