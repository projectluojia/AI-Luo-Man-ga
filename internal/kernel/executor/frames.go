// frames.go 把执行者线协议的生成类型以中性名称暴露给内核逻辑：内核代码只
// 依赖本契约包，不直接引用生成包（agentv1）。线协议升级时只有本包需要调整。
package executor

import (
	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
)

type (
	Frame                  = agentv1.AgentFrame
	Frame_StartRun         = agentv1.AgentFrame_StartRun
	Frame_RunAccepted      = agentv1.AgentFrame_RunAccepted
	Frame_CapabilityCall   = agentv1.AgentFrame_CapabilityCall
	Frame_CapabilityResult = agentv1.AgentFrame_CapabilityResult
	Frame_ReplyDelta       = agentv1.AgentFrame_ReplyDelta
	Frame_FinalMessage     = agentv1.AgentFrame_FinalMessage
	Frame_RunFailure       = agentv1.AgentFrame_RunFailure
	Frame_RunUsage         = agentv1.AgentFrame_RunUsage

	StartRun         = agentv1.StartRun
	Capability       = agentv1.Capability
	CapabilityCall   = agentv1.CapabilityCall
	CapabilityResult = agentv1.CapabilityResult
	ReplyDelta       = agentv1.ReplyDelta
	FinalMessage     = agentv1.FinalMessage
	RunFailure       = agentv1.RunFailure
	RunUsage         = agentv1.RunUsage
	HealthRequest    = agentv1.HealthRequest
	HealthResponse   = agentv1.HealthResponse
)
