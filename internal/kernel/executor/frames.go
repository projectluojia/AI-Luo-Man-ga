// frames.go 把执行者线协议的生成类型以中性名称暴露给内核逻辑：内核代码只
// 依赖本契约包，不直接引用生成包（executorv1）。线协议升级时只有本包需要调整。
package executor

import (
	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
)

type (
	Frame                  = executorv1.ExecutorFrame
	Frame_StartRun         = executorv1.ExecutorFrame_StartRun
	Frame_RunAccepted      = executorv1.ExecutorFrame_RunAccepted
	Frame_CapabilityCall   = executorv1.ExecutorFrame_CapabilityCall
	Frame_CapabilityResult = executorv1.ExecutorFrame_CapabilityResult
	Frame_OutputDelta      = executorv1.ExecutorFrame_OutputDelta
	Frame_FinalResult      = executorv1.ExecutorFrame_FinalResult
	Frame_RunFailure       = executorv1.ExecutorFrame_RunFailure
	Frame_ResourceUsage    = executorv1.ExecutorFrame_ResourceUsage

	StartRun         = executorv1.StartRun
	Payload          = executorv1.Payload
	Capability       = executorv1.Capability
	CapabilityCall   = executorv1.CapabilityCall
	CapabilityResult = executorv1.CapabilityResult
	OutputDelta      = executorv1.OutputDelta
	FinalResult      = executorv1.FinalResult
	RunFailure       = executorv1.RunFailure
	ResourceUsage    = executorv1.ResourceUsage
	HealthRequest    = executorv1.HealthRequest
	HealthResponse   = executorv1.HealthResponse
)
