package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	kernelruntime "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeHost struct {
	// mode 声明宿主服务的模式；空值按 ModeHosted 处理（多数用例单一模式）。
	mode      string
	verifyErr error
	loadErr   error
	runtime   loader.Runtime
	loadGate  chan struct{}
	verifies  atomic.Int32
	loads     atomic.Int32
}

func (h *fakeHost) Mode() string {
	if h.mode == "" {
		return loader.ModeHosted
	}
	return h.mode
}

func (h *fakeHost) Verify(ctx context.Context, manifest loader.Manifest) error {
	h.verifies.Add(1)
	if h.mode != "" && manifest.Mode != h.mode {
		return loader.ErrUnsupportedMode
	}
	return h.verifyErr
}

func (h *fakeHost) Load(ctx context.Context, _ loader.Manifest) (loader.Runtime, error) {
	h.loads.Add(1)
	if h.loadGate != nil {
		select {
		case <-h.loadGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.runtime, h.loadErr
}

type fakeRuntime struct {
	description loader.Description
	startErr    error
	healthErr   error
	invokeErr   error
	stopErr     error
	starts      atomic.Int32
	health      atomic.Int32
	invokes     atomic.Int32
	stops       atomic.Int32
}

func (r *fakeRuntime) Describe(context.Context) (loader.Description, error) {
	return r.description, nil
}

func (r *fakeRuntime) Start(context.Context) error {
	r.starts.Add(1)
	return r.startErr
}

func (r *fakeRuntime) Health(context.Context) error {
	r.health.Add(1)
	return r.healthErr
}

func (r *fakeRuntime) Invoke(_ context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	r.invokes.Add(1)
	if r.invokeErr != nil {
		return nil, r.invokeErr
	}
	return json.Marshal(map[string]any{"app_id": request.AppID, "payload_bytes": len(payload)})
}

func (r *fakeRuntime) Stop(context.Context) error {
	r.stops.Add(1)
	return r.stopErr
}

func TestLoaderSingleFlightsFirstUseAndDrainsBeforeShutdown(t *testing.T) {
	gate := make(chan struct{})
	runtime := &fakeRuntime{description: loader.Description{ID: "extension.test", Version: "1.2.3", Mode: loader.ModeHosted}}
	host := &fakeHost{runtime: runtime, loadGate: gate}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "extension.test", Version: "1.2.3", Mode: loader.ModeHosted,
		Role: loader.RoleCapability, LockedDigest: digest, IdleTTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	const callers = 20
	leases := make(chan *loader.Lease, callers)
	failures := make(chan error, callers)
	var workers sync.WaitGroup
	for index := 0; index < callers; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			lease, acquireErr := manager.Acquire(context.Background(), "extension.test")
			if acquireErr != nil {
				failures <- acquireErr
				return
			}
			leases <- lease
		}()
	}
	deadline := time.Now().Add(time.Second)
	for host.loads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	workers.Wait()
	close(leases)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	// 注册期绑定宿主一次 Verify，加载期 loadRuntime 再 Verify 一次：单飞加载只发生一次。
	if host.verifies.Load() != 2 || host.loads.Load() != 1 || runtime.starts.Load() != 1 || runtime.health.Load() != 1 {
		t.Fatalf("verify=%d load=%d start=%d health=%d", host.verifies.Load(), host.loads.Load(), runtime.starts.Load(), runtime.health.Load())
	}
	held := make([]*loader.Lease, 0, callers)
	for lease := range leases {
		held = append(held, lease)
	}
	if len(held) != callers {
		t.Fatalf("leases=%d", len(held))
	}
	// 在途租约阻塞卸载：Shutdown 必须等待排空后才能停止运行时。
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- manager.Shutdown(context.Background()) }()
	select {
	case <-shutdownDone:
		t.Fatal("in-flight lease must block shutdown")
	case <-time.After(50 * time.Millisecond):
	}
	for _, lease := range held {
		lease.Release()
		lease.Release()
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if runtime.stops.Load() != 1 {
		t.Fatalf("stops=%d", runtime.stops.Load())
	}
}

// serveHost 按 (模式, 服务 ID 集合) 精确承载清单，用于验证同一模式多宿主
// 场景下注册期按 Verify 绑定唯一宿主。
type serveHost struct {
	mode    string
	ids     map[string]bool
	runtime *fakeRuntime
	loads   atomic.Int32
}

func (h *serveHost) Mode() string { return h.mode }

