package loader

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

// hostedRuntimeBackend 实现 RuntimeHostBackend：按 BackendIdentity 装载 hosted 工件，
// 编译产物复用，每次调用独立实例化并受执行时间预算约束。
// 外部宿主在独立进程内以 wazero 沙箱执行 hosted 工件；需要宿主函数投影的工件
// （如内置 campus）只能在内核进程内执行：宿主函数是内核特权，跨进程无法投影
// 权威存储，这是架构契约而非降级路径。
type hostedRuntimeBackend struct {
	host     *WasmHost
	mu       sync.Mutex
	runtimes map[BackendIdentity]*wasmRuntime
}

// NewHostedRuntimeBackend 构造 hosted 后端；配置非法时返回显式错误。
// 复用 WasmHostConfig 作为唯一沙箱配置（读工件/内存上限/工件上限/执行时间预算），
// 不再定义重复的配置结构。
func NewHostedRuntimeBackend(config WasmHostConfig) (*hostedRuntimeBackend, error) {
	host, err := NewWasmHost(config)
	if err != nil {
		return nil, err
	}
	return &hostedRuntimeBackend{
		host: host, runtimes: make(map[BackendIdentity]*wasmRuntime),
	}, nil
}

func (b *hostedRuntimeBackend) Describe(ctx context.Context, identity BackendIdentity) (Description, error) {
	runtime, err := b.load(ctx, identity)
	if err != nil {
		return Description{}, err
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
		return ErrNotFound
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
func (b *hostedRuntimeBackend) load(ctx context.Context, identity BackendIdentity) (*wasmRuntime, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if runtime := b.runtimes[identity]; runtime != nil {
		return runtime, nil
	}
	loaded, err := b.host.Load(ctx, Manifest{ID: identity.ID, Version: identity.Version, Mode: ModeHosted})
	if err != nil {
		return nil, err
	}
	runtime, ok := loaded.(*wasmRuntime)
	if !ok {
		_ = loaded.Stop(ctx)
		return nil, ErrUnavailable
	}
	b.runtimes[identity] = runtime
	return runtime, nil
}
