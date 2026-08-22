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
	leaseA.Release()
	waitForCount(t, "a stops", func() int64 { return int64(a1.stops.Load()) }, 1)
	waitForCount(t, "b stops", func() int64 { return int64(b1.stops.Load()) }, 1)
	// 停止顺序：消费者 b 先停，Provider a 后停。
	seq := recorder.snapshot()
	if len(seq) != 2 || seq[0] != "pkg.test.b" || seq[1] != "pkg.test.a" {
		t.Fatalf("stop order = %v, want [pkg.test.b pkg.test.a]", seq)
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
