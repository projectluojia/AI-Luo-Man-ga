// frames.go 把执行者线协议的生成类型以中性名称暴露给内核逻辑：内核代码只
// 依赖本契约包，不直接引用生成包（executorv1）。线协议升级时只有本包需要调整。
package executor

import (
	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/executorv1"
)

type (
	Frame                  = executorv1.ExecutorFrame
	Frame_StartRun         = executorv1.ExecutorFrame_StartRun
	Frame_RunAccepted      = executorv1.ExecutorFrame_RunAccepted
	Frame_CapabilityCall   = executorv1.ExecutorFrame_CapabilityCall
	Frame_CapabilityResult = executorv1.ExecutorFrame_CapabilityResult
	Frame_ReplyDelta       = executorv1.ExecutorFrame_ReplyDelta
	Frame_FinalMessage     = executorv1.ExecutorFrame_FinalMessage
	Frame_RunFailure       = executorv1.ExecutorFrame_RunFailure
	Frame_RunUsage         = executorv1.ExecutorFrame_RunUsage

	StartRun         = executorv1.StartRun
	Capability       = executorv1.Capability
	CapabilityCall   = executorv1.CapabilityCall
	CapabilityResult = executorv1.CapabilityResult
	ConfirmationInfo = executorv1.ConfirmationInfo
	ReplyDelta       = executorv1.ReplyDelta
	FinalMessage     = executorv1.FinalMessage
	RunFailure       = executorv1.RunFailure
	RunUsage         = executorv1.RunUsage
	HealthRequest    = executorv1.HealthRequest
	HealthResponse   = executorv1.HealthResponse
)
