package loader

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const RuntimeHostProtocolVersion = "2.0"

const (
	maxRuntimeMessageBytes = 512 << 10
	maxInvokePayloadBytes  = 64 << 10
	maxInvokeResultBytes   = 256 << 10
	maxContextItems        = 64
	maxContextValueBytes   = 256
)

var ErrRuntimeProtocol = errors.New("runtime host protocol violation")

type InvocationError struct {
	Code      string
	Retryable bool
}

func (e InvocationError) Error() string {
	return "runtime invocation failed"
}

type GRPCHostConfig struct {
	Mode            string
	Address         string
	VerifyInstalled func(context.Context, Manifest) error
	Dialer          func(context.Context, string) (net.Conn, error)
	DialTimeout     time.Duration
	MaxRuntimes     int
	MaxConcurrent   int
}

type GRPCHost struct {
	config GRPCHostConfig

	mu          sync.Mutex
	connection  *grpc.ClientConn
	client      runtimev1.RuntimeHostClient
	dialing     chan struct{}
	closed      bool
	attached    int
	invocations chan struct{}
}

func NewGRPCHost(config GRPCHostConfig) (*GRPCHost, error) {
	if (config.Mode != ModeHosted && config.Mode != ModeIsolated) ||
		!IsLocalRuntimeAddress(config.Address) || config.VerifyInstalled == nil {
		return nil, ErrInvalidManifest
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.DialTimeout < 100*time.Millisecond || config.DialTimeout > time.Minute {
		return nil, ErrInvalidManifest
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
	return &GRPCHost{config: config, invocations: make(chan struct{}, config.MaxConcurrent)}, nil
}

func (h *GRPCHost) Verify(ctx context.Context, manifest Manifest) error {
	if manifest.Mode != h.config.Mode {
		return ErrUnsupportedMode
	}
	return h.config.VerifyInstalled(ctx, manifest)
}

func (h *GRPCHost) Load(ctx context.Context, manifest Manifest) (Runtime, error) {
	if err := h.reserveRuntime(); err != nil {
		return nil, err
	}
	connection, client, owned, err := h.connect(ctx)
	if err != nil {
		h.releaseRuntime()
		return nil, err
	}
	return &grpcRuntime{
		manifest:    manifest,
		client:      client,
		connection:  connection,
		owned:       owned,
		invocations: h.invocations,
		onStopped:   h.releaseRuntime,
	}, nil
}

func (h *GRPCHost) reserveRuntime() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrShuttingDown
	}
	if h.attached >= h.config.MaxRuntimes {
		return ErrRuntimeBusy
	}
	h.attached++
	return nil
}

func (h *GRPCHost) releaseRuntime() {
	h.mu.Lock()
	if h.attached > 0 {
		h.attached--
	}
	h.mu.Unlock()
}

func (h *GRPCHost) connect(ctx context.Context) (*grpc.ClientConn, runtimev1.RuntimeHostClient, bool, error) {
	if h.config.Mode == ModeIsolated {
		h.mu.Lock()
		closed := h.closed
		h.mu.Unlock()
		if closed {
			return nil, nil, false, ErrShuttingDown
		}
		connection, err := h.dial(ctx)
		if err != nil {
			return nil, nil, false, err
		}
		return connection, runtimev1.NewRuntimeHostClient(connection), true, nil
	}
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return nil, nil, false, ErrShuttingDown
		}
		if h.connection != nil {
			connection, client := h.connection, h.client
			h.mu.Unlock()
			return connection, client, false, nil
		}
		if h.dialing != nil {
			wait := h.dialing
			h.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, false, ctx.Err()
			case <-wait:
				continue
			}
		}
		h.dialing = make(chan struct{})
		wait := h.dialing
		h.mu.Unlock()

		connection, err := h.dial(ctx)
		h.mu.Lock()
		if err == nil && !h.closed {
			h.connection = connection
			h.client = runtimev1.NewRuntimeHostClient(connection)
		} else if err == nil {
			err = errors.Join(ErrShuttingDown, connection.Close())
		}
		close(wait)
		h.dialing = nil
		client := h.client
		h.mu.Unlock()
		if err != nil {
			return nil, nil, false, err
		}
		return connection, client, false, nil
	}
}

