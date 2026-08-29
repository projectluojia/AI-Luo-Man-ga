package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

var (
	ErrCapabilityDisabled     = errors.New("capability is not enabled for app")
	ErrCallDepthExceeded      = errors.New("maximum call depth exceeded")
	ErrCycleDetected          = errors.New("non-progressing call cycle detected")
	ErrIdempotencyKeyRequired = errors.New("idempotency key is required for side effects")
	ErrIdempotencyUnavailable = errors.New("durable idempotency is unavailable")
	ErrConfirmationRequired   = errors.New("governed confirmation is required")
	ErrAppPolicyUnavailable   = errors.New("app policy is unavailable")
)

type AppPolicy interface {
	Snapshot(context.Context, string) (appconfig.PolicySnapshot, error)
}

type ConfirmationRequest struct {
	AppID          string
	EchoID         string
	RunID          string
	SessionID      string
	ConfirmationID string
	TargetType     string
	TargetID       string
	SideEffect     string
	IdempotencyKey string
	// ArgumentDigest 是本次调用参数的规范化 JSON 摘要（sha256 十六进制），
	// 由 Dispatcher 边界统一计算，验证时与确认记录绑定摘要强制匹配。
	ArgumentDigest string
}

type ConfirmationVerifier interface {
	VerifyConfirmation(context.Context, ConfirmationRequest) error
}

// DispatcherConfig 装配 Dispatcher 的可选依赖；零值字段使用生产默认或保持禁用。
type DispatcherConfig struct {
	// MaxCallDepth 是 Capability/Tool 调用链的最大深度；0 使用默认值 16。
	MaxCallDepth uint16
	// ConfirmationVerifier 是持久确认验证器；为 nil 时要求确认的能力 fail-closed。
	ConfirmationVerifier ConfirmationVerifier
	// IdempotencyStore 是写/外部副作用调用的持久幂等存储；为 nil 时副作用调用 fail-closed。
	IdempotencyStore idempotency.Store
}

type Dispatcher struct {
	registry      *registry.Registry
	policy        AppPolicy
	confirmations ConfirmationVerifier
	idempotency   *idempotency.Manager
	maxCallDepth  uint16
}

func NewDispatcher(reg *registry.Registry, policy AppPolicy, config DispatcherConfig) *Dispatcher {
	if config.MaxCallDepth == 0 {
		config.MaxCallDepth = 16
	}
	d := &Dispatcher{registry: reg, policy: policy, maxCallDepth: config.MaxCallDepth}
	if config.ConfirmationVerifier != nil {
		d.confirmations = config.ConfirmationVerifier
	}
	if config.IdempotencyStore != nil {
		d.idempotency = idempotency.NewManager(config.IdempotencyStore)
	}
	return d
}

func (d *Dispatcher) InvokeCapability(ctx context.Context, request contracts.RequestContext, capabilityID string, payload json.RawMessage) (json.RawMessage, error) {
	return d.route(ctx, request, "capability", capabilityID, payload,
		[]slog.Attr{
			observe.StringAttr("target_type", "capability"),
			observe.StringAttr("capability_id", capabilityID),
		},
		func() (routedTarget, error) {
			spec, handler, err := d.registry.ResolveCapability(capabilityID)
			if err != nil {
				return routedTarget{}, err
			}
			return routedTarget{
				targetID: capabilityID, version: spec.Version, sideEffect: spec.SideEffect,
				requiresConfirmation: spec.RequiresConfirmation,
				requiredPermissions:  spec.RequiredPermissions, handler: handler,
				validateInput: func(payload json.RawMessage) error {
					return d.registry.ValidateCapabilityInput(capabilityID, payload)
				},
				fillChild: func(child contracts.RequestContext) contracts.RequestContext {
					child.CapabilityID = capabilityID
					child.ServiceID = spec.ServiceID
					child.ToolID = spec.ToolID
					return child
				},
				metric: observe.DefaultMetrics().ObserveCapability,
			}, nil
		},
	)
}

