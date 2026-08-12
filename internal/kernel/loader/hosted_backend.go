package loader

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

// HostedBackendConfig 装配外部 Runtime Host 的 hosted 后端。
// 外部宿主在独立进程内以 wazero 沙箱执行 hosted 工件（内存上限 + 执行时间预算）。
// 需要宿主函数投影的工件（如内置 campus）只能在内核进程内执行：宿主函数是内核特权，
// 跨进程无法投影权威存储，这是架构契约而非降级路径。
type HostedBackendConfig struct {
	// ReadArtifact 按 manifest 读取工件字节（digest 与大小校验由实现负责）。
	ReadArtifact func(context.Context, Manifest) ([]byte, error)
	// MemoryLimitPages 是 guest 线性内存上限（页）；0 使用默认值。
	MemoryLimitPages uint32
	// MaxArtifactBytes 是工件字节上限；0 使用默认值。
	MaxArtifactBytes int64
	// CallTimeout 是单次调用的执行时间预算；0 使用默认值。预算耗尽强制终止 guest。
	CallTimeout time.Duration
}

// hostedRuntimeBackend 实现 RuntimeHostBackend：按 BackendIdentity 装载 hosted 工件，
// 编译产物复用，每次调用独立实例化并受执行时间预算约束。
type hostedRuntimeBackend struct {
	config   HostedBackendConfig
	mu       sync.Mutex
	runtimes map[BackendIdentity]*wasmRuntime
}

// NewHostedRuntimeBackend 构造 hosted 后端；配置非法时返回显式错误。
func NewHostedRuntimeBackend(config HostedBackendConfig) (*hostedRuntimeBackend, error) {
	if config.ReadArtifact == nil {
		return nil, ErrUnavailable
	}
	return &hostedRuntimeBackend{
		config: config, runtimes: make(map[BackendIdentity]*wasmRuntime),
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
	host, err := NewWasmHost(WasmHostConfig{
		ReadArtifact:     b.config.ReadArtifact,
		MemoryLimitPages: b.config.MemoryLimitPages,
		MaxArtifactBytes: b.config.MaxArtifactBytes,
		CallTimeout:      b.config.CallTimeout,
	})
	if err != nil {
		return nil, err
	}
	loaded, err := host.Load(ctx, Manifest{ID: identity.ID, Version: identity.Version, Mode: ModeHosted})
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
