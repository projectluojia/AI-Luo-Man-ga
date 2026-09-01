// Package executor 是 Go Core 使用的执行者协议入口。
//
// 协议边界由 contracts/pkg/executor 唯一实现；本包只把公共契约接入内核，
// 不复制校验规则，也不包含任何具体执行者、模型或 Provider 逻辑。
package executor

import (
	contractsexecutor "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executor"
)

const (
	Version                   = contractsexecutor.Version
	MaxGRPCMessageBytes       = contractsexecutor.MaxGRPCMessageBytes
	MaxFrameBytes             = contractsexecutor.MaxFrameBytes
	MaxInputPayloadBytes      = contractsexecutor.MaxInputPayloadBytes
	MaxContextPayloadBytes    = contractsexecutor.MaxContextPayloadBytes
	MaxExecutorConfigBytes    = contractsexecutor.MaxExecutorConfigBytes
	MaxCapabilities           = contractsexecutor.MaxCapabilities
	MaxCapabilitySchemaBytes  = contractsexecutor.MaxCapabilitySchemaBytes
	MaxCapabilityPayloadBytes = contractsexecutor.MaxCapabilityPayloadBytes
	MaxResultPayloadBytes     = contractsexecutor.MaxResultPayloadBytes
	MaxOutputDeltaBytes       = contractsexecutor.MaxOutputDeltaBytes
	MaxOutputPayloadBytes     = contractsexecutor.MaxOutputPayloadBytes
	MaxFailureMessageBytes    = contractsexecutor.MaxFailureMessageBytes
	MaxIdentifierBytes        = contractsexecutor.MaxIdentifierBytes
	MaxDescriptionBytes       = contractsexecutor.MaxDescriptionBytes
	MaxNameBytes              = contractsexecutor.MaxNameBytes
	MaxProtocolSteps          = contractsexecutor.MaxProtocolSteps
	MaxCapabilityCalls        = contractsexecutor.MaxCapabilityCalls
	MaxProtocolVersions       = contractsexecutor.MaxProtocolVersions
	MaxStatusCodeBytes        = contractsexecutor.MaxStatusCodeBytes
	MaxExecutionUnits         = contractsexecutor.MaxExecutionUnits
	MaxCostMicrousd           = contractsexecutor.MaxCostMicrousd
	MaxRetries                = contractsexecutor.MaxRetries
)

var (
	ErrVersionMismatch = contractsexecutor.ErrVersionMismatch
	ErrInvalidFrame    = contractsexecutor.ErrInvalidFrame
	ErrSequence        = contractsexecutor.ErrSequence
	ErrFrameTooLarge   = contractsexecutor.ErrFrameTooLarge
	ErrUnexpectedFrame = contractsexecutor.ErrUnexpectedFrame
	ErrDuplicateCall   = contractsexecutor.ErrDuplicateCall
)

func Supports(versions []string) bool {
	return contractsexecutor.Supports(versions)
}

func ValidateHealthRequest(request *HealthRequest) error {
	return contractsexecutor.ValidateHealthRequest(request)
}

func ValidateHealthResponse(response *HealthResponse) error {
	return contractsexecutor.ValidateHealthResponse(response)
}

func ValidateStartFrame(frame *Frame) error {
	return contractsexecutor.ValidateStartFrame(frame)
}

func ValidateResourceUsage(usage *ResourceUsage, previousUnits, previousCost uint64, previousRetries uint32, maxUnits, maxCost uint64) error {
	return contractsexecutor.ValidateResourceUsage(usage, previousUnits, previousCost, previousRetries, maxUnits, maxCost)
}

func ValidateInboundEnvelope(frame *Frame, echoID, runID string, expectedSequence uint64) error {
	return contractsexecutor.ValidateInboundEnvelope(frame, echoID, runID, expectedSequence)
}

func ValidateRunAccepted(frame *Frame) error {
	return contractsexecutor.ValidateRunAccepted(frame)
}

func ValidateCapabilityCall(call *CapabilityCall) error {
	return contractsexecutor.ValidateCapabilityCall(call)
}

func ValidateOutputDelta(delta *OutputDelta) error {
	return contractsexecutor.ValidateOutputDelta(delta)
}

func ValidateFinalResult(result *FinalResult) error {
	return contractsexecutor.ValidateFinalResult(result)
}

func ValidateRunFailure(failure *RunFailure) error {
	return contractsexecutor.ValidateRunFailure(failure)
}

func ValidateCapabilityResultFrame(frame *Frame, echoID, runID string, expectedSequence uint64) error {
	return contractsexecutor.ValidateCapabilityResultFrame(frame, echoID, runID, expectedSequence)
}
