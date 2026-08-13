package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

	"google.golang.org/grpc"
)

// Runtime 是内置 AI 执行者的执行单元：监督 agent 进程，向内核暴露 agent 协议
// 客户端。生命周期（优雅停止 → 宽限等待 → 强制清理）复用 loader.Process 原语，
// agent 没有 RuntimeHost 协议 Stop，也不产生 Unix socket。
type Runtime struct {
	id, version    string
	process        *loader.Process // Spawn=false 时为 nil
	connection     *grpc.ClientConn
	client         agentv1.AgentRuntimeClient
	model          string
	stopGrace      time.Duration
	terminateGrace time.Duration

	stopOnce sync.Once
	stopErr  error
}

func (r *Runtime) Describe(context.Context) (loader.Description, error) {
	return loader.Description{ID: r.id, Version: r.version, Mode: loader.ModeIsolated}, nil
}

func (r *Runtime) Start(context.Context) error { return nil }

func (r *Runtime) Health(ctx context.Context) error {
	if r.process != nil && r.process.Exited() {
		return loader.ErrUnavailable
	}
	return health.AgentChecker{Client: r.client, Model: r.model}.Ping(ctx)
}

// Invoke 不服务请求/响应调用：agent 是 Capability 消费者，不注册任何能力。
func (r *Runtime) Invoke(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return nil, loader.ErrUnsupportedMode
}

// Stop 优雅停止 agent：Spawn 模式终止进程等待退出，超时强制清理；连接模式只关闭连接。
func (r *Runtime) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { r.stopErr = r.stop(ctx) })
	return r.stopErr
}

func (r *Runtime) stop(ctx context.Context) error {
	if r.process == nil {
		return r.closeTransport()
	}
	if err := r.process.Reap(ctx, r.stopGrace, r.terminateGrace); err != nil {
		return errors.Join(loader.ErrProcessCleanup, err)
	}
	return r.closeTransport()
}

func (r *Runtime) closeTransport() error {
	if r.connection == nil {
		return nil
	}
	connection := r.connection
	r.connection = nil
	return connection.Close()
}

// AgentClient 返回 agent 协议客户端，供内核 Orchestrator 驱动模型循环。
func (r *Runtime) AgentClient() agentv1.AgentRuntimeClient {
	return r.client
}

// Done 返回 agent 进程退出通知（连接模式永不触发）。
func (r *Runtime) Done() <-chan struct{} {
	if r.process == nil {
		return nil
	}
	return r.process.Done()
}

// Err 返回 agent 进程退出错误。
func (r *Runtime) Err() error {
	if r.process == nil {
		return nil
	}
	return r.process.Err()
}

var (
	_ loader.Runtime                 = (*Runtime)(nil)
	_ agentprotocol.ClientProvider   = (*Runtime)(nil)
	_ agentprotocol.ProcessLifecycle = (*Runtime)(nil)
)