func (h *GRPCHost) dial(ctx context.Context) (*grpc.ClientConn, error) {
	dialContext, cancel := context.WithTimeout(ctx, h.config.DialTimeout)
	defer cancel()
	options := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay: 20 * time.Millisecond, Multiplier: 1.5, Jitter: 0.2, MaxDelay: 200 * time.Millisecond,
			},
			MinConnectTimeout: 250 * time.Millisecond,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxRuntimeMessageBytes),
			grpc.MaxCallSendMsgSize(maxRuntimeMessageBytes),
		),
	}
	if h.config.Dialer != nil {
		options = append(options, grpc.WithContextDialer(h.config.Dialer))
	}
	connection, err := grpc.DialContext(dialContext, h.config.Address, options...)
	if err != nil {
		return nil, normalizeRuntimeRPCError(err)
	}
	return connection, nil
}

func (h *GRPCHost) Close(context.Context) error {
	h.mu.Lock()
	h.closed = true
	connection := h.connection
	h.connection = nil
	h.client = nil
	h.mu.Unlock()
	if connection != nil {
		return connection.Close()
	}
	return nil
}

type grpcRuntime struct {
	manifest    Manifest
	client      runtimev1.RuntimeHostClient
	connection  *grpc.ClientConn
	owned       bool
	invocations chan struct{}
	onStopped   func()

	mu      sync.Mutex
	stopped bool
}

func (r *grpcRuntime) Describe(ctx context.Context) (Description, error) {
	response, err := r.client.Describe(ctx, &runtimev1.DescribeRequest{Identity: r.identity()})
	if err != nil {
		return Description{}, normalizeRuntimeRPCError(err)
	}
	if hasUnknown(response) || response.RuntimeId != r.manifest.ID || response.Version != r.manifest.Version ||
		response.Mode != r.manifest.Mode || !slices.Contains(response.SupportedProtocolVersions, RuntimeHostProtocolVersion) {
		return Description{}, ErrRuntimeProtocol
	}
	return Description{ID: response.RuntimeId, Version: response.Version, Mode: response.Mode}, nil
}

func (r *grpcRuntime) Start(ctx context.Context) error {
	response, err := r.client.Start(ctx, &runtimev1.LifecycleRequest{Identity: r.identity()})
	if err != nil {
		return normalizeRuntimeRPCError(err)
	}
	if hasUnknown(response) || !r.validIdentity(response.Identity) || !response.Ready || response.StatusCode != "ready" {
		return ErrRuntimeProtocol
	}
	return nil
}

func (r *grpcRuntime) Health(ctx context.Context) error {
	response, err := r.client.Health(ctx, &runtimev1.LifecycleRequest{Identity: r.identity()})
	if err != nil {
		return normalizeRuntimeRPCError(err)
	}
	if hasUnknown(response) || !r.validIdentity(response.Identity) || !response.Ready || response.StatusCode != "ready" {
		return ErrRuntimeProtocol
	}
	return nil
}

func (r *grpcRuntime) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	if err := validateRuntimeInvoke(request, payload, time.Now().UTC()); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r.invocations <- struct{}{}:
		defer func() { <-r.invocations }()
	default:
		return nil, ErrRuntimeBusy
	}
	response, err := r.client.Invoke(ctx, &runtimev1.InvokeRequest{
		Identity: r.identity(),
		Context: &runtimev1.GovernedRequestContext{
			AppId: request.AppID, EchoId: request.EchoID, RequestId: request.RequestID,
			TraceId: request.TraceID, UserId: request.UserID, SessionId: request.SessionID,
			RunId: request.RunID, ParentRunId: request.ParentRunID, CallDepth: uint32(request.CallDepth),
			DeadlineUnixMs: deadlineUnixMilli(request.Deadline), IdempotencyKey: request.IdempotencyKey,
			ConfirmationId: request.ConfirmationID, ProtocolVersion: request.ProtocolVersion,
			PermissionScope: append([]string(nil), request.PermissionScope...),
			CallChain:       append([]string(nil), request.CallChain...),
			CallId:          request.CallID,
			TargetType:      request.TargetType,
			CapabilityId:    request.CapabilityID,
			ServiceId:       request.ServiceID,
			ToolId:          request.ToolID,
		},
		PayloadJson: append([]byte(nil), payload...),
	})
	if err != nil {
		return nil, normalizeRuntimeRPCError(err)
	}
	if hasUnknown(response) || !r.validIdentity(response.Identity) {
		return nil, ErrRuntimeProtocol
	}
	if response.Success {
		if len(response.PayloadJson) == 0 || len(response.PayloadJson) > maxInvokeResultBytes ||
			!json.Valid(response.PayloadJson) || response.ErrorCode != "" || response.Retryable {
			return nil, ErrRuntimeProtocol
		}
		return append(json.RawMessage(nil), response.PayloadJson...), nil
	}
	if len(response.PayloadJson) != 0 || !stableIDPattern.MatchString(response.ErrorCode) ||
		response.ErrorCode == "" {
		return nil, ErrRuntimeProtocol
	}
	return nil, InvocationError{Code: response.ErrorCode, Retryable: response.Retryable}
}

