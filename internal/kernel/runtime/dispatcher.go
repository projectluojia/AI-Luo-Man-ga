package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
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
	ConfirmationID string
	CapabilityID   string
	SideEffect     string
	IdempotencyKey string
}

type ConfirmationVerifier interface {
	VerifyConfirmation(context.Context, ConfirmationRequest) error
}

// DispatcherConfig 装配 Dispatcher 的可选依赖；零值字段使用生产默认或保持禁用。
type DispatcherConfig struct {
	// MaxCallDepth 是 Capability 调用链的最大深度；0 使用默认值 16。
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

// InvokeCapability 经过同一条校验、授权、幂等、确认和审计前置链路调用 Capability。
func (d *Dispatcher) InvokeCapability(ctx context.Context, request contracts.RequestContext, capabilityID string, payload json.RawMessage) (json.RawMessage, error) {
	return d.route(ctx, request, capabilityID, payload)
}

// route 是 Capability 的统一治理序列：上下文 → App 策略 → Registry → 权限与副作用
// → Schema → 调用链 → 幂等/确认 → 实现。
func (d *Dispatcher) route(
	ctx context.Context,
	request contracts.RequestContext,
	capabilityID string,
	payload json.RawMessage,
) (json.RawMessage, error) {
	started := time.Now()
	succeeded := false
	defer func() { observe.DefaultMetrics().ObserveCapability(succeeded, time.Since(started)) }()
	ctx, cancel := requestDeadlineContext(ctx, request.Deadline)
	defer cancel()
	ctx = requestLogContext(ctx, request, observe.StringAttr("capability_id", capabilityID))
	observe.Debug(ctx, "开始路由 Capability",
		observe.IntAttr("payload_bytes", len(payload)),
		observe.IntAttr("call_depth", int(request.CallDepth)),
	)
	if err := d.validate(ctx, request); err != nil {
		observe.Warn(ctx, "调用上下文校验失败", observe.StringAttr("error", err.Error()))
		return nil, err
	}
	policy, err := d.policySnapshot(ctx, request)
	if err != nil {
		return nil, err
	}
	if !policy.CapabilityEnabled(capabilityID) {
		err := fmt.Errorf("%w: app=%q capability=%q", ErrCapabilityDisabled, request.AppID, capabilityID)
		observe.Warn(ctx, "App 未启用请求的 Capability")
		return nil, err
	}
	spec, handler, err := d.registry.ResolveCapability(capabilityID)
	if err != nil {
		observe.Warn(ctx, "Registry 中未找到 Capability 路由", observe.StringAttr("error", err.Error()))
		return nil, err
	}
	narrowedPermissions, err := d.authorize(ctx, request, spec.ID, spec.SideEffect,
		spec.RequiresConfirmation, spec.RequiredPermissions)
	if err != nil {
		observe.Warn(ctx, "权限或副作用治理拒绝本次调用",
			observe.StringAttr("error_class", "governance"),
		)
		return nil, err
	}
	if err := d.registry.ValidateCapabilityInput(spec.ID, payload); err != nil {
		observe.Warn(ctx, "输入未通过 Capability Schema 校验",
			observe.StringAttr("error_class", "validation"),
		)
		return nil, err
	}
	child, fingerprint, err := d.childRequest(request, spec.ID, spec.Version, narrowedPermissions, payload)
	if err != nil {
		observe.Warn(ctx, "调用图治理拒绝本次调用", observe.StringAttr("error", err.Error()))
		return nil, err
	}
	child.CapabilityID = spec.ID
	result, replayed, err := d.invokeHandler(ctx, request, spec.ID, spec.Version,
		spec.SideEffect, fingerprint, func(executionContext context.Context) (json.RawMessage, error) {
			return handler(executionContext, child, payload)
		})
	if err != nil {
		observe.Warn(ctx, "Capability 处理失败",
			observe.StringAttr("error", err.Error()), observe.Duration(started))
		return nil, err
	}
	observe.Debug(ctx, "Capability 处理完成",
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
	targetID string,
	sideEffect string,
	requiresConfirmation bool,
	requiredPermissions []string,
) ([]string, error) {
	narrowedPermissions, err := registry.NarrowPermissions(request.PermissionScope, requiredPermissions)
	if err != nil {
		return nil, fmt.Errorf("%w: target=%q", registry.ErrPermissionDenied, targetID)
	}
	if sideEffect == capability.SideEffectWrite || sideEffect == capability.SideEffectExternal {
		if request.IdempotencyKey == "" {
			return nil, fmt.Errorf("%w: target=%q", ErrIdempotencyKeyRequired, targetID)
		}
	}
	if requiresConfirmation {
		if request.ConfirmationID == "" || d.confirmations == nil {
			return nil, fmt.Errorf("%w: target=%q", ErrConfirmationRequired, targetID)
		}
		confirmation := ConfirmationRequest{
			AppID: request.AppID, EchoID: request.EchoID, RunID: request.RunID,
			ConfirmationID: request.ConfirmationID, CapabilityID: targetID,
			SideEffect: sideEffect, IdempotencyKey: request.IdempotencyKey,
		}
		if err := d.confirmations.VerifyConfirmation(ctx, confirmation); err != nil {
			return nil, fmt.Errorf("%w: target=%q", ErrConfirmationRequired, targetID)
		}
	}
	return narrowedPermissions, nil
}

func (d *Dispatcher) invokeHandler(
	ctx context.Context,
	request contracts.RequestContext,
	targetID string,
	version string,
	sideEffect string,
	fingerprint string,
	handler func(context.Context) (json.RawMessage, error),
) (json.RawMessage, bool, error) {
	if sideEffect != capability.SideEffectWrite && sideEffect != capability.SideEffectExternal {
		result, err := handler(ctx)
		return result, false, err
	}
	if d.idempotency == nil {
		return nil, false, ErrIdempotencyUnavailable
	}
	result, replayed, err := d.idempotency.Execute(ctx, idempotency.Operation{
		AppID: request.AppID, Scope: "runtime.capability/" + targetID + "/" + version,
		Key: request.IdempotencyKey, Fingerprint: fingerprint, OwnerID: request.RequestID,
	}, func(executionContext context.Context) ([]byte, error) {
		return handler(executionContext)
	})
	return json.RawMessage(result), replayed, err
}

func (d *Dispatcher) childRequest(
	request contracts.RequestContext,
	targetID string,
	version string,
	narrowedPermissions []string,
	payload json.RawMessage,
) (contracts.RequestContext, string, error) {
	fingerprint, err := callFingerprint(request, targetID, version, payload)
	if err != nil {
		return contracts.RequestContext{}, "", err
	}
	for _, active := range request.CallChain {
		if active == fingerprint {
			return contracts.RequestContext{}, "", fmt.Errorf("%w: target=%q", ErrCycleDetected, targetID)
		}
	}
	child := request.NextCall()
	child.PermissionScope = narrowedPermissions
	child.CallChain = append(append([]string(nil), request.CallChain...), fingerprint)
	return child, fingerprint, nil
}

func callFingerprint(request contracts.RequestContext, targetID, version string, payload json.RawMessage) (string, error) {
	digest := sha256.New()
	permissions := append([]string(nil), request.PermissionScope...)
	sort.Strings(permissions)
	fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%s\x00", targetID, version, request.AppID, request.UserID)
	canonicalPayload, err := jsonutil.CanonicalJSON(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize call payload: %w", err)
	}
	_, _ = digest.Write(canonicalPayload)
	for _, permission := range permissions {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(permission))
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
