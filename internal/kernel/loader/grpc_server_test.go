package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeRuntimeBackend struct {
	mode string

	startErr  error
	healthErr error
	invokeErr error
	stopErr   error
	result    json.RawMessage
	block     chan struct{}
	entered   chan struct{}

	starts  atomic.Int32
	health  atomic.Int32
	invokes atomic.Int32
	stops   atomic.Int32

	mu       sync.Mutex
	contexts []contracts.RequestContext
}

func (b *fakeRuntimeBackend) Describe(_ context.Context, identity loader.BackendIdentity) (loader.Description, error) {
	return loader.Description{ID: identity.ID, Version: identity.Version, Mode: b.mode}, nil
}

func (b *fakeRuntimeBackend) Start(context.Context, loader.BackendIdentity) error {
	b.starts.Add(1)
	return b.startErr
}

func (b *fakeRuntimeBackend) Health(context.Context, loader.BackendIdentity) error {
	b.health.Add(1)
	return b.healthErr
}

func (b *fakeRuntimeBackend) Invoke(_ context.Context, _ loader.BackendIdentity, request contracts.RequestContext, _ json.RawMessage) (json.RawMessage, error) {
	b.invokes.Add(1)
	b.mu.Lock()
	b.contexts = append(b.contexts, request)
	b.mu.Unlock()
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	if b.block != nil {
		<-b.block
	}
	if b.invokeErr != nil {
		return nil, b.invokeErr
	}
	if b.result != nil {
		return append(json.RawMessage(nil), b.result...), nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (b *fakeRuntimeBackend) Stop(context.Context, loader.BackendIdentity) error {
	b.stops.Add(1)
	return b.stopErr
}

func backendIdentity(id string) *runtimev1.RuntimeIdentity {
	return &runtimev1.RuntimeIdentity{
		RuntimeId: id, Version: "1.2.3", ProtocolVersion: loader.RuntimeHostProtocolVersion,
	}
}

func lifecycleRequest(id string) *runtimev1.LifecycleRequest {
	return &runtimev1.LifecycleRequest{Identity: backendIdentity(id)}
}

func invokeRequest(id string) *runtimev1.InvokeRequest {
	return &runtimev1.InvokeRequest{
		Identity: backendIdentity(id),
		Context: &runtimev1.GovernedRequestContext{
			AppId: "app.test", EchoId: "echo-1", RequestId: "request-1", TraceId: "trace-1",
			RunId: "run-1", CallId: "call-1", ProtocolVersion: "1.0",
			TargetType: "capability", CapabilityId: "test.capability", ServiceId: "test.service",
			PermissionScope: []string{"test.read"},
		},
		PayloadJson: []byte(`{}`),
	}
}

func newProtocolServer(t *testing.T, backend *fakeRuntimeBackend, maxRuntimes, maxConcurrent int) *loader.RuntimeHostProtocolServer {
	t.Helper()
	server, err := loader.NewRuntimeHostProtocolServer(loader.RuntimeHostServerConfig{
		Mode: loader.ModeHosted, Backend: backend, AllowedRuntimes: []loader.BackendIdentity{
			{ID: "hosted.server", Version: "1.2.3"},
			{ID: "hosted.one", Version: "1.2.3"},
			{ID: "hosted.two", Version: "1.2.3"},
			{ID: "hosted.invalid", Version: "1.2.3"},
			{ID: "hosted.drain", Version: "1.2.3"},
			{ID: "hosted.secret", Version: "1.2.3"},
			{ID: testPackageID, Version: "1.2.3"},
			{ID: "busy.host", Version: "1.2.3"},
		}, MaxRuntimes: maxRuntimes, MaxConcurrent: maxConcurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestRuntimeHostProtocolServerRoundTrip(t *testing.T) {
	backend := &fakeRuntimeBackend{mode: loader.ModeHosted}
	protocolServer := newProtocolServer(t, backend, 2, 2)
	dialer, _ := startRuntimeHost(t, protocolServer)
	var verifies atomic.Int32
	host := newRuntimeGRPCHost(t, loader.ModeHosted, dialer, &verifies)
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("hosted.server", loader.ModeHosted)); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Handler("hosted.server")(
		context.Background(), governedRuntimeRequest(), json.RawMessage(`{"value":1}`),
	)
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
	backend.mu.Lock()
	contexts := append([]contracts.RequestContext(nil), backend.contexts...)
	backend.mu.Unlock()
	if len(contexts) != 1 || contexts[0].AppID != "app.test" || contexts[0].CallID != "call-1" ||
		contexts[0].TargetType != "capability" || contexts[0].CapabilityID != "test.capability" ||
		contexts[0].ServiceID != "test.service" || contexts[0].ToolID != "" {
		t.Fatalf("contexts=%#v", contexts)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.starts.Load() != 1 || backend.health.Load() != 1 ||
		backend.invokes.Load() != 1 || backend.stops.Load() != 1 {
		t.Fatalf("starts=%d health=%d invokes=%d stops=%d",
			backend.starts.Load(), backend.health.Load(), backend.invokes.Load(), backend.stops.Load())
	}
}

func TestRuntimeHostProtocolServerRejectsInvalidOrderingIdentityAndCapacity(t *testing.T) {
	backend := &fakeRuntimeBackend{mode: loader.ModeHosted}
	server := newProtocolServer(t, backend, 1, 1)
	if _, err := server.Invoke(context.Background(), invokeRequest("hosted.one")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("invoke before start error=%v", err)
	}
	invalid := lifecycleRequest("hosted.one")
	invalid.Identity.ProtocolVersion = "3.0"
	if _, err := server.Start(context.Background(), invalid); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("protocol mismatch error=%v", err)
	}
	request := lifecycleRequest("hosted.one")
	request.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if _, err := server.Start(context.Background(), request); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown field error=%v", err)
	}
	if _, err := server.Start(context.Background(), lifecycleRequest("hosted.one")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Start(context.Background(), lifecycleRequest("hosted.one")); err != nil {
		t.Fatalf("idempotent start error=%v", err)
	}
	if backend.starts.Load() != 1 {
		t.Fatalf("starts=%d", backend.starts.Load())
	}
	if _, err := server.Start(context.Background(), lifecycleRequest("hosted.two")); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("capacity error=%v", err)
	}
	if _, err := server.Stop(context.Background(), lifecycleRequest("hosted.one")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Stop(context.Background(), lifecycleRequest("hosted.one")); err != nil {
		t.Fatalf("idempotent stop error=%v", err)
	}
}

func TestRuntimeHostProtocolServerRejectsInvalidContextAndMalformedBackendResult(t *testing.T) {
	backend := &fakeRuntimeBackend{mode: loader.ModeHosted, result: json.RawMessage(`not-json`)}
	server := newProtocolServer(t, backend, 1, 1)
	if _, err := server.Start(context.Background(), lifecycleRequest("hosted.invalid")); err != nil {
		t.Fatal(err)
	}
	invalidContext := invokeRequest("hosted.invalid")
	invalidContext.Context.DeadlineUnixMs = time.Now().Add(25 * time.Hour).UnixMilli()
	if _, err := server.Invoke(context.Background(), invalidContext); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("future deadline error=%v", err)
	}
	unknownContext := invokeRequest("hosted.invalid")
	unknownContext.Context.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if _, err := server.Invoke(context.Background(), unknownContext); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown context error=%v", err)
	}
	missingTarget := invokeRequest("hosted.invalid")
	missingTarget.Context.TargetType = ""
	if _, err := server.Invoke(context.Background(), missingTarget); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing target identity error=%v", err)
	}
	mismatchedTarget := invokeRequest("hosted.invalid")
	mismatchedTarget.Context.TargetType = "tool"
	mismatchedTarget.Context.ToolId = "test.capability"
	if _, err := server.Invoke(context.Background(), mismatchedTarget); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mismatched target identity error=%v", err)
	}
	if _, err := server.Invoke(context.Background(), invokeRequest("hosted.invalid")); status.Code(err) != codes.Internal {
		t.Fatalf("malformed result error=%v", err)
	}
	if _, err := server.Health(context.Background(), lifecycleRequest("hosted.invalid")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("failed runtime health error=%v", err)
	}
	if _, err := server.Stop(context.Background(), lifecycleRequest("hosted.invalid")); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeHostProtocolServerDrainsAndReturnsOnlyStableErrors(t *testing.T) {
	backend := &fakeRuntimeBackend{
		mode: loader.ModeHosted, entered: make(chan struct{}, 1), block: make(chan struct{}),
		invokeErr: loader.InvocationError{Code: "permission_denied", Retryable: true},
	}
	server := newProtocolServer(t, backend, 1, 1)
	if _, err := server.Start(context.Background(), lifecycleRequest("hosted.drain")); err != nil {
		t.Fatal(err)
	}
	invokeDone := make(chan *runtimev1.InvokeResponse, 1)
	go func() {
		response, _ := server.Invoke(context.Background(), invokeRequest("hosted.drain"))
		invokeDone <- response
	}()
	<-backend.entered
	if _, err := server.Stop(context.Background(), lifecycleRequest("hosted.drain")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stop with in-flight error=%v", err)
	}
	close(backend.block)
	response := <-invokeDone
	if response == nil || response.Success || response.ErrorCode != "permission_denied" || !response.Retryable {
		t.Fatalf("response=%#v", response)
	}
	if _, err := server.Stop(context.Background(), lifecycleRequest("hosted.drain")); err != nil {
		t.Fatal(err)
	}

	backend = &fakeRuntimeBackend{mode: loader.ModeHosted, startErr: errors.New("secret /srv/private")}
	server = newProtocolServer(t, backend, 1, 1)
	if _, err := server.Start(context.Background(), lifecycleRequest("hosted.secret")); status.Code(err) != codes.Unavailable ||
		strings.Contains(status.Convert(err).Message(), "secret") || strings.Contains(status.Convert(err).Message(), "/srv/") {
		t.Fatalf("unsafe backend error=%v", err)
	}
}

// toolRuntimeRequest 构造 tool 目标类型的受治理请求（hosted guest 按 ToolID 分发）。
func toolRuntimeRequest() contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app.test", EchoID: "echo-1", RequestID: "request-1", TraceID: "trace-1",
		RunID: "run-1", CallID: "call-1", CallDepth: 1,
		IdempotencyKey: "operation-1", ProtocolVersion: "1.0",
		TargetType: "tool", ServiceID: testPackageID, ToolID: testToolID,
		PermissionScope: []string{testInvokeScope},
	}
}

