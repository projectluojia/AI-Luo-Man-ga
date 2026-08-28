package loader_test

import (
	"context"
	"sync"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

// keyedVersionHost 按 组件ID@版本 返回运行时；未登记加载失败。
type keyedVersionHost struct {
	runtimes map[string]loader.Runtime
}

func (h *keyedVersionHost) Mode() string { return loader.ModeHosted }

func (h *keyedVersionHost) Verify(context.Context, loader.Manifest) error { return nil }

func (h *keyedVersionHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	runtime, ok := h.runtimes[manifest.ID+"@"+manifest.Version]
	if !ok {
		return nil, loader.ErrUnavailable
	}
	return runtime, nil
}

// stopRecorder 记录停止顺序（验证反序 drain）。
type stopRecorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *stopRecorder) record(id string) {
	r.mu.Lock()
	r.seq = append(r.seq, id)
	r.mu.Unlock()
}

func (r *stopRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seq...)
}

// recordingRuntime 在停止时记录顺序。
type recordingRuntime struct {
	*fakeRuntime
	recorder *stopRecorder
	id       string
}

type warmupHost struct {
	runtimes map[string]loader.Runtime
}

func (h *warmupHost) Mode() string { return loader.ModeHosted }

func (h *warmupHost) Verify(context.Context, loader.Manifest) error { return nil }

func (h *warmupHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	runtime, ok := h.runtimes[manifest.ID]
	if !ok {
		return nil, loader.ErrUnavailable
	}
	return runtime, nil
}

type warmupOrder struct {
	mu  sync.Mutex
	ids []string
}

type warmupRuntime struct {
	*fakeRuntime
	order *warmupOrder
	id    string
}

func (r *warmupRuntime) Start(ctx context.Context) error {
	r.order.mu.Lock()
	r.order.ids = append(r.order.ids, r.id)
	r.order.mu.Unlock()
	return r.fakeRuntime.Start(ctx)
}

func (r *recordingRuntime) Stop(ctx context.Context) error {
	r.recorder.record(r.id)
	return r.fakeRuntime.Stop(ctx)
}

func TestUpgradePackageDrainsGroupInReverseOrder(t *testing.T) {
	ctx := context.Background()
	recorder := &stopRecorder{}
	newRuntime := func(id, version string) *recordingRuntime {
		return &recordingRuntime{
			fakeRuntime: &fakeRuntime{description: loader.Description{ID: id, Version: version, Mode: loader.ModeHosted}},
			recorder:    recorder, id: id,
		}
	}
	a1, b1 := newRuntime("pkg.test.a", "1.0.0"), newRuntime("pkg.test.b", "1.0.0")
	a2, b2 := newRuntime("pkg.test.a", "2.0.0"), newRuntime("pkg.test.b", "2.0.0")
	host := &keyedVersionHost{runtimes: map[string]loader.Runtime{
		"pkg.test.a@1.0.0": a1, "pkg.test.b@1.0.0": b1,
		"pkg.test.a@2.0.0": a2, "pkg.test.b@2.0.0": b2,
	}}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(ctx, upgradeManifest("pkg.test.a", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(ctx, upgradeManifest("pkg.test.b", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	// 依赖拓扑：a（provider）在前，b（consumer）在后。
	if err := manager.RegisterPackage("pkg.test", []string{"pkg.test.a", "pkg.test.b"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pkg.test.a", "pkg.test.b"} {
		if err := manager.EnsureLoaded(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// 持有两个旧组件租约。
	leaseA, err := manager.Acquire(ctx, "pkg.test.a")
	if err != nil {
		t.Fatal(err)
	}
	leaseA2, err := manager.Acquire(ctx, "pkg.test.a")
	if err != nil {
		t.Fatal(err)
	}
	leaseB, err := manager.Acquire(ctx, "pkg.test.b")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UpgradePackage(ctx, loader.PackageSpec{
		ID: "pkg.test",
		Components: []loader.ComponentSpec{
			{Runtime: upgradeManifest("pkg.test.a", "2.0.0")},
			{Runtime: upgradeManifest("pkg.test.b", "2.0.0")},
		},
	}); err != nil {
		t.Fatalf("UpgradePackage: %v", err)
	}
	// 新租约打到新版本。
	for id, runtime := range map[string]*fakeRuntime{"pkg.test.a": a2.fakeRuntime, "pkg.test.b": b2.fakeRuntime} {
		lease, err := manager.Acquire(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lease.Invoke(ctx, hostedTestRequest("pkg.read"), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		lease.Release()
		if runtime.invokes.Load() != 1 {
			t.Fatalf("%s v2 invokes = %d, want 1", id, runtime.invokes.Load())
		}
	}
	// 旧版本在途租约未释放前都不停止（组级 drain）。
	if a1.stops.Load() != 0 || b1.stops.Load() != 0 {
		t.Fatalf("old runtimes stopped before group drain: a=%d b=%d", a1.stops.Load(), b1.stops.Load())
	}
	leaseB.Release()
	leaseA2.Release()
	leaseA.Release()
	waitForCount(t, "a stops", func() int64 { return int64(a1.stops.Load()) }, 1)
	waitForCount(t, "b stops", func() int64 { return int64(b1.stops.Load()) }, 1)
	// 停止顺序：消费者 b 先停，Provider a 后停。
	seq := recorder.snapshot()
	if len(seq) != 2 || seq[0] != "pkg.test.b" || seq[1] != "pkg.test.a" {
		t.Fatalf("stop order = %v, want [pkg.test.b pkg.test.a]", seq)
	}
}

func TestWarmupLoadsPinnedPackageInTopologyOrder(t *testing.T) {
	order := &warmupOrder{}
	provider := &warmupRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: "pkg.warm.provider", Version: "1.0.0", Mode: loader.ModeHosted}},
		order:       order, id: "provider",
	}
	consumer := &warmupRuntime{
		fakeRuntime: &fakeRuntime{description: loader.Description{ID: "pkg.warm.consumer", Version: "1.0.0", Mode: loader.ModeHosted}},
		order:       order, id: "consumer",
	}
	manager, err := loader.New(&warmupHost{runtimes: map[string]loader.Runtime{
		"pkg.warm.provider": provider, "pkg.warm.consumer": consumer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := manager.Register(ctx, loader.Manifest{ID: "pkg.warm.provider", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest, Pin: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(ctx, loader.Manifest{ID: "pkg.warm.consumer", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest, Pin: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterPackage("pkg.warm", []string{"pkg.warm.provider", "pkg.warm.consumer"}); err != nil {
		t.Fatal(err)
	}
	pinned := manager.Pinned()
	if err := manager.Warmup(ctx, pinned, 2); err != nil {
		t.Fatal(err)
	}
	order.mu.Lock()
	got := append([]string(nil), order.ids...)
	order.mu.Unlock()
	if len(got) != 2 || got[0] != "provider" || got[1] != "consumer" {
		t.Fatalf("warmup order = %v, want [provider consumer]", got)
	}
}

func TestRegisterPackageRejectsDuplicate(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "pkg.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	manager, err := loader.New(&fakeHost{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := manager.Register(ctx, upgradeManifest("pkg.test", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterPackage("pkg.test", []string{"pkg.test"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterPackage("pkg.test", []string{"pkg.test"}); err == nil {
		t.Fatal("RegisterPackage duplicate = nil, want error")
	}
}