func (h *serveHost) Verify(_ context.Context, manifest loader.Manifest) error {
	if manifest.Mode != h.mode || !h.ids[manifest.ID] {
		return loader.ErrUnsupportedMode
	}
	return nil
}

func (h *serveHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	h.loads.Add(1)
	if h.runtime != nil {
		return h.runtime, nil
	}
	return &fakeRuntime{description: loader.Description{
		ID: manifest.ID, Version: manifest.Version, Mode: manifest.Mode,
	}}, nil
}

// TestLoaderBindsManifestToTheOnlyVerifyingHost 验证同一模式多个宿主时，
// 每个清单在注册期精确绑定到唯一能 Verify 它的宿主，加载只走绑定宿主。
func TestLoaderBindsManifestToTheOnlyVerifyingHost(t *testing.T) {
	t.Parallel()
	first := &fakeRuntime{description: loader.Description{ID: "hosted.first", Version: "1.0.0", Mode: loader.ModeHosted}}
	second := &fakeRuntime{description: loader.Description{ID: "hosted.second", Version: "1.0.0", Mode: loader.ModeHosted}}
	hostA := &serveHost{mode: loader.ModeHosted, ids: map[string]bool{"hosted.first": true}, runtime: first}
	hostB := &serveHost{mode: loader.ModeHosted, ids: map[string]bool{"hosted.second": true}, runtime: second}
	manager, err := loader.New(hostA, hostB)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, manifest := range []loader.Manifest{
		{ID: "hosted.first", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest},
		{ID: "hosted.second", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest},
	} {
		if err := manager.Register(ctx, manifest); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"hosted.first", "hosted.second"} {
		if err := manager.EnsureLoaded(ctx, id); err != nil {
			t.Fatalf("EnsureLoaded %s: %v", id, err)
		}
	}
	if hostA.loads.Load() != 1 || hostB.loads.Load() != 1 {
		t.Fatalf("loads hostA=%d hostB=%d, want 1/1", hostA.loads.Load(), hostB.loads.Load())
	}
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestLoaderRejectsAmbiguousAndUnservedManifests 验证注册期 fail-closed：
// 同一模式多个宿主都通过 Verify（歧义）或都没有宿主能承载（未服务）都拒绝注册。
func TestLoaderRejectsAmbiguousAndUnservedManifests(t *testing.T) {
	t.Parallel()
	hostA := &serveHost{mode: loader.ModeHosted, ids: map[string]bool{"shared": true}}
	hostB := &serveHost{mode: loader.ModeHosted, ids: map[string]bool{"shared": true}}
	manager, err := loader.New(hostA, hostB)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ambiguous := loader.Manifest{ID: "shared", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest}
	if err := manager.Register(ctx, ambiguous); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("ambiguous manifest error=%v, want ErrInvalidManifest", err)
	}
	unserved := loader.Manifest{ID: "nobody", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest}
	if err := manager.Register(ctx, unserved); !errors.Is(err, loader.ErrUnsupportedMode) {
		t.Fatalf("unserved manifest error=%v, want ErrUnsupportedMode", err)
	}
	unknownMode := loader.Manifest{ID: "remote.one", Version: "1.0.0", Mode: "remote", LockedDigest: digest}
	if err := manager.Register(ctx, unknownMode); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("unknown mode error=%v, want ErrInvalidManifest", err)
	}
}

func TestLoaderVerifiesLockBeforeLoadAndFailsFast(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "isolated.test", Version: "1.0.0", Mode: loader.ModeIsolated}}
	host := &fakeHost{mode: loader.ModeIsolated, runtime: runtime}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "isolated.test", Version: "1.0.0", Mode: loader.ModeIsolated, Role: loader.RoleCapability, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	// 注册时 Verify 绑定宿主一次；加载前篡改锁，Load 前的二次 Verify 必须失败。
	host.verifyErr = errors.New("digest mismatch")
	if err := manager.EnsureLoaded(context.Background(), "isolated.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("load error=%v", err)
	}
	if err := manager.EnsureLoaded(context.Background(), "isolated.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("fast failure=%v", err)
	}
	if host.verifies.Load() != 2 || host.loads.Load() != 0 {
		t.Fatalf("verify=%d load=%d", host.verifies.Load(), host.loads.Load())
	}
	// 失败句柄通过 RecoverFailed 清理后，下一次调用重新执行完整加载流程。
	if err := manager.RecoverFailed(context.Background(), "isolated.test"); err != nil {
		t.Fatal(err)
	}
	host.verifyErr = nil
	if err := manager.EnsureLoaded(context.Background(), "isolated.test"); err != nil {
		t.Fatal(err)
	}
	if host.verifies.Load() != 3 || host.loads.Load() != 1 {
		t.Fatalf("retry verify=%d load=%d", host.verifies.Load(), host.loads.Load())
	}
}

