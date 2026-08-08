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
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeHost struct {
	verifyErr error
	loadErr   error
	runtime   *fakeRuntime
	loadGate  chan struct{}
	verifies  atomic.Int32
	loads     atomic.Int32
}

type versionHost struct {
	runtimes map[string]*fakeRuntime
	loads    atomic.Int32
}

func (h *versionHost) Verify(context.Context, loader.Manifest) error {
	return nil
}

func (h *versionHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	h.loads.Add(1)
	runtime := h.runtimes[manifest.Version]
	if runtime == nil {
		return nil, errors.New("version unavailable")
	}
	return runtime, nil
}

func (h *fakeHost) Verify(context.Context, loader.Manifest) error {
	h.verifies.Add(1)
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

func TestLoaderSingleFlightsFirstUseAndDrainsBeforeUnload(t *testing.T) {
	gate := make(chan struct{})
	runtime := &fakeRuntime{description: loader.Description{ID: "extension.test", Version: "1.2.3", Mode: loader.ModeHosted}}
	host := &fakeHost{runtime: runtime, loadGate: gate}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "extension.test", Version: "1.2.3", Mode: loader.ModeHosted,
		LockedDigest: digest, IdleTTL: time.Minute,
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
	if host.verifies.Load() != 1 || host.loads.Load() != 1 || runtime.starts.Load() != 1 || runtime.health.Load() != 1 {
		t.Fatalf("verify=%d load=%d start=%d health=%d", host.verifies.Load(), host.loads.Load(), runtime.starts.Load(), runtime.health.Load())
	}
	held := make([]*loader.Lease, 0, callers)
	for lease := range leases {
		held = append(held, lease)
	}
	if len(held) != callers {
		t.Fatalf("leases=%d", len(held))
	}
	if err := manager.Unload(context.Background(), "extension.test"); !errors.Is(err, loader.ErrInFlight) {
		t.Fatalf("in-flight unload error=%v", err)
	}
	for _, lease := range held {
		lease.Release()
		lease.Release()
	}
	if err := manager.Unload(context.Background(), "extension.test"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot("extension.test")
	if err != nil || snapshot.State != loader.StateRegistered || snapshot.InFlight != 0 || runtime.stops.Load() != 1 {
		t.Fatalf("snapshot=%#v stops=%d err=%v", snapshot, runtime.stops.Load(), err)
	}
}

func TestLoaderVerifiesLockBeforeLoadAndFailsFast(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "isolated.test", Version: "1.0.0", Mode: loader.ModeIsolated}}
	host := &fakeHost{verifyErr: errors.New("digest mismatch"), runtime: runtime}
	manager, err := loader.New(map[string]loader.Host{loader.ModeIsolated: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "isolated.test", Version: "1.0.0", Mode: loader.ModeIsolated, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "isolated.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("load error=%v", err)
	}
	if err := manager.EnsureLoaded(context.Background(), "isolated.test"); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("fast failure=%v", err)
	}
	if host.verifies.Load() != 1 || host.loads.Load() != 0 {
		t.Fatalf("verify=%d load=%d", host.verifies.Load(), host.loads.Load())
	}
	snapshot, _ := manager.Snapshot("isolated.test")
	if snapshot.State != loader.StateFailed {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if err := manager.ResetFailed("isolated.test"); err != nil {
		t.Fatal(err)
	}
	host.verifyErr = nil
	if err := manager.EnsureLoaded(context.Background(), "isolated.test"); err != nil {
		t.Fatal(err)
	}
	if host.verifies.Load() != 2 || host.loads.Load() != 1 {
		t.Fatalf("retry verify=%d load=%d", host.verifies.Load(), host.loads.Load())
	}
}

func TestLoaderRegisterBatchIsAtomic(t *testing.T) {
	t.Parallel()

	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: &fakeHost{}})
	if err != nil {
		t.Fatal(err)
	}
	valid := loader.Manifest{
		ID: "batch.one", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
	}
	invalid := loader.Manifest{
		ID: "batch.two", Version: "not-semver", Mode: loader.ModeHosted, LockedDigest: digest,
	}
	if err := manager.RegisterBatch([]loader.Manifest{valid, invalid}); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("无效批次错误=%v", err)
	}
	if _, err := manager.Snapshot(valid.ID); !errors.Is(err, loader.ErrNotFound) {
		t.Fatalf("失败批次部分发布：%v", err)
	}
	if err := manager.RegisterBatch([]loader.Manifest{valid, {
		ID: "batch.two", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Snapshot("batch.one"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Snapshot("batch.two"); err != nil {
		t.Fatal(err)
	}
}

func TestLoaderRejectsDescriptionMismatchAndStopsLoadedRuntime(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "different", Version: "1.0.0", Mode: loader.ModeHosted}}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: &fakeHost{runtime: runtime}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "expected", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
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
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: &fakeHost{runtime: runtime}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "cleanup.test", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "cleanup.test"); !errors.Is(err, loader.ErrDescribeMismatch) {
		t.Fatalf("load error=%v", err)
	}
	if err := manager.ResetFailed("cleanup.test"); !errors.Is(err, loader.ErrCleanupRequired) {
		t.Fatalf("reset error=%v", err)
	}
	runtime.stopErr = nil
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.stops.Load() != 2 {
		t.Fatalf("stops=%d", runtime.stops.Load())
	}
}

