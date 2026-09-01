package loader_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

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
	host, err := loader.NewProcessHost(loader.ProcessHostConfig{
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
	lease, err := manager.Executor(t.Context(), manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := lease.Runtime()
	clientProvider, ok := runtime.(executor.ClientProvider)
	if !ok || clientProvider.Client() == nil {
		t.Fatal("executor runtime does not expose a client")
	}
	if _, ok := runtime.(loader.Invoker); ok {
		t.Fatal("executor runtime must not expose capability invocation")
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

// TestProcessHostRequiresSpawnForCapabilityRole 验证 capability 组件必须由本
// 宿主启动：连接模式只服务 executor 角色。
func TestProcessHostRequiresSpawnForCapabilityRole(t *testing.T) {
	host, err := loader.NewProcessHost(loader.ProcessHostConfig{
		Resolve: func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error) {
			return packagecontract.ProcessSpec{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = host.Verify(t.Context(), loader.Manifest{
		ID: "capability.test", Version: "1.0.0", Mode: loader.ModeIsolated,
		Role: loader.RoleProvider, LockedDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if !errors.Is(err, loader.ErrInvalidProcessSpec) {
		t.Fatalf("verify error = %v, want ErrInvalidProcessSpec", err)
	}
}