func (r *grpcRuntime) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	response, err := r.client.Stop(ctx, &runtimev1.LifecycleRequest{Identity: r.identity()})
	if err != nil {
		return normalizeRuntimeRPCError(err)
	}
	if hasUnknown(response) || !r.validIdentity(response.Identity) || response.Ready || response.StatusCode != "stopped" {
		return ErrRuntimeProtocol
	}
	r.stopped = true
	if r.onStopped != nil {
		r.onStopped()
		r.onStopped = nil
	}
	if r.owned {
		return r.closeTransportLocked()
	}
	return nil
}

func (r *grpcRuntime) closeTransport() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeTransportLocked()
}

func (r *grpcRuntime) closeTransportLocked() error {
	if !r.owned || r.connection == nil {
		return nil
	}
	connection := r.connection
	r.connection = nil
	return connection.Close()
}

func (r *grpcRuntime) identity() *runtimev1.RuntimeIdentity {
	return &runtimev1.RuntimeIdentity{
		RuntimeId: r.manifest.ID, Version: r.manifest.Version, ProtocolVersion: RuntimeHostProtocolVersion,
	}
}

func (r *grpcRuntime) validIdentity(identity *runtimev1.RuntimeIdentity) bool {
	return !hasUnknown(identity) && identity.RuntimeId == r.manifest.ID &&
		identity.Version == r.manifest.Version && identity.ProtocolVersion == RuntimeHostProtocolVersion
}

func validateRuntimeInvoke(request contracts.RequestContext, payload json.RawMessage, now time.Time) error {
	if err := request.Validate(now); err != nil {
		return err
	}
	if len(payload) == 0 || len(payload) > maxInvokePayloadBytes || !json.Valid(payload) ||
		request.CallDepth > 64 || len(request.PermissionScope) > maxContextItems || len(request.CallChain) > maxContextItems {
		return ErrRuntimeProtocol
	}
	values := []string{
		request.AppID, request.EchoID, request.RequestID, request.TraceID, request.UserID, request.SessionID,
		request.RunID, request.ParentRunID, request.CallID, request.IdempotencyKey, request.ConfirmationID, request.ProtocolVersion,
		request.TargetType, request.CapabilityID, request.ServiceID, request.ToolID,
	}
	values = append(values, request.PermissionScope...)
	values = append(values, request.CallChain...)
	for _, value := range values {
		if len(value) > maxContextValueBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return ErrRuntimeProtocol
		}
	}
	switch request.TargetType {
	case "capability":
		if !stableIDPattern.MatchString(request.CapabilityID) || !stableIDPattern.MatchString(request.ServiceID) || request.ToolID != "" {
			return ErrRuntimeProtocol
		}
	case "tool":
		if request.CapabilityID != "" || !stableIDPattern.MatchString(request.ServiceID) || !stableIDPattern.MatchString(request.ToolID) {
			return ErrRuntimeProtocol
		}
	default:
		return ErrRuntimeProtocol
	}
	return nil
}

func deadlineUnixMilli(deadline time.Time) int64 {
	if deadline.IsZero() {
		return 0
	}
	return deadline.UnixMilli()
}

func normalizeRuntimeRPCError(err error) error {
	switch {
	case errors.Is(err, context.Canceled), status.Code(err) == codes.Canceled:
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded), status.Code(err) == codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case status.Code(err) == codes.ResourceExhausted:
		return ErrRuntimeBusy
	case status.Code(err) == codes.InvalidArgument, status.Code(err) == codes.FailedPrecondition,
		status.Code(err) == codes.DataLoss, status.Code(err) == codes.Unimplemented:
		return ErrRuntimeProtocol
	default:
		return ErrUnavailable
	}
}

func hasUnknown(message proto.Message) bool {
	return message == nil || len(message.ProtoReflect().GetUnknown()) != 0
}

// IsLocalRuntimeAddress 校验运行时宿主地址只能是 loopback 或绝对 Unix socket：
// 明文 gRPC 只允许同 Deployment 本机边界，非本机地址一律拒绝。
func IsLocalRuntimeAddress(address string) bool {
	if strings.HasPrefix(address, "unix:") {
		socketPath := strings.TrimPrefix(address, "unix:")
		return strings.HasPrefix(socketPath, "/")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
