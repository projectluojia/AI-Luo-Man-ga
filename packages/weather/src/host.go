// host 实现 runtime_host v3 协议的宿主侧服务：身份回显、生命周期状态机、
// fail-closed 入参校验与稳定错误码映射，全部按内核 grpcRuntime 的响应契约执行。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/runtimev1"
	"github.com/projectluojia/weather/src/wx"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// 协议版本与闭式上界复刻内核 loader 常量（CI code-drift 测试防两侧漂移）。
	protocolVersion    = "3.0"
	maxResultBytes     = 256 << 10
	maxPayloadBytes    = 64 << 10
	maxCallDepth       = 64
	maxContextItems    = 64
	maxContextValueB   = 256
	maxContextDeadline = 24 * time.Hour
)

var stableIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// governedContext 是治理上下文的进程侧投影（对应 runtimev1.GovernedRequestContext）。
type governedContext struct {
	AppID           string
	Deadline        time.Time
	CallDepth       uint32
	CallChain       []string
	CallID          string
	CapabilityID    string
	RequestID       string
	EchoID          string
	IdempotencyKey  string
	ConfirmationID  string
	ProtocolVersion string
}

// capabilityService 是宿主对领域服务的最小视图（消费方接口，协议层测试可替换实现）。
type capabilityService interface {
	Current(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error)
	Hourly(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error)
	AQI(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error)
	Alerts(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error)
}

// hostServer 实现 runtimev1.RuntimeHostServer；状态机只服务 Stop 的宽限约束，
// 生命周期权威仍在内核（Describe→Start→Health→Invoke→Stop 顺序由内核保证）。
type hostServer struct {
	runtimev1.UnimplementedRuntimeHostServer

	service   capabilityService
	onStopped func()

	mu       sync.Mutex
	state    string
	inFlight int
}

func newHostServer(service capabilityService) *hostServer {
	return &hostServer{service: service, state: "loading"}
}

func newHostServerWith(service capabilityService, onStopped func()) *hostServer {
	return &hostServer{service: service, onStopped: onStopped, state: "loading"}
}

func (s *hostServer) Describe(_ context.Context, request *runtimev1.DescribeRequest) (*runtimev1.RuntimeDescription, error) {
	identity, err := decodeIdentity(request.GetIdentity())
	if err != nil {
		return nil, invalidArgument
	}
	return &runtimev1.RuntimeDescription{
		RuntimeId: identity.RuntimeId, Version: identity.Version, Mode: packagecontract.ModeIsolated,
		SupportedProtocolVersions: []string{protocolVersion},
	}, nil
}

func (s *hostServer) Start(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	identity, err := decodeLifecycle(request)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.state == "failed" {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "runtime host request failed")
	}
	s.state = "ready"
	s.mu.Unlock()
	return lifecycleResponse(identity, true, "ready"), nil
}

func (s *hostServer) Health(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	identity, err := decodeLifecycle(request)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	ready := s.state == "ready"
	s.mu.Unlock()
	if !ready {
		return nil, status.Error(codes.FailedPrecondition, "runtime host request failed")
	}
	return lifecycleResponse(identity, true, "ready"), nil
}

func (s *hostServer) Invoke(ctx context.Context, request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
	identity, governed, payload, err := decodeInvoke(request)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.state != "ready" {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "runtime host request failed")
	}
	s.inFlight++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	invokeContext := ctx
	cancel := func() {}
	if !governed.Deadline.IsZero() {
		if current, exists := ctx.Deadline(); !exists || governed.Deadline.Before(current) {
			invokeContext, cancel = context.WithDeadline(ctx, governed.Deadline)
		}
	}
	defer cancel()

	result, invokeErr := s.invoke(invokeContext, governed, payload)
	if invokeErr != nil {
		return failureResponse(identity, invokeErr), nil
	}
	if len(result) == 0 || len(result) > maxResultBytes || !json.Valid(result) {
		s.markFailed()
		return &runtimev1.InvokeResponse{
			Identity: echoIdentity(identity), Success: false,
			ErrorCode: wx.CodeCapabilityFailed, Retryable: false,
		}, nil
	}
	return &runtimev1.InvokeResponse{
		Identity: echoIdentity(identity), Success: true, PayloadJson: append([]byte(nil), result...),
	}, nil
}

func (s *hostServer) invoke(ctx context.Context, governed governedContext, payload json.RawMessage) (json.RawMessage, error) {
	switch governed.CapabilityID {
	case "weather.current":
		return s.service.Current(ctx, governed.AppID, payload)
	case "weather.hourly":
		return s.service.Hourly(ctx, governed.AppID, payload)
	case "weather.aqi":
		return s.service.AQI(ctx, governed.AppID, payload)
	case "weather.alerts":
		return s.service.Alerts(ctx, governed.AppID, payload)
	default:
		return nil, &wx.Error{Code: wx.CodeInvalidArguments, Message: "unknown capability: " + governed.CapabilityID}
	}
}

func (s *hostServer) Stop(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	identity, err := decodeLifecycle(request)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.inFlight != 0 || (s.state != "ready" && s.state != "failed" && s.state != "loading") {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "runtime host request failed")
	}
	s.state = "stopped"
	s.mu.Unlock()
	if s.onStopped != nil {
		go s.onStopped()
	}
	return lifecycleResponse(identity, false, "stopped"), nil
}

