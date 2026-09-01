package loader

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxRuntimeDeadline = 24 * time.Hour

type BackendIdentity struct {
	ID      string
	Version string
}

// RuntimeHostBackend 实现扩展宿主内的实际装载；它不得绕过 Go 内核访问外部系统或权威存储。
type RuntimeHostBackend interface {
	Describe(context.Context, BackendIdentity) (Description, error)
	Start(context.Context, BackendIdentity) error
	Health(context.Context, BackendIdentity) error
	Invoke(context.Context, BackendIdentity, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)
	Stop(context.Context, BackendIdentity) error
}

type RuntimeHostServerConfig struct {
	Mode            string
	Backend         RuntimeHostBackend
	AllowedRuntimes []BackendIdentity
	MaxRuntimes     int
	MaxConcurrent   int
	Now             func() time.Time
}

type runtimeHostServerEntry struct {
	state    string
	inFlight int
}

// RuntimeHostProtocolServer 在宿主侧执行身份、顺序、上下文和载荷校验。
type RuntimeHostProtocolServer struct {
	runtimev1.UnimplementedRuntimeHostServer

	config RuntimeHostServerConfig

	mu          sync.Mutex
	entries     map[BackendIdentity]*runtimeHostServerEntry
	allowed     map[BackendIdentity]struct{}
	invocations chan struct{}
}

func NewRuntimeHostProtocolServer(config RuntimeHostServerConfig) (*RuntimeHostProtocolServer, error) {
	if (config.Mode != ModeHosted && config.Mode != ModeIsolated) || config.Backend == nil || len(config.AllowedRuntimes) == 0 {
		return nil, ErrInvalidManifest
	}
	allowed := make(map[BackendIdentity]struct{}, len(config.AllowedRuntimes))
	for _, identity := range config.AllowedRuntimes {
		if !stableIDPattern.MatchString(identity.ID) {
			return nil, ErrInvalidManifest
		}
		if _, err := packagecontract.ParseVersion(identity.Version); err != nil {
			return nil, ErrInvalidManifest
		}
		if _, exists := allowed[identity]; exists {
			return nil, ErrDuplicateID
		}
		allowed[identity] = struct{}{}
	}
	if config.MaxRuntimes == 0 {
		config.MaxRuntimes = 128
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 64
	}
	if config.MaxRuntimes < 1 || config.MaxRuntimes > 4096 ||
		config.MaxConcurrent < 1 || config.MaxConcurrent > 4096 {
		return nil, ErrInvalidManifest
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &RuntimeHostProtocolServer{
		config: config, entries: make(map[BackendIdentity]*runtimeHostServerEntry), allowed: allowed,
		invocations: make(chan struct{}, config.MaxConcurrent),
	}, nil
}

func RuntimeHostGRPCServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(maxRuntimeMessageBytes),
		grpc.MaxSendMsgSize(maxRuntimeMessageBytes),
	}
}

