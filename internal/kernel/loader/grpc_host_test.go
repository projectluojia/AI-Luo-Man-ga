package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type fakeRuntimeHostServer struct {
	runtimev1.UnimplementedRuntimeHostServer

	mode              string
	describeProtocols []string
	describeUnknown   bool
	invoke            func(*runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error)

	mu       sync.Mutex
	requests []*runtimev1.InvokeRequest
	stops    int
}

func (s *fakeRuntimeHostServer) Describe(_ context.Context, request *runtimev1.DescribeRequest) (*runtimev1.RuntimeDescription, error) {
	protocols := s.describeProtocols
	if protocols == nil {
		protocols = []string{loader.RuntimeHostProtocolVersion}
	}
	response := &runtimev1.RuntimeDescription{
		RuntimeId: request.Identity.RuntimeId, Version: request.Identity.Version,
		Mode: s.mode, SupportedProtocolVersions: append([]string(nil), protocols...),
	}
	if s.describeUnknown {
		response.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	}
	return response, nil
}

func (s *fakeRuntimeHostServer) Start(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return readyResponse(request.Identity), nil
}

func (s *fakeRuntimeHostServer) Health(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return readyResponse(request.Identity), nil
}

func (s *fakeRuntimeHostServer) Invoke(_ context.Context, request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, proto.Clone(request).(*runtimev1.InvokeRequest))
	invoke := s.invoke
	s.mu.Unlock()
	if invoke != nil {
		return invoke(request)
	}
	return &runtimev1.InvokeResponse{
		Identity: cloneIdentity(request.Identity), Success: true, PayloadJson: []byte(`{"ok":true}`),
	}, nil
}

func (s *fakeRuntimeHostServer) Stop(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
	return &runtimev1.LifecycleResponse{
		Identity: cloneIdentity(request.Identity), Ready: false, StatusCode: "stopped",
	}, nil
}

func (s *fakeRuntimeHostServer) snapshot() ([]*runtimev1.InvokeRequest, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*runtimev1.InvokeRequest(nil), s.requests...), s.stops
}

func readyResponse(identity *runtimev1.RuntimeIdentity) *runtimev1.LifecycleResponse {
	return &runtimev1.LifecycleResponse{
		Identity: cloneIdentity(identity), Ready: true, StatusCode: "ready",
	}
}

func cloneIdentity(identity *runtimev1.RuntimeIdentity) *runtimev1.RuntimeIdentity {
	if identity == nil {
		return nil
	}
	return proto.Clone(identity).(*runtimev1.RuntimeIdentity)
}

func startRuntimeHost(t *testing.T, implementation runtimev1.RuntimeHostServer) (func(context.Context, string) (net.Conn, error), *atomic.Int32) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(loader.RuntimeHostGRPCServerOptions()...)
	runtimev1.RegisterRuntimeHostServer(server, implementation)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	var dials atomic.Int32
	return func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return listener.Dial()
	}, &dials
}

func newRuntimeGRPCHost(t *testing.T, mode string, dialer func(context.Context, string) (net.Conn, error), verifies *atomic.Int32) *loader.GRPCHost {
	t.Helper()
	host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: mode, Address: "unix:/runtime-host-test.sock", Dialer: dialer,
		VerifyInstalled: func(context.Context, loader.Manifest) error {
			verifies.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func runtimeManifest(id, mode string) loader.Manifest {
	return loader.Manifest{
		ID: id, Version: "1.2.3", Mode: mode, LockedDigest: digest,
	}
}

func governedRuntimeRequest() contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "campus-services", EchoID: "echo-1", RequestID: "request-1", TraceID: "trace-1",
		RunID: "run-1", ParentRunID: "parent-1", CallID: "call-1", CallDepth: 2,
		IdempotencyKey: "operation-1", ConfirmationID: "confirmation-1", ProtocolVersion: "1.0",
		TargetType: "capability", CapabilityID: "campus.bus.routes.list", ServiceID: "campus",
		PermissionScope: []string{"bus.read"}, CallChain: []string{"first"},
	}
}

