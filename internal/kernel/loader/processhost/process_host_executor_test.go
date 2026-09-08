package processhost_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader/processhost"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtimehost"

	"google.golang.org/grpc"
)

type executorHealthServer struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

func (executorHealthServer) Health(context.Context, *executorv1.HealthRequest) (*executorv1.HealthResponse, error) {
	return &executorv1.HealthResponse{
		Ready: true, SupportedProtocolVersions: []string{executor.Version},
	}, nil
}

// TestProcessHostServesExecutorOverConnectMode 验证统一进程宿主的 executor 面：
// 连接模式（Spawn=false）只拨号外部已启动的 executor.v1 运行时，并按角色暴露
// 会话客户端（不暴露能力调用面）。
func TestProcessHostServesExecutorOverConnectMode(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(server, executorHealthServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	manifest := loader.Manifest{
		ID: "executor.test", Version: "1.0.0", Mode: loader.ModeIsolated,
		Role: loader.RoleExecutor, LockedDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	host, err := processhost.NewProcessHost(processhost.ProcessHostConfig{
		Resolve: func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) {
			return packagecontract.ProcessSpec{Address: listener.Addr().String()}, nil
		},
		DialTimeout: 5 * time.Second,
		StopGrace:   time.Second, TerminateGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := manager.Warmup(t.Context(), []string{manifest.ID}, 1); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	lease, err := manager.Acquire(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Faces().Client == nil {
		t.Fatal("executor lease does not expose a client")
	}
	if lease.Faces().Invoker != nil {
		t.Fatal("executor lease must not expose capability invocation")
	}
	if _, err := lease.Invoke(t.Context(), contracts.RequestContext{CapabilityID: "executor.call"}, []byte(`{}`)); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("invoke error = %v, want ErrUnavailable", err)
	}
	lease.Release()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

// fakeCapabilityHostServer 是连接模式 capability 测试的最小 runtime_host
// 实现：生命周期调用返回 ready，Invoke 返回固定成功载荷。
type fakeCapabilityHostServer struct {
	runtimev1.UnimplementedRuntimeHostServer
}

func (fakeCapabilityHostServer) Describe(_ context.Context, request *runtimev1.DescribeRequest) (*runtimev1.RuntimeDescription, error) {
	return &runtimev1.RuntimeDescription{
		RuntimeId: request.Identity.RuntimeId, Version: request.Identity.Version,
		Mode: loader.ModeIsolated, SupportedProtocolVersions: []string{loader.RuntimeHostProtocolVersion},
	}, nil
}

func (fakeCapabilityHostServer) Start(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return &runtimev1.LifecycleResponse{Identity: request.Identity, Ready: true, StatusCode: "ready"}, nil
}

func (fakeCapabilityHostServer) Health(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return &runtimev1.LifecycleResponse{Identity: request.Identity, Ready: true, StatusCode: "ready"}, nil
}

func (fakeCapabilityHostServer) Invoke(_ context.Context, request *runtimev1.InvokeRequest) (*runtimev1.InvokeResponse, error) {
	return &runtimev1.InvokeResponse{
		Identity: request.Identity, Success: true, PayloadJson: []byte(`{"ok":true}`),
	}, nil
}

func (fakeCapabilityHostServer) Stop(_ context.Context, request *runtimev1.LifecycleRequest) (*runtimev1.LifecycleResponse, error) {
	return &runtimev1.LifecycleResponse{Identity: request.Identity, Ready: false, StatusCode: "stopped"}, nil
}

// TestProcessHostServesCapabilityOverConnectMode 验证连接模式对两种角色统一：
// capability 组件也可以不由本宿主启动，宿主只拨号外部已启动的 runtime_host
// 服务并装载能力执行面。
func TestProcessHostServesCapabilityOverConnectMode(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(runtimehost.RuntimeHostGRPCServerOptions()...)
	runtimev1.RegisterRuntimeHostServer(server, fakeCapabilityHostServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	manifest := loader.Manifest{
		ID: "capability.connect", Version: "1.0.0", Mode: loader.ModeIsolated,
		Role: loader.RoleProvider, LockedDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	host, err := processhost.NewProcessHost(processhost.ProcessHostConfig{
		Resolve: func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) {
			return packagecontract.ProcessSpec{Address: listener.Addr().String()}, nil
		},
		DialTimeout: 5 * time.Second,
		StopGrace:   time.Second, TerminateGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	if err := manager.Warmup(t.Context(), []string{manifest.ID}, 1); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	lease, err := manager.Acquire(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Faces().Invoker == nil {
		t.Fatal("capability lease does not expose an invoker")
	}
	if _, err := lease.Invoke(t.Context(), contracts.RequestContext{
		AppID: "app.test", EchoID: "echo.test", RequestID: "request.test", CapabilityID: "capability.connect.call",
	}, []byte(`{}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	lease.Release()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}