func TestLoaderRegisterBatchIsAtomic(t *testing.T) {
	t.Parallel()

	manager, err := loader.New(&serveHost{mode: loader.ModeHosted, ids: map[string]bool{"batch.one": true, "batch.two": true}})
	if err != nil {
		t.Fatal(err)
	}
	valid := loader.Manifest{
		ID: "batch.one", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}
	invalid := loader.Manifest{
		ID: "batch.two", Version: "not-semver", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}
	if err := manager.RegisterBatch(context.Background(), []loader.Manifest{valid, invalid}); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("无效批次错误=%v", err)
	}
	if err := manager.EnsureLoaded(context.Background(), valid.ID); !errors.Is(err, loader.ErrNotFound) {
		t.Fatalf("失败批次部分发布：%v", err)
	}
	if err := manager.RegisterBatch(context.Background(), []loader.Manifest{valid, {
		ID: "batch.two", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"batch.one", "batch.two"} {
		if err := manager.EnsureLoaded(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoaderRejectsDescriptionMismatchAndStopsLoadedRuntime(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "different", Version: "1.0.0", Mode: loader.ModeHosted}}
	manager, err := loader.New(&fakeHost{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "expected", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "expected"); !errors.Is(err, loader.ErrDescribeMismatch) {
		t.Fatalf("description mismatch error=%v", err)
	}
	if runtime.starts.Load() != 0 || runtime.stops.Load() != 1 {
		t.Fatalf("starts=%d stops=%d", runtime.starts.Load(), runtime.stops.Load())
	}
}

func TestLoaderRetainsHandleWhenFailedLoadCannotStop(t *testing.T) {
	runtime := &fakeRuntime{
		description: loader.Description{ID: "different", Version: "1.0.0", Mode: loader.ModeHosted},
		stopErr:     errors.New("stop failed"),
	}
	manager, err := loader.New(&fakeHost{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "cleanup.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "cleanup.test"); !errors.Is(err, loader.ErrDescribeMismatch) {
		t.Fatalf("load error=%v", err)
	}
	// 失败句柄在 Stop 失败期间被保留：RecoverFailed 必须透出清理错误而非丢失句柄。
	if err := manager.RecoverFailed(context.Background(), "cleanup.test"); err == nil {
		t.Fatal("cleanup failure must surface")
	}
	runtime.stopErr = nil
	if err := manager.RecoverFailed(context.Background(), "cleanup.test"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.stops.Load() != 3 {
		t.Fatalf("stops=%d", runtime.stops.Load())
	}
}

func TestLoaderHandlerPreservesGovernedContextAndPin(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "hosted.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	manager, err := loader.New(&fakeHost{runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "hosted.test", Version: "1.0.0", Mode: loader.ModeHosted,
		Role: loader.RoleCapability, LockedDigest: digest, Pin: true,
	}); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler("hosted.test")
	result, err := handler(context.Background(), contracts.RequestContext{
		AppID: "app", EchoID: "echo", RequestID: "request", Deadline: time.Now().Add(time.Minute),
	}, json.RawMessage(`{"value":1}`))
	if err != nil || string(result) != `{"app_id":"app","payload_bytes":11}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.stops.Load() != 1 {
		t.Fatalf("stops=%d", runtime.stops.Load())
	}
}

func TestLoaderHandlerRemainsBehindRegistryDispatcherGovernance(t *testing.T) {
	loaded := &fakeRuntime{description: loader.Description{ID: "runtime.capability", Version: "1.0.0", Mode: loader.ModeHosted}}
	host := &fakeHost{runtime: loaded}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "runtime.capability", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable("app", "lazy.capability")
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: capability.ServiceSpec{ID: "lazy", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			"lazy.capability": {
				Spec: capability.CapabilitySpec{
					ID: "lazy.capability", Version: "1.0.0", Name: "懒加载测试",
					Description: "验证统一 Dispatcher 治理", ServiceID: "lazy",
					InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`,
					SideEffect:      capability.SideEffectRead,
				},
				Handler: manager.Handler("runtime.capability"),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := kernelruntime.NewDispatcher(reg, policy, kernelruntime.DispatcherConfig{})
	request := contracts.RequestContext{
		AppID: "app", EchoID: "echo", RequestID: "request", TraceID: "trace",
		Deadline: time.Now().Add(time.Minute),
	}
	if _, err := dispatcher.InvokeCapability(context.Background(), request, "lazy.capability", json.RawMessage(`{"value":"bad"}`)); err == nil {
		t.Fatal("schema-invalid payload reached lazy runtime")
	}
	if host.loads.Load() != 0 {
		t.Fatalf("runtime loaded before Dispatcher schema acceptance: %d", host.loads.Load())
	}
	result, err := dispatcher.InvokeCapability(context.Background(), request, "lazy.capability", json.RawMessage(`{"value":1}`))
	if err != nil || string(result) != `{"app_id":"app","payload_bytes":11}` || host.loads.Load() != 1 || loaded.invokes.Load() != 1 {
		t.Fatalf("result=%s loads=%d invokes=%d err=%v", result, host.loads.Load(), loaded.invokes.Load(), err)
	}
}

func TestLoaderShutdownRejectsAdmissionAndHonorsDeadline(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "shutdown.test", Version: "1.0.0", Mode: loader.ModeIsolated}}
	manager, err := loader.New(&fakeHost{mode: loader.ModeIsolated, runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "shutdown.test", Version: "1.0.0", Mode: loader.ModeIsolated, Role: loader.RoleCapability, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), "shutdown.test")
	if err != nil {
		t.Fatal(err)
	}
	timeoutContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Shutdown(timeoutContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown deadline error=%v", err)
	}
	if _, err := manager.Acquire(context.Background(), "shutdown.test"); !errors.Is(err, loader.ErrShuttingDown) {
		t.Fatalf("late acquire error=%v", err)
	}
	lease.Release()
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.stops.Load() != 1 {
		t.Fatalf("stops=%d", runtime.stops.Load())
	}
}