func TestLoaderHandlerPreservesGovernedContextAndIdlePolicy(t *testing.T) {
	hostedRuntime := &fakeRuntime{description: loader.Description{ID: "hosted.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	pinnedRuntime := &fakeRuntime{description: loader.Description{ID: "core.test", Version: "1.0.0", Mode: loader.ModeEmbedded}}
	manager, err := loader.New(map[string]loader.Host{
		loader.ModeHosted:   &fakeHost{runtime: hostedRuntime},
		loader.ModeEmbedded: &fakeHost{runtime: pinnedRuntime},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "hosted.test", Version: "1.0.0", Mode: loader.ModeHosted,
		LockedDigest: digest, IdleTTL: time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "core.test", Version: "1.0.0", Mode: loader.ModeEmbedded,
		LockedDigest: digest, Pin: true, IdleTTL: time.Millisecond,
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
	if err := manager.EnsureLoaded(context.Background(), "core.test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := manager.SweepIdle(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	hosted, _ := manager.Snapshot("hosted.test")
	core, _ := manager.Snapshot("core.test")
	if hosted.State != loader.StateRegistered || core.State != loader.StateReady || hostedRuntime.stops.Load() != 1 {
		t.Fatalf("hosted=%#v core=%#v stops=%d", hosted, core, hostedRuntime.stops.Load())
	}
	if err := manager.Unload(context.Background(), "core.test"); !errors.Is(err, loader.ErrPinned) {
		t.Fatalf("pinned unload error=%v", err)
	}
}

func TestLoaderHandlerRemainsBehindRegistryDispatcherGovernance(t *testing.T) {
	loaded := &fakeRuntime{description: loader.Description{ID: "runtime.capability", Version: "1.0.0", Mode: loader.ModeHosted}}
	host := &fakeHost{runtime: loaded}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "runtime.capability", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	policy := kernelruntime.NewStaticAppPolicy()
	policy.Enable("app", "lazy.capability")
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "lazy", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"lazy.capability": {
				Spec: registry.CapabilitySpec{
					ID: "lazy.capability", Version: "1.0.0", Name: "懒加载测试",
					Description: "验证统一 Dispatcher 治理", ServiceID: "lazy",
					InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: manager.Handler("runtime.capability"),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := kernelruntime.NewDispatcher(reg, policy)
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

func TestLoaderUpgradeSwitchesNewTrafficBeforeDrainingOldVersion(t *testing.T) {
	oldRuntime := &fakeRuntime{description: loader.Description{ID: "upgrade.test", Version: "1.0.0", Mode: loader.ModeHosted}}
	newRuntime := &fakeRuntime{description: loader.Description{ID: "upgrade.test", Version: "2.0.0", Mode: loader.ModeHosted}}
	host := &versionHost{runtimes: map[string]*fakeRuntime{"1.0.0": oldRuntime, "2.0.0": newRuntime}}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "upgrade.test", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	oldLease, err := manager.Acquire(context.Background(), "upgrade.test")
	if err != nil {
		t.Fatal(err)
	}
	upgradeDone := make(chan error, 1)
	go func() {
		upgradeDone <- manager.Upgrade(context.Background(), loader.Manifest{
			ID: "upgrade.test", Version: "2.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
		})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot, snapshotErr := manager.Snapshot("upgrade.test")
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if snapshot.Version == "2.0.0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new version was not activated")
		}
		time.Sleep(time.Millisecond)
	}
	newLease, err := manager.Acquire(context.Background(), "upgrade.test")
	if err != nil {
		t.Fatal(err)
	}
	if oldRuntime.stops.Load() != 0 || newRuntime.starts.Load() != 1 {
		t.Fatalf("old stops=%d new starts=%d", oldRuntime.stops.Load(), newRuntime.starts.Load())
	}
	newLease.Release()
	select {
	case err := <-upgradeDone:
		t.Fatalf("upgrade drained before old invocation released: %v", err)
	default:
	}
	oldLease.Release()
	if err := <-upgradeDone; err != nil {
		t.Fatal(err)
	}
	if oldRuntime.stops.Load() != 1 || newRuntime.stops.Load() != 0 || host.loads.Load() != 2 {
		t.Fatalf("old stops=%d new stops=%d loads=%d", oldRuntime.stops.Load(), newRuntime.stops.Load(), host.loads.Load())
	}
}

func TestLoaderUpgradeDeadlineRetainsOldHandleForShutdownCleanup(t *testing.T) {
	oldRuntime := &fakeRuntime{description: loader.Description{ID: "upgrade.timeout", Version: "1.0.0", Mode: loader.ModeIsolated}}
	newRuntime := &fakeRuntime{description: loader.Description{ID: "upgrade.timeout", Version: "2.0.0", Mode: loader.ModeIsolated}}
	host := &versionHost{runtimes: map[string]*fakeRuntime{"1.0.0": oldRuntime, "2.0.0": newRuntime}}
	manager, err := loader.New(map[string]loader.Host{loader.ModeIsolated: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "upgrade.timeout", Version: "1.0.0", Mode: loader.ModeIsolated, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), "upgrade.timeout")
	if err != nil {
		t.Fatal(err)
	}
	upgradeContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = manager.Upgrade(upgradeContext, loader.Manifest{
		ID: "upgrade.timeout", Version: "2.0.0", Mode: loader.ModeIsolated, LockedDigest: digest,
	})
	if !errors.Is(err, loader.ErrInFlight) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("upgrade timeout error=%v", err)
	}
	snapshot, _ := manager.Snapshot("upgrade.timeout")
	if snapshot.Version != "2.0.0" || oldRuntime.stops.Load() != 0 {
		t.Fatalf("snapshot=%#v old stops=%d", snapshot, oldRuntime.stops.Load())
	}
	lease.Release()
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oldRuntime.stops.Load() != 1 || newRuntime.stops.Load() != 1 {
		t.Fatalf("old stops=%d new stops=%d", oldRuntime.stops.Load(), newRuntime.stops.Load())
	}
}

func TestLoaderShutdownRejectsAdmissionAndHonorsDeadline(t *testing.T) {
	runtime := &fakeRuntime{description: loader.Description{ID: "shutdown.test", Version: "1.0.0", Mode: loader.ModeIsolated}}
	manager, err := loader.New(map[string]loader.Host{loader.ModeIsolated: &fakeHost{runtime: runtime}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "shutdown.test", Version: "1.0.0", Mode: loader.ModeIsolated, LockedDigest: digest,
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
	if _, err := loader.New(map[string]loader.Host{"remote": &fakeHost{}}); !errors.Is(err, loader.ErrUnsupportedMode) {
		t.Fatalf("remote host error=%v", err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: &fakeHost{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range []loader.Manifest{
		{ID: "Bad ID", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest},
		{ID: "valid", Version: "latest", Mode: loader.ModeHosted, LockedDigest: digest},
		{ID: "valid", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: "not-a-digest"},
		{ID: "valid", Version: "1.0.0", Mode: "remote", LockedDigest: digest},
	} {
		if err := manager.Register(manifest); !errors.Is(err, loader.ErrInvalidManifest) {
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
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(loader.Manifest{
		ID: "recover.test", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler("recover.test")
	if _, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`)); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("invoke error=%v", err)
	}
	snapshot, err := manager.Snapshot("recover.test")
	if err != nil || snapshot.State != loader.StateFailed {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
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