func TestHostedGRPCHostSharesConnectionAndPreservesGovernedContext(t *testing.T) {
	implementation := &fakeRuntimeHostServer{mode: loader.ModeHosted}
	dialer, dials := startRuntimeHost(t, implementation)
	var verifies atomic.Int32
	host := newRuntimeGRPCHost(t, loader.ModeHosted, dialer, &verifies)
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"hosted.one", "hosted.two"} {
		if err := manager.Register(context.Background(), runtimeManifest(id, loader.ModeHosted)); err != nil {
			t.Fatal(err)
		}
	}

	var loads sync.WaitGroup
	failures := make(chan error, 2)
	for _, id := range []string{"hosted.one", "hosted.two"} {
		loads.Add(1)
		go func(runtimeID string) {
			defer loads.Done()
			failures <- manager.EnsureLoaded(context.Background(), runtimeID)
		}(id)
	}
	loads.Wait()
	close(failures)
	for loadErr := range failures {
		if loadErr != nil {
			t.Fatal(loadErr)
		}
	}
	// 注册期两个清单各 Verify 一次绑定宿主，加载期 loadRuntime 各再 Verify 一次。
	if dials.Load() != 1 || verifies.Load() != 4 {
		t.Fatalf("dials=%d verifies=%d", dials.Load(), verifies.Load())
	}

	handler := manager.Handler("hosted.one")
	result, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{"route_id":"route-1"}`))
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
	requests, _ := implementation.snapshot()
	if len(requests) != 1 {
		t.Fatalf("invoke requests=%d", len(requests))
	}
	got := requests[0].Context
	if got.AppId != "campus-services" || got.EchoId != "echo-1" || got.RunId != "run-1" ||
		got.CallId != "call-1" || got.CallDepth != 2 || got.DeadlineUnixMs != 0 ||
		got.TargetType != "capability" || got.CapabilityId != "campus.bus.routes.list" ||
		got.ServiceId != "campus" || got.ToolId != "" ||
		len(got.PermissionScope) != 1 || got.PermissionScope[0] != "bus.read" {
		t.Fatalf("governed context=%#v", got)
	}

	if err := manager.SweepIdle(context.Background(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("idle sweep closed active hosted connection: %v", err)
	}
	if dials.Load() != 1 {
		t.Fatalf("idle sweep caused redial: %d", dials.Load())
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, stops := implementation.snapshot()
	if stops != 2 {
		t.Fatalf("stops=%d", stops)
	}
}

func TestIsolatedGRPCHostUsesOwnedConnectionsAndRejectsLoadAfterClose(t *testing.T) {
	implementation := &fakeRuntimeHostServer{mode: loader.ModeIsolated}
	dialer, dials := startRuntimeHost(t, implementation)
	var verifies atomic.Int32
	host := newRuntimeGRPCHost(t, loader.ModeIsolated, dialer, &verifies)
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"isolated.one", "isolated.two"} {
		manifest := runtimeManifest(id, loader.ModeIsolated)
		if err := manager.Register(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
		if err := manager.EnsureLoaded(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 2 {
		t.Fatalf("dials=%d", dials.Load())
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Load(context.Background(), runtimeManifest("isolated.three", loader.ModeIsolated)); !errors.Is(err, loader.ErrShuttingDown) {
		t.Fatalf("load after close error=%v", err)
	}
}

func TestGRPCHostRejectsProtocolMismatchAndCleansLoadedRuntime(t *testing.T) {
	implementation := &fakeRuntimeHostServer{
		mode: loader.ModeHosted, describeProtocols: []string{"3.0"},
	}
	dialer, _ := startRuntimeHost(t, implementation)
	var verifies atomic.Int32
	host := newRuntimeGRPCHost(t, loader.ModeHosted, dialer, &verifies)
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("hosted.mismatch", loader.ModeHosted)); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "hosted.mismatch"); !errors.Is(err, loader.ErrLoadFailed) ||
		!errors.Is(err, loader.ErrRuntimeProtocol) {
		t.Fatalf("load error=%v", err)
	}
	_, stops := implementation.snapshot()
	if stops != 1 {
		t.Fatalf("stops=%d", stops)
	}
}

func TestGRPCHostRejectsMalformedResponsesAndSanitizesRemoteErrors(t *testing.T) {
	implementation := &fakeRuntimeHostServer{mode: loader.ModeHosted}
	dialer, _ := startRuntimeHost(t, implementation)
	var verifies atomic.Int32
	host := newRuntimeGRPCHost(t, loader.ModeHosted, dialer, &verifies)
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("hosted.validation", loader.ModeHosted)); err != nil {
		t.Fatal(err)
	}
	handler := manager.Handler("hosted.validation")

	if _, err := handler(context.Background(), contracts.RequestContext{}, json.RawMessage(`{}`)); !errors.Is(err, contracts.ErrMissingAppID) {
		t.Fatalf("invalid request error=%v", err)
	}
	requests, _ := implementation.snapshot()
	if len(requests) != 0 {
		t.Fatalf("invalid request crossed boundary: %d", len(requests))
	}

	implementation.mu.Lock()
	implementation.invoke = func(request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
		return &runtimev1.InvokeResponse{
			Identity: cloneIdentity(request.Identity), ErrorCode: "permission_denied",
			Retryable: true,
		}, nil
	}
	implementation.mu.Unlock()
	_, err = handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`))
	var invocationError loader.InvocationError
	if !errors.As(err, &invocationError) || invocationError.Code != "permission_denied" ||
		invocationError.Retryable != true || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/srv/") {
		t.Fatalf("invocation error=%#v raw=%v", invocationError, err)
	}

	implementation.mu.Lock()
	implementation.invoke = func(request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
		response := &runtimev1.InvokeResponse{
			Identity: cloneIdentity(request.Identity), Success: true, PayloadJson: []byte(`{}`),
		}
		response.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		return response, nil
	}
	implementation.mu.Unlock()
	if _, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`)); !errors.Is(err, loader.ErrRuntimeProtocol) {
		t.Fatalf("unknown response error=%v", err)
	}
	if err := manager.RecoverFailed(context.Background(), "hosted.validation"); err != nil {
		t.Fatal(err)
	}

	implementation.mu.Lock()
	implementation.invoke = func(*runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
		return nil, status.Error(codes.Internal, "host secret /srv/private")
	}
	implementation.mu.Unlock()
	if _, err := handler(context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`)); !errors.Is(err, loader.ErrUnavailable) ||
		strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/srv/") {
		t.Fatalf("transport error=%v", err)
	}
}

