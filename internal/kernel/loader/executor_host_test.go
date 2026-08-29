package loader_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"

	"google.golang.org/grpc"
)

type executorHealthServer struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

func (executorHealthServer) Health(context.Context, *executorv1.HealthRequest) (*executorv1.HealthResponse, error) {
	return &executorv1.HealthResponse{
		Ready: true, Provider: "test", SupportedProtocolVersions: []string{executor.Version},
	}, nil
}

func TestExecutorHostLoadsConnectedRuntime(t *testing.T) {
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
	host, err := loader.NewExecutorHost(loader.ExecutorHostConfig{
		Manifest: manifest,
		Resolve: func(context.Context) (packmgr.ProcessSpec, error) {
			return packmgr.ProcessSpec{Address: listener.Addr().String()}, nil
		},
		Model: "test-model", DialTimeout: 5 * time.Second,
		StopGrace: time.Second, TerminateGrace: time.Second,
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
	lease, err := manager.Executor(t.Context())
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
	if _, err := lease.Invoke(t.Context(), contracts.RequestContext{ToolID: "executor.call"}, []byte(`{}`)); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("invoke error = %v, want ErrUnavailable", err)
	}
	lease.Release()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorHostRejectsMissingExecutorManifest(t *testing.T) {
	_, err := loader.NewExecutorHost(loader.ExecutorHostConfig{
		Manifest: loader.Manifest{ID: "capability.test", Version: "1.0.0", Mode: loader.ModeHosted, Role: loader.RoleCapability, LockedDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		Resolve:  func(context.Context) (packmgr.ProcessSpec, error) { return packmgr.ProcessSpec{}, nil },
		Model:    "test-model",
	})
	if !errors.Is(err, loader.ErrInvalidManifest) {
		t.Fatalf("error = %v, want ErrInvalidManifest", err)
	}
}