func (d *Dispatcher) UseTool(ctx context.Context, request contracts.RequestContext, serviceID, toolID string, payload json.RawMessage) (json.RawMessage, error) {
	return d.route(ctx, request, "tool", toolID, payload,
		[]slog.Attr{
			observe.StringAttr("target_type", "tool"),
			observe.StringAttr("service_id", serviceID),
			observe.StringAttr("tool_id", toolID),
		},
		func() (routedTarget, error) {
			spec, handler, err := d.registry.ResolveTool(serviceID, toolID)
			if err != nil {
				return routedTarget{}, err
			}
			return routedTarget{
				targetID: toolID, version: spec.Version, sideEffect: spec.SideEffect,
				requiresConfirmation: spec.RequiresConfirmation,
				requiredPermissions:  spec.RequiredPermissions, handler: handler,
				validateInput: func(payload json.RawMessage) error {
					return d.registry.ValidateToolInput(serviceID, toolID, payload)
				},
				fillChild: func(child contracts.RequestContext) contracts.RequestContext {
					child.CapabilityID = ""
					child.ServiceID = serviceID
					child.ToolID = toolID
					return child
				},
				metric: observe.DefaultMetrics().ObserveTool,
			}, nil
		},
	)
}

// routedTarget 是 Capability/Tool 共用的治理目标视图：解析结果、输入校验、
// 子请求标识填充与执行指标封装在一起，使两条调用路径共享同一条治理序列。
type routedTarget struct {
	targetID             string
	version              string
	sideEffect           string
	requiresConfirmation bool
	requiredPermissions  []string
	handler              registry.Handler
	validateInput        func(json.RawMessage) error
	fillChild            func(contracts.RequestContext) contracts.RequestContext
	metric               func(bool, time.Duration)
}

