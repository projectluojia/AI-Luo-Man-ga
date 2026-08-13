package agentprotocol

import (
	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
)

// ClientProvider 由向内核 Orchestrator 提供 agent 协议客户端的运行时实现。
// 内核只认识该契约，不依赖任何具体语言或实现的 agent：任何以 isolated /
// hosted / remote 形态注册、实现本契约的运行时都可作为内核的 AI 执行者。
type ClientProvider interface {
	AgentClient() agentv1.AgentRuntimeClient
}

// ProcessLifecycle 由持有受监督进程的 agent 运行时实现（连接模式不拥有进程时
// 不实现该契约）：进程异常退出时内核按 fail-closed 策略停止。
type ProcessLifecycle interface {
	Done() <-chan struct{}
	Err() error
}
