// Package executor 定义内核执行者契约：任何能驱动受治理 Run 会话的实现
// （规划器、工作流引擎、批处理器或远程服务等）都以本契约为准。内核只认识
// 本包，不认识具体实现或语言；executor.v1 只是线协议的生成产物，本包是
// 它在内核侧的唯一边界。
package executor

import (
	"context"

	"google.golang.org/grpc"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
)

// RunStream 是执行者会话的双向流：内核发送 StartRun、取消和能力结果，
// 执行者发送 RunAccepted / CapabilityCall / 流式回复 / 终态。
type RunStream = executorv1.ExecutorRuntime_RunClient

// Client 是内核消费执行者所需的协议客户端面；executorv1 生成的客户端天然满足。
type Client interface {
	Run(ctx context.Context, opts ...grpc.CallOption) (RunStream, error)
	Health(ctx context.Context, in *executorv1.HealthRequest, opts ...grpc.CallOption) (*executorv1.HealthResponse, error)
}

// NewClient 将 gRPC 连接收敛为内核执行者契约，其他内核包不直接依赖生成客户端。
func NewClient(conn grpc.ClientConnInterface) Client {
	return executorv1.NewExecutorRuntimeClient(conn)
}

// ClientProvider 由向内核提供执行者客户端的运行时实现；任何以 embedded、
// hosted 或 isolated 形态注册、实现本契约的运行时都可作为内核执行者。
type ClientProvider interface {
	Client() Client
}

// ProcessLifecycle 由持有受监督进程的执行者运行时实现（连接模式不拥有进程时
// 不实现该契约）：进程异常退出时内核按 fail-closed 策略停止。
type ProcessLifecycle interface {
	Done() <-chan struct{}
	Err() error
}