// route 是 Capability 与 Tool 调用的统一治理序列：校验 → 策略快照（Capability
// 另做启用检查）→ 解析 → 授权与收窄 → 严格 Schema 重验 → 子请求（深度+1、
// 调用链、循环检测）→ 幂等/确认治理执行 → 指标。
func (d *Dispatcher) route(
	ctx context.Context,
	request contracts.RequestContext,
	targetType string,
	targetID string,
	payload json.RawMessage,
	logAttrs []slog.Attr,
	resolve func() (routedTarget, error),
) (json.RawMessage, error) {
	started := time.Now()
	succeeded := false
	ctx, cancel := requestDeadlineContext(ctx, request.Deadline)
	defer cancel()
	ctx = requestLogContext(ctx, request, logAttrs...)
	observe.Debug(ctx, "开始路由调用",
		observe.IntAttr("payload_bytes", len(payload)),
		observe.IntAttr("call_depth", int(request.CallDepth)),
	)
	if err := d.validate(ctx, request); err != nil {
		observe.Warn(ctx, "调用上下文校验失败",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	policy, err := d.policySnapshot(ctx, request)
	if err != nil {
		return nil, err
	}
	if targetType == registry.TargetTypeCapability && !policy.CapabilityEnabled(targetID) {
		err := fmt.Errorf("%w: app=%q capability=%q", ErrCapabilityDisabled, request.AppID, targetID)
		observe.Warn(ctx, "App 未启用请求的 Capability")
		return nil, err
	}
	target, err := resolve()
	if err != nil {
		observe.Warn(ctx, "Registry 中未找到调用路由",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	defer func() { target.metric(succeeded, time.Since(started)) }()
	narrowedPermissions, err := d.authorize(ctx, request, targetType, target.targetID, target.sideEffect, target.requiresConfirmation, target.requiredPermissions, payload)
	if err != nil {
		observe.Warn(ctx, "权限或副作用治理拒绝本次调用",
			observe.StringAttr("error_class", "governance"),
		)
		return nil, err
	}
	if err := target.validateInput(payload); err != nil {
		observe.Warn(ctx, "输入未通过注册 Schema 校验",
			observe.StringAttr("error_class", "validation"),
		)
		return nil, err
	}
	child, fingerprint, err := d.childRequest(request, targetType, target.targetID, target.version, narrowedPermissions, payload)
	if err != nil {
		observe.Warn(ctx, "调用图治理拒绝本次调用",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	child = target.fillChild(child)
	child.TargetType = targetType
	result, replayed, err := d.invokeHandler(ctx, request, targetType, target.targetID, target.version, target.sideEffect, fingerprint, func(executionContext context.Context) (json.RawMessage, error) {
		return target.handler(executionContext, child, payload)
	})
	if err != nil {
		observe.Warn(ctx, "调用处理失败",
			observe.StringAttr("error", err.Error()),
			observe.Duration(started),
		)
		return nil, err
	}
	observe.Debug(ctx, "调用处理完成",
		observe.IntAttr("result_bytes", len(result)),
		observe.BoolAttr("idempotency_replayed", replayed),
		observe.Duration(started),
	)
	succeeded = true
	return result, nil
}

func (d *Dispatcher) policySnapshot(ctx context.Context, request contracts.RequestContext) (appconfig.PolicySnapshot, error) {
	if d.policy == nil {
		return appconfig.PolicySnapshot{}, ErrAppPolicyUnavailable
	}
	snapshot, err := d.policy.Snapshot(ctx, request.AppID)
	if err != nil {
		return appconfig.PolicySnapshot{}, errors.Join(ErrAppPolicyUnavailable, err)
	}
	if err := snapshot.Verify(request.AppID); err != nil {
		return appconfig.PolicySnapshot{}, ErrAppPolicyUnavailable
	}
	if !snapshot.Enabled {
		return appconfig.PolicySnapshot{}, ErrCapabilityDisabled
	}
	if _, err := registry.NarrowPermissions(snapshot.PermissionScope, request.PermissionScope); err != nil {
		return appconfig.PolicySnapshot{}, fmt.Errorf("%w: app=%q", registry.ErrPermissionDenied, request.AppID)
	}
	return snapshot, nil
}

func requestLogContext(ctx context.Context, request contracts.RequestContext, attrs ...slog.Attr) context.Context {
	base := []slog.Attr{
		observe.StringAttr("app_id", request.AppID),
		observe.StringAttr("echo_id", request.EchoID),
		observe.StringAttr("run_id", request.RunID),
		observe.StringAttr("request_id", request.RequestID),
		observe.StringAttr("trace_id", request.TraceID),
	}
	return observe.With(ctx, append(base, attrs...)...)
}

func requestDeadlineContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

func (d *Dispatcher) authorize(
	ctx context.Context,
	request contracts.RequestContext,
	targetType string,
	targetID string,
	sideEffect string,
	requiresConfirmation bool,
	requiredPermissions []string,
	payload json.RawMessage,
) ([]string, error) {
	narrowedPermissions, err := registry.NarrowPermissions(request.PermissionScope, requiredPermissions)
	if err != nil {
		return nil, fmt.Errorf("%w: target=%q", registry.ErrPermissionDenied, targetID)
	}
	if sideEffect == registry.SideEffectWrite || sideEffect == registry.SideEffectExternal {
		if request.IdempotencyKey == "" {
			return nil, fmt.Errorf("%w: target=%q", ErrIdempotencyKeyRequired, targetID)
		}
	}
	if requiresConfirmation {
		if request.ConfirmationID == "" || d.confirmations == nil {
			return nil, fmt.Errorf("%w: target=%q", ErrConfirmationRequired, targetID)
		}
		// 参数摘要与确认创建侧使用同一规范化算法（jsonutil.CanonicalDigest），
		// 参数改变后旧确认摘要必然不匹配，fail-closed 收敛为 confirmation_required。
		digest, err := argumentDigest(payload)
		if err != nil {
			return nil, fmt.Errorf("%w: target=%q", ErrConfirmationRequired, targetID)
		}
		confirmation := ConfirmationRequest{
			AppID:          request.AppID,
			EchoID:         request.EchoID,
			RunID:          request.RunID,
			SessionID:      request.SessionID,
			ConfirmationID: request.ConfirmationID,
			TargetType:     targetType,
			TargetID:       targetID,
			SideEffect:     sideEffect,
			IdempotencyKey: request.IdempotencyKey,
			ArgumentDigest: digest,
		}
		if err := d.confirmations.VerifyConfirmation(ctx, confirmation); err != nil {
			return nil, fmt.Errorf("%w: target=%q", ErrConfirmationRequired, targetID)
		}
	}
	return narrowedPermissions, nil
}

// argumentDigest computes the canonical JSON SHA-256 digest of an invocation payload.
// argumentDigest computes the canonical SHA-256 digest of a JSON payload and returns it as hexadecimal text.
func argumentDigest(payload json.RawMessage) (string, error) {
	sum, err := jsonutil.CanonicalDigest(payload)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

func (d *Dispatcher) invokeHandler(
	ctx context.Context,
	request contracts.RequestContext,
	targetType string,
	targetID string,
	version string,
	sideEffect string,
	fingerprint string,
	handler func(context.Context) (json.RawMessage, error),
) (json.RawMessage, bool, error) {
	if sideEffect != registry.SideEffectWrite && sideEffect != registry.SideEffectExternal {
		result, err := handler(ctx)
		return result, false, err
	}
	if d.idempotency == nil {
		return nil, false, ErrIdempotencyUnavailable
	}
	result, replayed, err := d.idempotency.Execute(ctx, idempotency.Operation{
		AppID:       request.AppID,
		Scope:       "runtime." + targetType + "/" + targetID + "/" + version,
		Key:         request.IdempotencyKey,
		Fingerprint: fingerprint,
		OwnerID:     request.RequestID,
	}, func(executionContext context.Context) ([]byte, error) {
		return handler(executionContext)
	})
	return json.RawMessage(result), replayed, err
}

func (d *Dispatcher) childRequest(
	request contracts.RequestContext,
	targetType string,
	targetID string,
	version string,
	narrowedPermissions []string,
	payload json.RawMessage,
) (contracts.RequestContext, string, error) {
	fingerprint, err := callFingerprint(request, targetType, targetID, version, payload)
	if err != nil {
		return contracts.RequestContext{}, "", err
	}
	for _, active := range request.CallChain {
		if active == fingerprint {
			return contracts.RequestContext{}, "", fmt.Errorf("%w: target=%q", ErrCycleDetected, targetID)
		}
	}
	child := request.Child()
	child.PermissionScope = narrowedPermissions
	child.CallChain = append(append([]string(nil), request.CallChain...), fingerprint)
	return child, fingerprint, nil
}

func callFingerprint(request contracts.RequestContext, targetType, targetID, version string, payload json.RawMessage) (string, error) {
	digest := sha256.New()
	permissions := append([]string(nil), request.PermissionScope...)
	sort.Strings(permissions)
	fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%s\x00%s\x00", targetType, targetID, version, request.AppID, request.UserID)
	canonicalPayload, err := jsonutil.CanonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize call payload: %w", err)
	}
	digest.Write(canonicalPayload)
	for _, permission := range permissions {
		digest.Write([]byte{0})
		digest.Write([]byte(permission))
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func (d *Dispatcher) validate(ctx context.Context, request contracts.RequestContext) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Validate(time.Now()); err != nil {
		return err
	}
	if request.CallDepth >= d.maxCallDepth {
		return fmt.Errorf("%w: depth=%d limit=%d", ErrCallDepthExceeded, request.CallDepth, d.maxCallDepth)
	}
	return nil
}
