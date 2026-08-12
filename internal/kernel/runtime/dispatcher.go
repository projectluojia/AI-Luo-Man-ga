package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

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

type ToolCaller interface {
	UseTool(context.Context, contracts.RequestContext, string, string, json.RawMessage) (json.RawMessage, error)
}

type ConfirmationRequest struct {
	AppID          string
	EchoID         string
	RunID          string
	ConfirmationID string
	TargetType     string
	TargetID       string
	SideEffect     string
	IdempotencyKey string
}

type ConfirmationVerifier interface {
	VerifyConfirmation(context.Context, ConfirmationRequest) error
}

type DispatcherOption func(*Dispatcher)

func WithMaxCallDepth(depth uint16) DispatcherOption {
	return func(d *Dispatcher) {
		if depth > 0 {
			d.maxCallDepth = depth
		}
	}
}

func WithConfirmationVerifier(verifier ConfirmationVerifier) DispatcherOption {
	return func(d *Dispatcher) {
		d.confirmations = verifier
	}
}

func WithIdempotencyStore(store idempotency.Store) DispatcherOption {
	return func(d *Dispatcher) {
		if store != nil {
			d.idempotency = idempotency.NewManager(store)
		}
	}
}

type Dispatcher struct {
	registry      *registry.Registry
	policy        AppPolicy
	confirmations ConfirmationVerifier
	idempotency   *idempotency.Manager
	maxCallDepth  uint16
	now           func() time.Time
}

func NewDispatcher(reg *registry.Registry, policy AppPolicy, options ...DispatcherOption) *Dispatcher {
	d := &Dispatcher{registry: reg, policy: policy, maxCallDepth: 16, now: time.Now}
	for _, option := range options {
		option(d)
	}
	return d
}