// TestRuntimeHostServesHostedArtifactOverProtocol 验证外部 Runtime Host 产品接线：
// 真实 hosted Backend（wazero 执行）经完整 RuntimeHost 协议被内核 GRPCHost 调用。
func TestRuntimeHostServesHostedArtifactOverProtocol(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("success", "success.wasm"))
	backend, err := loader.NewHostedRuntimeBackend(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, manifest loader.Manifest) ([]byte, error) {
			if manifest.ID != testPackageID {
				return nil, loader.ErrNotFound
			}
			return artifact, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	protocolServer, err := loader.NewRuntimeHostProtocolServer(loader.RuntimeHostServerConfig{
		Mode: loader.ModeHosted, Backend: backend, AllowedRuntimes: []loader.BackendIdentity{{ID: testPackageID, Version: "1.2.3"}}, MaxRuntimes: 2, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	dialer, _ := startRuntimeHost(t, protocolServer)
	host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: loader.ModeHosted, Address: "unix:/runtime-host-test.sock", Dialer: dialer,
		VerifyInstalled: func(context.Context, loader.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest(testPackageID, loader.ModeHosted)); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Handler(testPackageID)(
		context.Background(), toolRuntimeRequest(), json.RawMessage(`{"value":"hello"}`),
	)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("unmarshal result %q: %v", result, err)
	}
	if decoded["value"] != "hello" {
		t.Fatalf("result = %v, want original payload", decoded)
	}
}

// TestRuntimeHostEnforcesExecutionBudgetOverProtocol 验证外部宿主同样强制执行时间预算：
// 死循环工件经协议调用在预算内被终止，归类为稳定超时。
func TestRuntimeHostEnforcesExecutionBudgetOverProtocol(t *testing.T) {
	artifact := hostedArtifact(t, filepath.Join("busy", "busy.wasm"))
	backend, err := loader.NewHostedRuntimeBackend(loader.WasmHostConfig{
		ReadArtifact: func(_ context.Context, manifest loader.Manifest) ([]byte, error) {
			if manifest.ID != "busy.host" {
				return nil, loader.ErrNotFound
			}
			return artifact, nil
		},
		CallTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	protocolServer, err := loader.NewRuntimeHostProtocolServer(loader.RuntimeHostServerConfig{
		Mode: loader.ModeHosted, Backend: backend, AllowedRuntimes: []loader.BackendIdentity{{ID: "busy.host", Version: "1.2.3"}}, MaxRuntimes: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	dialer, _ := startRuntimeHost(t, protocolServer)
	host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: loader.ModeHosted, Address: "unix:/runtime-host-budget.sock", Dialer: dialer,
		VerifyInstalled: func(context.Context, loader.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), runtimeManifest("busy.host", loader.ModeHosted)); err != nil {
		t.Fatal(err)
	}
	// 预编译（race 下首次编译可达数秒），Invoke 只测量执行与预算终止。
	if err := manager.Warmup(context.Background(), []string{"busy.host"}, 1); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = manager.Handler("busy.host")(context.Background(), toolRuntimeRequest(), json.RawMessage(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("invoke error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("预算终止耗时 %v，死循环未被强制终止", elapsed)
	}
}
