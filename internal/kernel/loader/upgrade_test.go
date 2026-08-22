package loader_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// versionedHost 按清单版本返回对应运行时，未登记版本加载失败。
type versionedHost struct {
	runtimes map[string]*fakeRuntime
	mode     string
}

func (h *versionedHost) Mode() string {
	if h.mode == "" {
		return loader.ModeHosted
	}
	return h.mode
}

func (h *versionedHost) Verify(context.Context, loader.Manifest) error { return nil }

func (h *versionedHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	runtime, ok := h.runtimes[manifest.Version]
	if !ok {
		return nil, loader.ErrUnavailable
	}
	return runtime, nil
}

func upgradeManifest(id, version string) loader.Manifest {
	return loader.Manifest{
		ID: id, Version: version, Mode: loader.ModeHosted,
		Role: loader.RoleCapability, LockedDigest: digest,
	}
}

// registerSingleComponent 注册单组件包并记录分组。
func registerSingleComponent(t *testing.T, manager *loader.Manager, id, version string) {
	t.Helper()
	if err := manager.Register(context.Background(), upgradeManifest(id, version)); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterPackage(id, []string{id}); err != nil {
		t.Fatal(err)
	}
}

func upgradeSingleComponent(id, version string) loader.PackageSpec {
	return loader.PackageSpec{ID: id, Components: []loader.ComponentSpec{{Runtime: upgradeManifest(id, version)}}}
}

func TestManagerUpgradeSwitchesAtomicallyAndDrainsOldVersion(t *testing.T) {
	v1 := &fakeRuntime{description: loader.Description{ID: "extension.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	v2 := &fakeRuntime{description: loader.Description{ID: "extension.test", Version: "2.0.0", Mode: loader.ModeHosted}}
	host := &versionedHost{runtimes: map[string]*fakeRuntime{"1.0.0": v1, "2.0.0": v2}}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	registerSingleComponent(t, manager, "extension.test", "1.0.0")
	if err := manager.EnsureLoaded(ctx, "extension.test"); err != nil {
		t.Fatal(err)
	}
	if v1.starts.Load() != 1 {
		t.Fatalf("v1 starts = %d, want 1", v1.starts.Load())
	}
	// 持有旧版本租约：升级期间在途调用不中断。
	leaseA, err := manager.Acquire(ctx, "extension.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpgradePackage(ctx, upgradeSingleComponent("extension.test", "2.0.0")); err != nil {
		t.Fatalf("UpgradePackage: %v", err)
	}
	if v2.starts.Load() != 1 {
		t.Fatalf("v2 starts = %d, want 1", v2.starts.Load())
	}
	// 新租约打到新版本。
	leaseB, err := manager.Acquire(ctx, "extension.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaseB.Invoke(ctx, hostedTestRequest("extension.read"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if v2.invokes.Load() != 1 || v1.invokes.Load() != 0 {
		t.Fatalf("invokes v2=%d v1=%d, want v2=1 v1=0", v2.invokes.Load(), v1.invokes.Load())
	}
	// 旧版本在途租约未释放前不停止。
	if v1.stops.Load() != 0 {
		t.Fatalf("v1 stops = %d before drain, want 0", v1.stops.Load())
	}
	leaseB.Release()
	// 释放旧版本租约后，旧版本在途排空并停止（异步）。
	leaseA.Release()
	waitForCount(t, "v1 stops", func() int64 { return int64(v1.stops.Load()) }, 1)
	// 新版本继续可调用。
	leaseC, err := manager.Acquire(ctx, "extension.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaseC.Invoke(ctx, hostedTestRequest("extension.read"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	leaseC.Release()
	if v2.invokes.Load() != 2 {
		t.Fatalf("v2 invokes after upgrade = %d, want 2", v2.invokes.Load())
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if v2.stops.Load() != 1 {
		t.Fatalf("v2 stops = %d, want 1", v2.stops.Load())
	}
}

func TestManagerUpgradeCandidateFailureKeepsOldVersion(t *testing.T) {
	v1 := &fakeRuntime{description: loader.Description{ID: "extension.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	host := &versionedHost{runtimes: map[string]*fakeRuntime{"1.0.0": v1}}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := manager.Register(ctx, upgradeManifest("extension.test", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(ctx, "extension.test"); err != nil {
		t.Fatal(err)
	}
	// 候选版本宿主无法装载：升级失败，旧版本不受影响。
	if err := manager.UpgradePackage(ctx, upgradeSingleComponent("extension.test", "2.0.0")); err == nil {
		t.Fatal("UpgradePackage with failing candidate = nil, want error")
	}
	lease, err := manager.Acquire(ctx, "extension.test")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if _, err := lease.Invoke(ctx, hostedTestRequest("extension.read"), []byte(`{}`)); err != nil {
		t.Fatalf("old version must keep serving after failed upgrade: %v", err)
	}
	if v1.invokes.Load() != 1 {
		t.Fatalf("v1 invokes = %d, want 1", v1.invokes.Load())
	}
}

func TestManagerUpgradeRejectsInvalidTargets(t *testing.T) {
	v1 := &fakeRuntime{description: loader.Description{ID: "extension.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	host := &versionedHost{runtimes: map[string]*fakeRuntime{"1.0.0": v1}}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := manager.Register(ctx, upgradeManifest("extension.test", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterPackage("extension.test", []string{"extension.test"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(ctx, "extension.test"); err != nil {
		t.Fatal(err)
	}
	// 未注册包。
	if err := manager.UpgradePackage(ctx, upgradeSingleComponent("missing.test", "1.0.0")); !errors.Is(err, loader.ErrNotFound) {
		t.Fatalf("UpgradePackage unknown package error = %v, want ErrNotFound", err)
	}
	// 同版本升级无意义。
	if err := manager.UpgradePackage(ctx, upgradeSingleComponent("extension.test", "1.0.0")); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("UpgradePackage same version error = %v, want ErrInvalidManifest", err)
	}
	// 未就绪条目不可升级。
	if err := manager.Register(ctx, upgradeManifest("pending.test", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterPackage("pending.test", []string{"pending.test"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.UpgradePackage(ctx, upgradeSingleComponent("pending.test", "2.0.0")); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("UpgradePackage not-ready error = %v, want ErrUnavailable", err)
	}
}

// waitForCount 轮询等待原子计数器达到期望值。
func waitForCount(t *testing.T, what string, load func() int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if load() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", what, load(), want)
}
