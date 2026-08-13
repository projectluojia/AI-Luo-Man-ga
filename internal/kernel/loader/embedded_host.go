package loader

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

// Builtin 是编译进内核的内置包：Manifest 声明身份与版本，Invoke 是进程内执行入口。
// Schema、App 策略、权限、副作用与幂等治理由 Dispatcher 在 Loader 之外完成，
// 内置包本身只负责执行，与 hosted / isolated 包保持一致。
type Builtin struct {
	Manifest Manifest
	Invoke   func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)
}

// EmbeddedHost 管理编译进内核的包：Verify 校验 manifest 与内置包表精确匹配，
// Load 返回进程内 Runtime，不产生子进程或沙箱。
type EmbeddedHost struct {
	builtins map[string]Builtin
}

// NewEmbeddedHost 构造内置包宿主。同一 ID 只允许一个 manifest；执行入口不可为空。
// 当前仓库暂无生产内置包（业务扩展包按设计默认 hosted），该形态为内核自有组件
// （Storage 适配器、平台接入适配器等）以包形式纳入统一治理预留。
func NewEmbeddedHost(builtins []Builtin) (*EmbeddedHost, error) {
	table := make(map[string]Builtin, len(builtins))
	for _, builtin := range builtins {
		if err := validateManifest(builtin.Manifest); err != nil {
			return nil, err
		}
		if builtin.Manifest.Mode != ModeEmbedded {
			return nil, ErrUnsupportedMode
		}
		if builtin.Invoke == nil {
			return nil, ErrUnavailable
		}
		if _, exists := table[builtin.Manifest.ID]; exists {
			return nil, ErrDuplicateID
		}
		table[builtin.Manifest.ID] = builtin
	}
	return &EmbeddedHost{builtins: table}, nil
}

// Mode 返回宿主服务的运行模式：embedded 进程内。
func (h *EmbeddedHost) Mode() string { return ModeEmbedded }

// Verify 确认 manifest 精确匹配已编译进内核的内置包；任何字段不一致都拒绝加载。
func (h *EmbeddedHost) Verify(_ context.Context, manifest Manifest) error {
	builtin, exists := h.builtins[manifest.ID]
	if !exists {
		return ErrNotFound
	}
	if builtin.Manifest.Version != manifest.Version || builtin.Manifest.Mode != manifest.Mode ||
		builtin.Manifest.LockedDigest != manifest.LockedDigest {
		return fmt.Errorf("embedded manifest 与内置包不一致: %w", ErrDescribeMismatch)
	}
	return nil
}

// Load 返回内置包的进程内 Runtime。
func (h *EmbeddedHost) Load(_ context.Context, manifest Manifest) (Runtime, error) {
	builtin, exists := h.builtins[manifest.ID]
	if !exists {
		return nil, ErrNotFound
	}
	return &embeddedRuntime{
		id:      builtin.Manifest.ID,
		version: builtin.Manifest.Version,
		invoke:  builtin.Invoke,
	}, nil
}

// embeddedRuntime 是编译进内核的包的进程内执行单元：常驻、恒健康、无独立生命周期。
type embeddedRuntime struct {
	id      string
	version string
	invoke  func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)
}

func (r *embeddedRuntime) Describe(context.Context) (Description, error) {
	return Description{ID: r.id, Version: r.version, Mode: ModeEmbedded}, nil
}

func (r *embeddedRuntime) Start(context.Context) error { return nil }

func (r *embeddedRuntime) Health(context.Context) error { return nil }

func (r *embeddedRuntime) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	return r.invoke(ctx, request, payload)
}

func (r *embeddedRuntime) Stop(context.Context) error { return nil }