func (s *RuntimeHostProtocolServer) Describe(ctx context.Context, request *runtimev1.DescribeRequest) (*runtimev1.RuntimeDescription, error) {
	identity, err := decodeRuntimeIdentity(request.GetIdentity())
	if request == nil || hasUnknown(request) || err != nil {
		return nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	if !s.isAllowed(identity) {
		return nil, safeRuntimeStatus(codes.NotFound)
	}
	description, err := s.config.Backend.Describe(ctx, identity)
	if err != nil {
		return nil, mapRuntimeBackendStatus(err)
	}
	if description.ID != identity.ID || description.Version != identity.Version || description.Mode != s.config.Mode {
		return nil, safeRuntimeStatus(codes.Internal)
	}
	return &runtimev1.RuntimeDescription{
		RuntimeId: description.ID, Version: description.Version, Mode: description.Mode,
		SupportedProtocolVersions: []string{RuntimeHostProtocolVersion},
	}, nil
}

func (s *RuntimeHostProtocolServer) Start(ctx context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	identity, err := decodeLifecycleRequest(request)
	if err != nil {
		return nil, err
	}
	if !s.isAllowed(identity) {
		return nil, safeRuntimeStatus(codes.NotFound)
	}
	s.mu.Lock()
	if entry := s.entries[identity]; entry != nil {
		if entry.state == StateReady {
			s.mu.Unlock()
			return serverLifecycleResponse(identity, true, "ready"), nil
		}
		s.mu.Unlock()
		return nil, safeRuntimeStatus(codes.FailedPrecondition)
	}
	if len(s.entries) >= s.config.MaxRuntimes {
		s.mu.Unlock()
		return nil, safeRuntimeStatus(codes.ResourceExhausted)
	}
	entry := &runtimeHostServerEntry{state: StateLoading}
	s.entries[identity] = entry
	s.mu.Unlock()

	startErr := s.config.Backend.Start(ctx, identity)
	s.mu.Lock()
	if startErr != nil {
		entry.state = StateFailed
	} else {
		entry.state = StateReady
	}
	s.mu.Unlock()
	if startErr != nil {
		return nil, mapRuntimeBackendStatus(startErr)
	}
	return serverLifecycleResponse(identity, true, "ready"), nil
}

func (s *RuntimeHostProtocolServer) Health(ctx context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	identity, err := decodeLifecycleRequest(request)
	if err != nil {
		return nil, err
	}
	if !s.isAllowed(identity) {
		return nil, safeRuntimeStatus(codes.NotFound)
	}
	s.mu.Lock()
	entry := s.entries[identity]
	ready := entry != nil && entry.state == StateReady
	s.mu.Unlock()
	if !ready {
		return nil, safeRuntimeStatus(codes.FailedPrecondition)
	}
	if err := s.config.Backend.Health(ctx, identity); err != nil {
		s.markFailed(identity)
		return nil, mapRuntimeBackendStatus(err)
	}
	return serverLifecycleResponse(identity, true, "ready"), nil
}

func (s *RuntimeHostProtocolServer) Invoke(ctx context.Context, request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
	identity, governed, payload, err := s.decodeInvoke(request)
	if err != nil {
		return nil, err
	}
	if !s.isAllowed(identity) {
		return nil, safeRuntimeStatus(codes.NotFound)
	}
	select {
	case <-ctx.Done():
		return nil, mapRuntimeBackendStatus(ctx.Err())
	case s.invocations <- struct{}{}:
		defer func() { <-s.invocations }()
	default:
		return nil, safeRuntimeStatus(codes.ResourceExhausted)
	}
	if !s.beginInvoke(identity) {
		return nil, safeRuntimeStatus(codes.FailedPrecondition)
	}
	defer s.endInvoke(identity)

	invokeContext := ctx
	cancel := func() {}
	if !governed.Deadline.IsZero() {
		if current, exists := ctx.Deadline(); !exists || governed.Deadline.Before(current) {
			invokeContext, cancel = context.WithDeadline(ctx, governed.Deadline)
		}
	}
	defer cancel()
	result, invokeErr := s.config.Backend.Invoke(invokeContext, identity, governed, payload)
	if invokeErr != nil {
		var failure InvocationError
		if errors.As(invokeErr, &failure) && stableIDPattern.MatchString(failure.Code) {
			return &runtimev1.InvokeResponse{
				Identity: encodeRuntimeIdentity(identity), ErrorCode: failure.Code, Retryable: failure.Retryable,
			}, nil
		}
		if errors.Is(invokeErr, context.Canceled) || errors.Is(invokeErr, context.DeadlineExceeded) {
			return nil, mapRuntimeBackendStatus(invokeErr)
		}
		return &runtimev1.InvokeResponse{
			Identity: encodeRuntimeIdentity(identity), ErrorCode: "capability_failed",
		}, nil
	}
	if len(result) == 0 || len(result) > maxInvokeResultBytes || !json.Valid(result) {
		s.markFailed(identity)
		return nil, safeRuntimeStatus(codes.Internal)
	}
	return &runtimev1.InvokeResponse{
		Identity: encodeRuntimeIdentity(identity), Success: true, PayloadJson: append([]byte(nil), result...),
	}, nil
}

func (s *RuntimeHostProtocolServer) Stop(ctx context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	identity, err := decodeLifecycleRequest(request)
	if err != nil {
		return nil, err
	}
	if !s.isAllowed(identity) {
		return nil, safeRuntimeStatus(codes.NotFound)
	}
	s.mu.Lock()
	entry := s.entries[identity]
	if entry == nil {
		s.mu.Unlock()
		return serverLifecycleResponse(identity, false, "stopped"), nil
	}
	if entry.inFlight != 0 || (entry.state != StateReady && entry.state != StateFailed) {
		s.mu.Unlock()
		return nil, safeRuntimeStatus(codes.FailedPrecondition)
	}
	entry.state = StateUnloading
	s.mu.Unlock()

	if err := s.config.Backend.Stop(ctx, identity); err != nil {
		s.mu.Lock()
		entry.state = StateFailed
		s.mu.Unlock()
		return nil, mapRuntimeBackendStatus(err)
	}
	s.mu.Lock()
	delete(s.entries, identity)
	s.mu.Unlock()
	return serverLifecycleResponse(identity, false, "stopped"), nil
}

func (s *RuntimeHostProtocolServer) decodeInvoke(request *runtimev1.InvokeRequest) (BackendIdentity, contracts.RequestContext, json.RawMessage, error) {
	if request == nil || hasUnknown(request) || request.Context == nil || hasUnknown(request.Context) {
		return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	identity, err := decodeRuntimeIdentity(request.Identity)
	if err != nil {
		return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	now := s.config.Now().UTC()
	if request.Context.DeadlineUnixMs < 0 {
		return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	var deadline time.Time
	if request.Context.DeadlineUnixMs != 0 {
		deadline = time.UnixMilli(request.Context.DeadlineUnixMs).UTC()
		if deadline.After(now.Add(maxRuntimeDeadline)) {
			return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
		}
	}
	if request.Context.CallDepth > 64 {
		return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	importedCapabilities, err := decodeCapabilityProjections(request.Context.ImportedCapabilities)
	if err != nil {
		return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	governed := contracts.RequestContext{
		AppID: request.Context.AppId, EchoID: request.Context.EchoId, RequestID: request.Context.RequestId,
		TraceID: request.Context.TraceId, UserID: request.Context.UserId, SessionID: request.Context.SessionId,
		RunID: request.Context.RunId, ParentRunID: request.Context.ParentRunId, CallID: request.Context.CallId,
		CallDepth: uint16(request.Context.CallDepth), Deadline: deadline,
		IdempotencyKey: request.Context.IdempotencyKey, ConfirmationID: request.Context.ConfirmationId,
		ProtocolVersion:      request.Context.ProtocolVersion,
		TargetType:           request.Context.TargetType,
		CapabilityID:         request.Context.CapabilityId,
		ServiceID:            request.Context.ServiceId,
		ToolID:               request.Context.ToolId,
		ImportedCapabilities: importedCapabilities,
		PermissionScope:      append([]string(nil), request.Context.PermissionScope...),
		CallChain:            append([]string(nil), request.Context.CallChain...),
	}
	payload := append(json.RawMessage(nil), request.PayloadJson...)
	if err := validateRuntimeInvoke(governed, payload, now); err != nil {
		return BackendIdentity{}, contracts.RequestContext{}, nil, safeRuntimeStatus(codes.InvalidArgument)
	}
	return identity, governed, payload, nil
}

func decodeCapabilityProjections(values []*runtimev1.CapabilityProjection) ([]contracts.CapabilityProjection, error) {
	result := make([]contracts.CapabilityProjection, 0, len(values))
	for _, value := range values {
		if value == nil {
			return nil, errors.New("capability projection entry is nil")
		}
		result = append(result, contracts.CapabilityProjection{
			ID: value.Id, Version: value.Version, InputSchemaJSON: value.InputSchemaJson,
			RequiredPermissions: append([]string(nil), value.RequiredPermissions...),
		})
	}
	return result, nil
}

func (s *RuntimeHostProtocolServer) beginInvoke(identity BackendIdentity) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[identity]
	if entry == nil || entry.state != StateReady {
		return false
	}
	entry.inFlight++
	return true
}

func (s *RuntimeHostProtocolServer) isAllowed(identity BackendIdentity) bool {
	_, allowed := s.allowed[identity]
	return allowed
}

func (s *RuntimeHostProtocolServer) endInvoke(identity BackendIdentity) {
	s.mu.Lock()
	if entry := s.entries[identity]; entry != nil && entry.inFlight > 0 {
		entry.inFlight--
	}
	s.mu.Unlock()
}

func (s *RuntimeHostProtocolServer) markFailed(identity BackendIdentity) {
	s.mu.Lock()
	if entry := s.entries[identity]; entry != nil {
		entry.state = StateFailed
	}
	s.mu.Unlock()
}

func decodeLifecycleRequest(request *runtimev1.LifecycleRequest) (BackendIdentity, error) {
	if request == nil || hasUnknown(request) {
		return BackendIdentity{}, safeRuntimeStatus(codes.InvalidArgument)
	}
	identity, err := decodeRuntimeIdentity(request.Identity)
	if err != nil {
		return BackendIdentity{}, safeRuntimeStatus(codes.InvalidArgument)
	}
	return identity, nil
}

func decodeRuntimeIdentity(identity *runtimev1.RuntimeIdentity) (BackendIdentity, error) {
	if identity == nil || hasUnknown(identity) || !stableIDPattern.MatchString(identity.RuntimeId) ||
		identity.ProtocolVersion != RuntimeHostProtocolVersion {
		return BackendIdentity{}, ErrRuntimeProtocol
	}
	if _, err := packagecontract.ParseVersion(identity.Version); err != nil {
		return BackendIdentity{}, ErrRuntimeProtocol
	}
	return BackendIdentity{ID: identity.RuntimeId, Version: identity.Version}, nil
}

func encodeRuntimeIdentity(identity BackendIdentity) *runtimev1.RuntimeIdentity {
	return &runtimev1.RuntimeIdentity{
		RuntimeId: identity.ID, Version: identity.Version, ProtocolVersion: RuntimeHostProtocolVersion,
	}
}

func serverLifecycleResponse(identity BackendIdentity, ready bool, code string) *runtimev1.LifecycleResponse {
	return &runtimev1.LifecycleResponse{
		Identity: encodeRuntimeIdentity(identity), Ready: ready, StatusCode: code,
	}
}

func mapRuntimeBackendStatus(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return safeRuntimeStatus(codes.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return safeRuntimeStatus(codes.DeadlineExceeded)
	case errors.Is(err, ErrRuntimeBusy):
		return safeRuntimeStatus(codes.ResourceExhausted)
	default:
		return safeRuntimeStatus(codes.Unavailable)
	}
}

func safeRuntimeStatus(code codes.Code) error {
	return status.Error(code, "runtime host request failed")
}