func TestLoaderRejectsRemoteAndMalformedLocks(t *testing.T) {
	if _, err := loader.New(&fakeHost{mode: "remote"}); !errors.Is(err, loader.ErrUnsupportedMode) {
		t.Fatalf("remote host error=%v", err)
	}
	manager, err := loader.New(&fakeHost{})
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []loader.Manifest{
		{ID: "Bad ID", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest},
		{ID: "valid", Version: "latest", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest},
		{ID: "valid", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: "not-a-digest"},
		{ID: "valid", Version: "1.0.0", Mode: "remote", LockedDigest: digest},
	} {
		if err := manager.Register(context.Background(), manifest); !errors.Is(err, loader.ErrInvalidManifest) {
			t.Fatalf("manifest=%#v err=%v", manifest, err)
		}
	}
}

func TestLoaderMarksFatalRuntimeFailureAndRecoversExplicitly(t *testing.T) {
	runtime := &fakeRuntime{
		description: loader.Description{ID: "recover.test", Version: "1.0.0", Mode: loader.ModeHosted},
		invokeErr:   loader.ErrUnavailable,
	}
	host := &fakeHost{runtime: runtime}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "recover.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler("recover.test")
	if _, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`)); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("invoke error=%v", err)
	}
	if err := manager.EnsureLoaded(context.Background(), "recover.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("failed runtime did not fail fast: %v", err)
	}
	runtime.invokeErr = nil
	if err := manager.RecoverFailed(context.Background(), "recover.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if runtime.stops.Load() != 1 || runtime.starts.Load() != 2 {
		t.Fatalf("stops=%d starts=%d", runtime.stops.Load(), runtime.starts.Load())
	}
}

// TestManagerPinnedDerivesFromManifests 验证 Pinned() 由各清单的 Pin 声明推导：
// 不同包统一，装配不再硬编码预热清单。
func TestManagerPinnedDerivesFromManifests(t *testing.T) {
	host := &fakeHost{mode: loader.ModeHosted, runtime: &fakeRuntime{description: loader.Description{ID: "pinned.test", Version: "1.0.0", Mode: loader.ModeHosted}}}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, manifest := range []loader.Manifest{
		{ID: "pinned.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest, Pin: true},
		{ID: "lazy.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: digest, IdleTTL: time.Minute},
	} {
		if err := manager.Register(ctx, manifest); err != nil {
			t.Fatal(err)
		}
	}
	pinned := manager.Pinned()
	if len(pinned) != 1 || pinned[0] != "pinned.test" {
		t.Fatalf("pinned = %v, want [pinned.test]", pinned)
	}
}
