package loader_test

import (
	"context"
	"errors"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

	"google.golang.org/grpc"
)

// lifecycleOnlyRuntime 只实现生命周期面，不具备任何角色执行面（既不是
// Invoker 也不是 executor.ClientProvider），用于角色校验负向测试。
type lifecycleOnlyRuntime struct {
	description loader.Description
}

func (r *lifecycleOnlyRuntime) Describe(context.Context) (loader.Description, error) {
	return r.description, nil
}

func (r *lifecycleOnlyRuntime) Start(context.Context) error  { return nil }
func (r *lifecycleOnlyRuntime) Health(context.Context) error { return nil }
func (r *lifecycleOnlyRuntime) Stop(context.Context) error   { return nil }

// fakeExecutorClient 是 executor.Client 的最小测试实现（本测试不调用其方法）。
type fakeExecutorClient struct{}

func (fakeExecutorClient) Run(context.Context, ...grpc.CallOption) (executor.RunStream, error) {
	return nil, errors.New("run not used in role test")
}

func (fakeExecutorClient) Health(context.Context, *executor.HealthRequest, ...grpc.CallOption) (*executor.HealthResponse, error) {
	return nil, errors.New("health not used in role test")
}

// fakeExecutorRuntime 实现执行者角色契约：生命周期面 + executor.ClientProvider。
type fakeExecutorRuntime struct {
	description loader.Description
}

func (r *fakeExecutorRuntime) Describe(context.Context) (loader.Description, error) {
	return r.description, nil
}

func (r *fakeExecutorRuntime) Start(context.Context) error  { return nil }
func (r *fakeExecutorRuntime) Health(context.Context) error { return nil }
func (r *fakeExecutorRuntime) Stop(context.Context) error   { return nil }
func (r *fakeExecutorRuntime) Client() executor.Client      { return fakeExecutorClient{} }

// idKeyedHost 按清单 ID 返回对应运行时，用于单宿主承载多个运行时（角色解析测试）。
type idKeyedHost struct {
	mode     string
	runtimes map[string]loader.Runtime
}

func (h *idKeyedHost) Mode() string { return h.mode }

func (h *idKeyedHost) Verify(context.Context, loader.Manifest) error { return nil }

func (h *idKeyedHost) Load(_ context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	runtime := h.runtimes[manifest.ID]
	if runtime == nil {
		return nil, errors.New("runtime unavailable")
	}
	return runtime, nil
}

func TestLoaderRejectsManifestWithoutRole(t *testing.T) {
	manager, err := loader.New(&fakeHost{runtime: &fakeRuntime{}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := loader.Manifest{ID: "norole.test", Version: "1.0.0", Mode: loader.ModeHosted, LockedDigest: digest}
	if err := manager.Register(context.Background(), manifest); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("register without role error=%v, want ErrInvalidManifest", err)
	}
}

func TestLoaderRejectsCapabilityRoleWithoutInvoker(t *testing.T) {
	description := loader.Description{ID: "cap.test", Version: "1.0.0", Mode: loader.ModeHosted}
	manager, err := loader.New(&fakeHost{runtime: &lifecycleOnlyRuntime{description: description}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: description.ID, Version: description.Version, Mode: description.Mode,
		Role: loader.RoleProvider, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), description.ID); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("load error=%v, want ErrLoadFailed", err)
	}
}

func TestLoaderRejectsExecutorRoleWithoutContract(t *testing.T) {
	description := loader.Description{ID: "exec.test", Version: "1.0.0", Mode: loader.ModeIsolated}
	// fakeRuntime 只实现能力执行面（Invoker），不实现 executor.ClientProvider。
	manager, err := loader.New(&fakeHost{mode: loader.ModeIsolated, runtime: &fakeRuntime{description: description}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: description.ID, Version: description.Version, Mode: description.Mode,
		Role: loader.RoleExecutor, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), description.ID); !errors.Is(err, loader.ErrLoadFailed) {
		t.Fatalf("load error=%v, want ErrLoadFailed", err)
	}
}

func TestManagerExecutorResolvesUniqueExecutor(t *testing.T) {
	description := loader.Description{ID: "exec.unique", Version: "1.0.0", Mode: loader.ModeIsolated}
	manager, err := loader.New(&fakeHost{mode: loader.ModeIsolated, runtime: &fakeExecutorRuntime{description: description}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: description.ID, Version: description.Version, Mode: description.Mode,
		Role: loader.RoleExecutor, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Warmup(context.Background(), []string{description.ID}, 1); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	lease, err := manager.Executor(context.Background())
	if err != nil {
		t.Fatalf("Executor: %v", err)
	}
	defer lease.Release()
	if lease.ID() != description.ID {
		t.Fatalf("executor id=%q, want %q", lease.ID(), description.ID)
	}
	if _, ok := lease.Runtime().(executor.ClientProvider); !ok {
		t.Fatal("executor runtime does not expose the executor contract")
	}
}

func TestManagerExecutorFailsClosedWithoutExecutor(t *testing.T) {
	manager, err := loader.New(&fakeHost{runtime: &fakeRuntime{description: loader.Description{ID: "cap.test", Version: "1.0.0", Mode: loader.ModeHosted}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), loader.Manifest{
		ID: "cap.test", Version: "1.0.0", Mode: loader.ModeHosted,
		Role: loader.RoleProvider, LockedDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Executor(context.Background()); !errors.Is(err, loader.ErrNotFound) {
		t.Fatalf("Executor error=%v, want ErrNotFound", err)
	}
}

func TestManagerExecutorFailsClosedWithMultipleExecutors(t *testing.T) {
	host := &idKeyedHost{mode: loader.ModeIsolated, runtimes: map[string]loader.Runtime{
		"exec.one": &fakeExecutorRuntime{description: loader.Description{ID: "exec.one", Version: "1.0.0", Mode: loader.ModeIsolated}},
		"exec.two": &fakeExecutorRuntime{description: loader.Description{ID: "exec.two", Version: "1.0.0", Mode: loader.ModeIsolated}},
	}}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"exec.one", "exec.two"} {
		if err := manager.Register(context.Background(), loader.Manifest{
			ID: id, Version: "1.0.0", Mode: loader.ModeIsolated,
			Role: loader.RoleExecutor, LockedDigest: digest,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Executor(context.Background()); !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("Executor error=%v, want ErrInvalidManifest", err)
	}
}

func TestLoaderRoleConsistencyEnforcedAtLoad(t *testing.T) {
	description := loader.Description{ID: "role.test", Version: "1.0.0", Mode: loader.ModeHosted}
	manager, err := loader.New(&fakeHost{runtime: &fakeRuntime{description: description}})
	if err != nil {
		t.Fatal(err)
	}
	capability := loader.Manifest{
		ID: description.ID, Version: "1.0.0", Mode: loader.ModeHosted,
		Role: loader.RoleProvider, LockedDigest: digest,
	}
	if err := manager.Register(context.Background(), capability); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), description.ID); err != nil {
		t.Fatalf("能力提供者角色必须可加载：%v", err)
	}
}

var _ loader.Invoker = (*fakeRuntime)(nil)