func (s *hostServer) markFailed() {
	s.mu.Lock()
	s.state = "failed"
	s.mu.Unlock()
}

func decodeInvoke(request *runtimev1.InvokeRequest) (*runtimev1.RuntimeIdentity, governedContext, json.RawMessage, error) {
	if request == nil || request.Identity == nil || request.Context == nil {
		return nil, governedContext{}, nil, invalidArgument
	}
	identity, err := decodeIdentity(request.Identity)
	if err != nil {
		return nil, governedContext{}, nil, invalidArgument
	}
	context := request.Context
	governed := governedContext{
		AppID: context.AppId, EchoID: context.EchoId, RequestID: context.RequestId,
		CallDepth: context.CallDepth, IdempotencyKey: context.IdempotencyKey,
		ConfirmationID: context.ConfirmationId, ProtocolVersion: context.ProtocolVersion,
		CallChain: append([]string(nil), context.CallChain...), CallID: context.CallId,
		CapabilityID: context.CapabilityId,
	}
	if context.DeadlineUnixMs < 0 {
		return nil, governedContext{}, nil, invalidArgument
	}
	if context.DeadlineUnixMs != 0 {
		governed.Deadline = time.UnixMilli(context.DeadlineUnixMs).UTC()
		if governed.Deadline.After(time.Now().Add(maxContextDeadline)) {
			return nil, governedContext{}, nil, invalidArgument
		}
	}
	payload := append(json.RawMessage(nil), request.PayloadJson...)
	if err := validateInvoke(governed, payload); err != nil {
		return nil, governedContext{}, nil, invalidArgument
	}
	return identity, governed, payload, nil
}

// validateInvoke 复刻内核侧入参闭式上界：载荷、调用深度、上下文串与能力标识。
// 违例返回错误并以 gRPC InvalidArgument 应答（内核映射为协议违例）。
func validateInvoke(governed governedContext, payload json.RawMessage) error {
	if len(payload) == 0 || len(payload) > maxPayloadBytes || !json.Valid(payload) ||
		governed.CallDepth > maxCallDepth || len(governed.CallChain) > maxContextItems {
		return errors.New("invoke payload is invalid")
	}
	if governed.AppID == "" || governed.EchoID == "" || governed.RequestID == "" {
		return errors.New("governed context is incomplete")
	}
	if !governed.Deadline.IsZero() && !time.Now().Before(governed.Deadline) {
		return errors.New("request deadline has already expired")
	}
	values := []string{
		governed.AppID, governed.EchoID, governed.RequestID, governed.CallID, governed.IdempotencyKey,
		governed.ConfirmationID, governed.ProtocolVersion, governed.CapabilityID,
	}
	values = append(values, governed.CallChain...)
	for _, value := range values {
		if len(value) > maxContextValueB || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("governed context value is invalid")
		}
	}
	if !stableIDPattern.MatchString(governed.CapabilityID) {
		return errors.New("capability id is invalid")
	}
	return nil
}

func decodeIdentity(identity *runtimev1.RuntimeIdentity) (*runtimev1.RuntimeIdentity, error) {
	if identity == nil || !stableIDPattern.MatchString(identity.RuntimeId) ||
		identity.ProtocolVersion != protocolVersion {
		return nil, errors.New("runtime identity is invalid")
	}
	if _, err := packagecontract.ParseVersion(identity.Version); err != nil {
		return nil, err
	}
	return identity, nil
}

func decodeLifecycle(request *runtimev1.LifecycleRequest) (*runtimev1.RuntimeIdentity, error) {
	if request == nil {
		return nil, errors.New("lifecycle request is invalid")
	}
	return decodeIdentity(request.Identity)
}

func echoIdentity(identity *runtimev1.RuntimeIdentity) *runtimev1.RuntimeIdentity {
	return &runtimev1.RuntimeIdentity{
		RuntimeId: identity.RuntimeId, Version: identity.Version, ProtocolVersion: identity.ProtocolVersion,
	}
}

func lifecycleResponse(identity *runtimev1.RuntimeIdentity, ready bool, code string) *runtimev1.LifecycleResponse {
	return &runtimev1.LifecycleResponse{Identity: echoIdentity(identity), Ready: ready, StatusCode: code}
}

var invalidArgument = status.Error(codes.InvalidArgument, "runtime host request is invalid")

// failureResponse 把领域错误映射进 InvokeResponse：稳定码透传，取消/超时归
// deadline_exceeded，其余归为 capability_failed（guest 不发明私有码）。
func failureResponse(identity *runtimev1.RuntimeIdentity, err error) *runtimev1.InvokeResponse {
	response := &runtimev1.InvokeResponse{Identity: echoIdentity(identity), Success: false}
	var domain *wx.Error
	if errors.As(err, &domain) && stableIDPattern.MatchString(domain.Code) {
		response.ErrorCode = domain.Code
		response.Retryable = domain.Retryable
		return response
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		response.ErrorCode = wx.CodeDeadlineExceeded
		response.Retryable = true
		return response
	}
	response.ErrorCode = wx.CodeCapabilityFailed
	return response
}