func (d *Dispatcher) InvokeCapability(ctx context.Context, request contracts.RequestContext, capabilityID string, payload json.RawMessage) (json.RawMessage, error) {
	started := time.Now()
	succeeded := false
	defer func() {
		observe.DefaultMetrics().ObserveCapability(succeeded, time.Since(started))
	}()
	ctx, cancel := requestDeadlineContext(ctx, request.Deadline)
	defer cancel()
	ctx = requestLogContext(ctx, request,
		observe.StringAttr("target_type", "capability"),
		observe.StringAttr("capability_id", capabilityID),
	)
	observe.Debug(ctx, "开始路由 Capability 调用",
		observe.IntAttr("payload_bytes", len(payload)),
		observe.IntAttr("call_depth", int(request.CallDepth)),
	)
	if err := d.validate(ctx, request); err != nil {
		observe.Warn(ctx, "Capability 调用上下文校验失败",
			observe.StringAttr("error", err.Error()),
		)
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
		observe.Warn(ctx, "Registry 中未找到 Capability 路由",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	narrowedPermissions, err := d.authorize(ctx, request, "capability", capabilityID, spec.SideEffect, spec.RequiresConfirmation, spec.RequiredPermissions)
	if err != nil {
		observe.Warn(ctx, "Capability 权限或副作用治理拒绝本次调用",
			observe.StringAttr("error_class", "governance"),
		)
		return nil, err
	}
	if err := d.registry.ValidateCapabilityInput(capabilityID, payload); err != nil {
		observe.Warn(ctx, "Capability 输入未通过注册 Schema 校验",
			observe.StringAttr("error_class", "validation"),
		)
		return nil, err
	}
	child, fingerprint, err := d.childRequest(request, "capability", capabilityID, spec.Version, narrowedPermissions, payload)
	if err != nil {
		observe.Warn(ctx, "Capability 调用图治理拒绝本次调用",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	child.TargetType = "capability"
	child.CapabilityID = capabilityID
	child.ServiceID = spec.ServiceID
	child.ToolID = spec.ToolID
	result, replayed, err := d.invokeHandler(ctx, request, "capability", capabilityID, spec.Version, spec.SideEffect, fingerprint, func(executionContext context.Context) (json.RawMessage, error) {
		return handler(executionContext, child, payload)
	})
	if err != nil {
		observe.Warn(ctx, "Capability 处理失败",
			observe.StringAttr("error", err.Error()),
			observe.Duration(started),
		)
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

func (d *Dispatcher) UseTool(ctx context.Context, request contracts.RequestContext, serviceID, toolID string, payload json.RawMessage) (json.RawMessage, error) {
	started := time.Now()
	succeeded := false
	defer func() {
		observe.DefaultMetrics().ObserveTool(succeeded, time.Since(started))
	}()
	ctx, cancel := requestDeadlineContext(ctx, request.Deadline)
	defer cancel()
	ctx = requestLogContext(ctx, request,
		observe.StringAttr("target_type", "tool"),
		observe.StringAttr("service_id", serviceID),
		observe.StringAttr("tool_id", toolID),
	)
	observe.Debug(ctx, "开始路由 Tool 调用",
		observe.IntAttr("payload_bytes", len(payload)),
		observe.IntAttr("call_depth", int(request.CallDepth)),
	)
	if err := d.validate(ctx, request); err != nil {
		observe.Warn(ctx, "Tool 调用上下文校验失败",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	if _, err := d.policySnapshot(ctx, request); err != nil {
		return nil, err
	}
	spec, handler, err := d.registry.ResolveTool(serviceID, toolID)
	if err != nil {
		observe.Warn(ctx, "Registry 中未找到 Tool 路由",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	narrowedPermissions, err := d.authorize(ctx, request, "tool", toolID, spec.SideEffect, spec.RequiresConfirmation, spec.RequiredPermissions)
	if err != nil {
		observe.Warn(ctx, "Tool 权限或副作用治理拒绝本次调用",
			observe.StringAttr("error_class", "governance"),
		)
		return nil, err
	}
	if err := d.registry.ValidateToolInput(serviceID, toolID, payload); err != nil {
		observe.Warn(ctx, "Tool 输入未通过注册 Schema 校验",
			observe.StringAttr("error_class", "validation"),
		)
		return nil, err
	}
	child, fingerprint, err := d.childRequest(request, "tool", toolID, spec.Version, narrowedPermissions, payload)
	if err != nil {
		observe.Warn(ctx, "Tool 调用图治理拒绝本次调用",
			observe.StringAttr("error", err.Error()),
		)
		return nil, err
	}
	child.TargetType = "tool"
	child.CapabilityID = ""
	child.ServiceID = serviceID
	child.ToolID = toolID
	result, replayed, err := d.invokeHandler(ctx, request, "tool", toolID, spec.Version, spec.SideEffect, fingerprint, func(executionContext context.Context) (json.RawMessage, error) {
		return handler(executionContext, child, payload)
	})
	if err != nil {
		observe.Warn(ctx, "Tool 处理失败",
			observe.StringAttr("error", err.Error()),
			observe.Duration(started),
		)
		return nil, err
	}
	observe.Debug(ctx, "Tool 处理完成",
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
		confirmation := ConfirmationRequest{
			AppID:          request.AppID,
			EchoID:         request.EchoID,
			RunID:          request.RunID,
			ConfirmationID: request.ConfirmationID,
			TargetType:     targetType,
			TargetID:       targetID,
			SideEffect:     sideEffect,
			IdempotencyKey: request.IdempotencyKey,
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
	var decoded any
	if len(payload) == 0 {
		decoded = map[string]any{}
	} else if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("canonicalize call payload: %w", err)
	}
	canonicalPayload, err := json.Marshal(decoded)
	if err != nil {
		return "", fmt.Errorf("canonicalize call payload: %w", err)
	}
	permissions := append([]string(nil), request.PermissionScope...)
	sort.Strings(permissions)
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%s\x00%s\x00", targetType, targetID, version, request.AppID, request.UserID)
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
	if err := request.Validate(d.now()); err != nil {
		return err
	}
	if request.CallDepth >= d.maxCallDepth {
		return fmt.Errorf("%w: depth=%d limit=%d", ErrCallDepthExceeded, request.CallDepth, d.maxCallDepth)
	}
	return nil
}

type StaticAppPolicy struct {
	mu          sync.RWMutex
	enabled     map[string]map[string]struct{}
	permissions map[string]map[string]struct{}
}

func NewStaticAppPolicy() *StaticAppPolicy {
	return &StaticAppPolicy{
		enabled: make(map[string]map[string]struct{}), permissions: make(map[string]map[string]struct{}),
	}
}

func (p *StaticAppPolicy) Enable(appID, capabilityID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enabled[appID] == nil {
		p.enabled[appID] = make(map[string]struct{})
	}
	p.enabled[appID][capabilityID] = struct{}{}
}

func (p *StaticAppPolicy) Grant(appID, permission string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.permissions[appID] == nil {
		p.permissions[appID] = make(map[string]struct{})
	}
	p.permissions[appID][permission] = struct{}{}
}

func (p *StaticAppPolicy) Snapshot(_ context.Context, appID string) (appconfig.PolicySnapshot, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	capabilities := make([]string, 0, len(p.enabled[appID]))
	for capability := range p.enabled[appID] {
		capabilities = append(capabilities, capability)
	}
	permissions := make([]string, 0, len(p.permissions[appID]))
	for permission := range p.permissions[appID] {
		permissions = append(permissions, permission)
	}
	sort.Strings(capabilities)
	sort.Strings(permissions)
	return appconfig.PolicySnapshot{
		AppID: appID, Revision: "static", Generation: 1,
		Enabled:             true,
		EnabledCapabilities: capabilities, PermissionScope: permissions,
	}, nil
}
