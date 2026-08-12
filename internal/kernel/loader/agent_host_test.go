package loader_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

	"google.golang.org/grpc"
)

// fakeAgentServer 是 agent 协议的测试实现：健康就绪，Run 未实现（本测试只验证
// 加载、健康、客户端暴露与停止生命周期）。
type fakeAgentServer struct {
	agentv1.UnimplementedAgentRuntimeServer
}

func (s *fakeAgentServer) Health(context.Context, *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	return &agentv1.HealthResponse{
		Ready: true, Provider: "test",
		SupportedProtocolVersions: []string{agentprotocol.Version},
	}, nil
}

// TestAgentHostLoadsConnectedAgent 验证内置 agent 以 isolated Runtime 纳管：
// 连接模式（内核不拥有进程）下 Load → 加载期健康检查 → 客户端暴露 → Invoke 拒绝
// （agent 是 Capability 消费者）→ Stop 关闭连接。
func TestAgentHostLoadsConnectedAgent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(server, &fakeAgentServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	host, err := loader.NewAgentHost(loader.AgentHostConfig{
		Resolve: func(context.Context) (loader.AgentSpec, error) {
			return loader.AgentSpec{Address: listener.Addr().String()}, nil
		},
		Spawn:          false,
		Model:          "test-model",
		DialTimeout:    5 * time.Second,
		StopGrace:      time.Second,
		TerminateGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeIsolated: host})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(host.Manifest()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Warmup(context.Background(), []string{loader.AgentRuntimeID}, 1); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	lease, err := manager.Acquire(context.Background(), loader.AgentRuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := lease.Runtime()
	if _, ok := runtime.(loader.AgentClientProvider); !ok {
		t.Fatal("agent runtime does not expose an agent client")
	}
	if _, err := runtime.Describe(context.Background()); err != nil {
		t.Fatalf("describe: %v", err)
	}
	// agent 不服务请求/响应能力调用。
	if _, err := runtime.Invoke(context.Background(), hostedTestRequest("agent.call"), json.RawMessage(`{}`)); !errors.Is(err, loader.ErrUnsupportedMode) {
		t.Fatalf("invoke error = %v, want ErrUnsupportedMode", err)
	}
	// 释放租约后再关闭 manager：Shutdown 等待 inFlight 归零。
	lease.Release()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

// TestAgentHostRejectsInvalidConfiguration 验证非法配置 fail-closed。
func TestAgentHostRejectsInvalidConfiguration(t *testing.T) {
	if _, err := loader.NewAgentHost(loader.AgentHostConfig{}); err == nil {
		t.Fatal("nil resolve must fail")
	}
	if _, err := loader.NewAgentHost(loader.AgentHostConfig{
		Resolve: func(context.Context) (loader.AgentSpec, error) {
			return loader.AgentSpec{}, nil
		},
	}); err == nil {
		t.Fatal("missing model must fail")
	}
}
