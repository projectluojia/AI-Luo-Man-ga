package runtimehost

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader/wasmhost"
)

// processBackendRuntime 是 hosted 后端承载的运行时面：hosted 清单只有 provider
// 角色，装载结果同时实现生命周期与治理调用。
type processBackendRuntime interface {
	loader.Runtime
	loader.Invoker
}

// hostedRuntimeBackend 实现 RuntimeHostBackend：按 BackendIdentity 装载 hosted 工件，
// 编译产物复用，每次调用独立实例化并受执行时间预算约束。
// 外部宿主在独立进程内以 wazero 沙箱执行 hosted 工件；需要宿主函数投影的工件
// （如声明 ailuo.store 的安装包）只能在内核进程内执行：宿主函数是内核特权，跨进程无法投影
// 权威存储，这是架构契约而非降级路径。
type hostedRuntimeBackend struct {
	host     *wasmhost.WasmHost
	mu       sync.Mutex
	runtimes map[BackendIdentity]processBackendRuntime
}

// NewHostedRuntimeBackend 构造 hosted 后端；配置非法时返回显式错误。
// 复用 WasmHostConfig 作为唯一沙箱配置（读工件/内存上限/工件上限/执行时间预算），
// 不再定义重复的配置结构。
func NewHostedRuntimeBackend(config wasmhost.WasmHostConfig) (*hostedRuntimeBackend, error) {
	host, err := wasmhost.NewWasmHost(config)
	if err != nil {
		return nil, err
	}
	return &hostedRuntimeBackend{
		host: host, runtimes: make(map[BackendIdentity]processBackendRuntime),
	}, nil
}

func (b *hostedRuntimeBackend) Describe(ctx context.Context, identity BackendIdentity) (loader.Description, error) {
	runtime, err := b.load(ctx, identity)
	if err != nil {
		return loader.Description{}, err
	}
	return runtime.Describe(ctx)
}

func (b *hostedRuntimeBackend) Start(ctx context.Context, identity BackendIdentity) error {
	_, err := b.load(ctx, identity)
	return err
}

func (b *hostedRuntimeBackend) Health(ctx context.Context, identity BackendIdentity) error {
	b.mu.Lock()
	runtime := b.runtimes[identity]
	b.mu.Unlock()
	if runtime == nil {
		return loader.ErrNotFound
	}
	return runtime.Health(ctx)
}

func (b *hostedRuntimeBackend) Invoke(ctx context.Context, identity BackendIdentity, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	runtime, err := b.load(ctx, identity)
	if err != nil {
		return nil, err
	}
	return runtime.Invoke(ctx, request, payload)
}

func (b *hostedRuntimeBackend) Stop(ctx context.Context, identity BackendIdentity) error {
	b.mu.Lock()
	runtime := b.runtimes[identity]
	delete(b.runtimes, identity)
	b.mu.Unlock()
	if runtime == nil {
		return nil
	}
	return runtime.Stop(ctx)
}

// load 读取并编译工件（首次调用），之后复用编译产物。装载失败不缓存，下次调用重试；
// 协议服务端会把 Start 失败标记为 Failed 并拒绝后续调用（fail-closed）。
func (b *hostedRuntimeBackend) load(ctx context.Context, identity BackendIdentity) (processBackendRuntime, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if runtime := b.runtimes[identity]; runtime != nil {
		return runtime, nil
	}
	loaded, err := b.host.Load(ctx, loader.Manifest{ID: identity.ID, Version: identity.Version, Mode: loader.ModeHosted})
	if err != nil {
		return nil, err
	}
	runtime, ok := loaded.(processBackendRuntime)
	if !ok {
		_ = loaded.Stop(ctx)
		return nil, loader.ErrUnavailable
	}
	b.runtimes[identity] = runtime
	return runtime, nil
}
