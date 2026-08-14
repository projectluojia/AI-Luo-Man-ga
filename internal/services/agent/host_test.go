package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/agent"

	"google.golang.org/grpc"
)

// fakeAgentServer 是执行者协议的测试实现：健康就绪，Run 未实现（本测试只验证
// 加载、健康、客户端暴露与停止生命周期）。
type fakeAgentServer struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

func (s *fakeAgentServer) Health(context.Context, *executorv1.HealthRequest) (*executorv1.HealthResponse, error) {
	return &executorv1.HealthResponse{
		Ready: true, Provider: "test",
		SupportedProtocolVersions: []string{executor.Version},
	}, nil
}

// TestHostLoadsConnectedAgent 验证内置执行者以 isolated Runtime 纳管：
// 连接模式（内核不拥有进程）下 Load → 加载期健康检查 → 执行者客户端暴露 →
// 不实现能力 Invoker（执行者角色没有能力执行面）→ Stop 关闭连接。
func TestHostLoadsConnectedAgent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(server, &fakeAgentServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	host, err := agent.NewHost(agent.Config{
		Resolve: func(context.Context) (agent.Spec, error) {
			return agent.Spec{Address: listener.Addr().String()}, nil
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
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(context.Background(), host.Manifest()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Warmup(context.Background(), []string{agent.RuntimeID}, 1); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	lease, err := manager.Acquire(context.Background(), agent.RuntimeID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := lease.Runtime()
	clientProvider, ok := runtime.(executor.ClientProvider)
	if !ok {
		t.Fatal("executor runtime does not expose an executor client")
	}
	if clientProvider.Client() == nil {
		t.Fatal("executor client is nil")
	}
	if _, ok := runtime.(loader.Invoker); ok {
		t.Fatal("executor runtime must not implement the capability Invoker")
	}
	if _, err := runtime.Describe(context.Background()); err != nil {
		t.Fatalf("describe: %v", err)
	}
	// 执行者租约没有能力执行面：直接 Invoke 是误用，防御性拒绝。
	if _, err := lease.Invoke(context.Background(), testRequest("agent.call"), json.RawMessage(`{}`)); !errors.Is(err, loader.ErrUnavailable) {
		t.Fatalf("invoke error = %v, want ErrUnavailable", err)
	}
	// 释放租约后再关闭 manager：Shutdown 等待 inFlight 归零。
	lease.Release()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

// TestHostRejectsInvalidConfiguration 验证非法配置 fail-closed。
func TestHostRejectsInvalidConfiguration(t *testing.T) {
	if _, err := agent.NewHost(agent.Config{}); err == nil {
		t.Fatal("nil resolve must fail")
	}
	if _, err := agent.NewHost(agent.Config{
		Resolve: func(context.Context) (agent.Spec, error) {
			return agent.Spec{}, nil
		},
	}); err == nil {
		t.Fatal("missing model must fail")
	}
}

func testRequest(toolID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app.campus", EchoID: "echo-1", RequestID: "request-1",
		ToolID: toolID,
	}
}

// TestRecordRegistersViaInstalledPath 验证插件注册路径：agent.Record(host) 携带
// 宿主清单（单一来源），经 RegisterInstalled 以与 campus/installed 相同的机制
// 注册，预热清单由 Pinned() 从清单声明推导。运行时不加载（连接模式拨号会失败），
// 本测试只验证注册与 pin 推导。
func TestRecordRegistersViaInstalledPath(t *testing.T) {
	host, err := agent.NewHost(agent.Config{
		Resolve: func(context.Context) (agent.Spec, error) {
			return agent.Spec{Address: "127.0.0.1:1"}, nil
		},
		Spawn: false, Model: "test-model",
		DialTimeout: 5 * time.Second, StopGrace: time.Second, TerminateGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := agent.Record(host)
	if !reflect.DeepEqual(record.Runtime, host.Manifest()) {
		t.Fatalf("record runtime = %#v, want host manifest %#v", record.Runtime, host.Manifest())
	}
	if record.Runtime.ID != agent.RuntimeID || record.Runtime.Mode != loader.ModeIsolated ||
		record.Runtime.Role != loader.RoleExecutor || !record.Runtime.Pin {
		t.Fatalf("record runtime = %#v", record.Runtime)
	}
	manager, err := loader.New(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.RegisterInstalled(context.Background(), manager, registry.New(), []loader.InstalledRecord{record}); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	pinned := manager.Pinned()
	if len(pinned) != 1 || pinned[0] != agent.RuntimeID {
		t.Fatalf("pinned = %v, want [%s]", pinned, agent.RuntimeID)
	}
}