func TestNewGRPCHostRejectsUnsupportedOrNonLocalConfiguration(t *testing.T) {
	t.Parallel()
	verify := func(context.Context, loader.Manifest) error { return nil }
	tests := []loader.GRPCHostConfig{
		{Mode: loader.ModeHosted, Address: "0.0.0.0:9000", VerifyInstalled: verify},
		{Mode: "remote", Address: "127.0.0.1:9000", VerifyInstalled: verify},
		{Mode: loader.ModeHosted, Address: "unix:relative.sock", VerifyInstalled: verify},
		{Mode: loader.ModeHosted, Address: "unix:/runtime.sock"},
		{Mode: loader.ModeHosted, Address: "unix:/runtime.sock", VerifyInstalled: verify, DialTimeout: time.Millisecond},
		{Mode: loader.ModeHosted, Address: "unix:/runtime.sock", VerifyInstalled: verify, MaxRuntimes: -1},
		{Mode: loader.ModeHosted, Address: "unix:/runtime.sock", VerifyInstalled: verify, MaxConcurrent: 4097},
	}
	for _, config := range tests {
		if _, err := loader.NewGRPCHost(config); !errors.Is(err, loader.ErrInvalidManifest) {
			t.Fatalf("config=%#v error=%v", config, err)
		}
	}
}

func TestHostedGRPCHostEnforcesRuntimeAndInvocationCapacity(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	implementation := &fakeRuntimeHostServer{
		mode: loader.ModeHosted,
		invoke: func(request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
			close(entered)
			<-release
			return &runtimev1.InvokeResponse{
				Identity: cloneIdentity(request.Identity), Success: true, PayloadJson: []byte(`{}`),
			}, nil
		},
	}
	dialer, _ := startRuntimeHost(t, implementation)
	host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: loader.ModeHosted, Address: "unix:/runtime-host-test.sock", Dialer: dialer,
		VerifyInstalled: func(context.Context, loader.Manifest) error { return nil },
		MaxRuntimes:     1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"hosted.capacity.one", "hosted.capacity.two"} {
		if err := manager.Register(context.Background(), runtimeManifest(id, loader.ModeHosted)); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.EnsureLoaded(context.Background(), "hosted.capacity.one"); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "hosted.capacity.two"); !errors.Is(err, loader.ErrLoadFailed) ||
		!errors.Is(err, loader.ErrRuntimeBusy) {
		t.Fatalf("runtime capacity error=%v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, invokeErr := manager.Handler("hosted.capacity.one")(
			context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`),
		)
		firstDone <- invokeErr
	}()
	<-entered
	if _, err := manager.Handler("hosted.capacity.one")(
		context.Background(), governedRuntimeRequest(), json.RawMessage(`{}`),
	); !errors.Is(err, loader.ErrRuntimeBusy) {
		t.Fatalf("invocation capacity error=%v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := manager.Unload(context.Background(), "hosted.capacity.one"); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverFailed(context.Background(), "hosted.capacity.two"); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsureLoaded(context.Background(), "hosted.capacity.two"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
