// Package executor 提供与 executor.v1 配套的中性帧校验和资源边界。
//
// 这里不包含任何 Executor 的算法、模型或 Provider 逻辑；外部执行者包和
// Go Core 使用同一份协议安全规则。
package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
)

const (
	Version = "4.0"

	MaxGRPCMessageBytes       = 512 << 10
	MaxFrameBytes             = 300 << 10
	MaxInputPayloadBytes      = 16 << 10
	MaxContextPayloadBytes    = 64 << 10
	MaxExecutorConfigBytes    = 64 << 10
	MaxCapabilities           = 64
	MaxCapabilitySchemaBytes  = 64 << 10
	MaxCapabilityPayloadBytes = 64 << 10
	MaxResultPayloadBytes     = 256 << 10
	MaxOutputDeltaBytes       = 16 << 10
	MaxOutputPayloadBytes     = 256 << 10
	MaxFailureMessageBytes    = 1024
	MaxIdentifierBytes        = 128
	MaxDescriptionBytes       = 4096
	MaxNameBytes              = 256
	MaxProtocolSteps          = 64
	MaxCapabilityCalls        = 256
	MaxProtocolVersions       = 16
	MaxStatusCodeBytes        = 64
	MaxExecutionUnits         = 1_000_000_000
	MaxCostMicrousd           = 1_000_000_000_000_000
	MaxRetries                = 320
)

var (
	ErrVersionMismatch = errors.New("executor protocol version mismatch")
	ErrInvalidFrame    = errors.New("invalid executor protocol frame")
	ErrSequence        = errors.New("executor protocol frame sequence violation")
	ErrFrameTooLarge   = errors.New("executor protocol frame exceeds size limit")
	ErrUnexpectedFrame = errors.New("unexpected executor protocol frame type")
	ErrDuplicateCall   = errors.New("duplicate executor capability call id")
)

var (
	codePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	tracePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanPattern  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func Supports(versions []string) bool {
	for _, version := range versions {
		if version == Version {
			return true
		}
	}
	return false
}

func ValidateHealthRequest(request *executorv1.HealthRequest) error {
	if request == nil || hasUnknownFields(request.ProtoReflect()) || len(request.AcceptedProtocolVersions) > MaxProtocolVersions {
		return ErrInvalidFrame
	}
	for _, version := range request.AcceptedProtocolVersions {
		if !validText(version, 1, MaxIdentifierBytes) {
			return ErrInvalidFrame
		}
	}
	return nil
}

func ValidateHealthResponse(response *executorv1.HealthResponse) error {
	if response == nil || hasUnknownFields(response.ProtoReflect()) || len(response.SupportedProtocolVersions) > MaxProtocolVersions ||
		(response.StatusCode != "" && (!validToken(response.StatusCode, MaxStatusCodeBytes) || !codePattern.MatchString(response.StatusCode))) {
		return ErrInvalidFrame
	}
	for _, version := range response.SupportedProtocolVersions {
		if !validText(version, 1, MaxIdentifierBytes) {
			return ErrInvalidFrame
		}
	}
	return nil
}

func ValidateStartFrame(frame *executorv1.ExecutorFrame) error {
	if frame == nil {
		return ErrInvalidFrame
	}
	if err := validateEnvelope(frame, frame.GetEchoId(), frame.GetRunId(), 1); err != nil {
		return err
	}
	start := frame.GetStartRun()
	if start == nil {
		return ErrUnexpectedFrame
	}
	if start.ProtocolVersion != Version {
		return ErrVersionMismatch
	}
	if !validToken(start.AppId, MaxIdentifierBytes) ||
		!validPayload(start.InputPayload, true, MaxInputPayloadBytes) ||
		!validPayload(start.ContextPayload, false, MaxContextPayloadBytes) ||
		!validPayload(start.ExecutorConfig, false, MaxExecutorConfigBytes) ||
		start.MaxSteps == 0 || start.MaxSteps > MaxProtocolSteps ||
		start.MaxCapabilityCalls == 0 || start.MaxCapabilityCalls > MaxCapabilityCalls ||
		start.MaxExecutionUnits == 0 || start.MaxExecutionUnits > MaxExecutionUnits ||
		start.MaxOutputBytes == 0 || start.MaxOutputBytes > MaxOutputPayloadBytes ||
		start.MaxCostMicrousd > MaxCostMicrousd ||
		(start.ParentRunId != "" && (!validToken(start.ParentRunId, MaxIdentifierBytes) || start.ParentRunId == frame.GetRunId())) ||
		(start.TraceId != "" && (!tracePattern.MatchString(start.TraceId) || start.TraceId == "00000000000000000000000000000000")) ||
		(start.ParentSpanId != "" && (!spanPattern.MatchString(start.ParentSpanId) || start.ParentSpanId == "0000000000000000")) ||
		len(start.Capabilities) > MaxCapabilities {
		return ErrInvalidFrame
	}
	seen := make(map[string]struct{}, len(start.Capabilities))
	for _, capability := range start.Capabilities {
		if capability == nil ||
			!validToken(capability.Id, MaxIdentifierBytes) ||
			!validText(capability.Version, 1, MaxIdentifierBytes) ||
			!validText(capability.Name, 1, MaxNameBytes) ||
			!validText(capability.Description, 1, MaxDescriptionBytes) ||
			!validText(capability.InputSchemaJson, 2, MaxCapabilitySchemaBytes) ||
			!json.Valid([]byte(capability.InputSchemaJson)) {
			return ErrInvalidFrame
		}
		if _, exists := seen[capability.Id]; exists {
			return ErrInvalidFrame
		}
		seen[capability.Id] = struct{}{}
	}
	return nil
}

func ValidateResourceUsage(
	usage *executorv1.ResourceUsage,
	previousUnits, previousCost uint64,
	previousRetries uint32,
	maxUnits, maxCost uint64,
) error {
	if usage == nil || usage.ExecutionUnits == 0 || usage.ExecutionUnits > MaxExecutionUnits ||
		usage.CostMicrousd > MaxCostMicrousd || usage.ExecutionUnits < previousUnits ||
		usage.CostMicrousd < previousCost || usage.Retries < previousRetries ||
		usage.Retries > MaxRetries || usage.ExecutionUnits > maxUnits ||
		(maxCost > 0 && usage.CostMicrousd > maxCost) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateInboundEnvelope(frame *executorv1.ExecutorFrame, echoID, runID string, expectedSequence uint64) error {
	return validateEnvelope(frame, echoID, runID, expectedSequence)
}

func ValidateRunAccepted(frame *executorv1.ExecutorFrame) error {
	accepted := frame.GetRunAccepted()
	if accepted == nil {
		return ErrUnexpectedFrame
	}
	if accepted.ProtocolVersion != Version {
		return ErrVersionMismatch
	}
	return nil
}

func ValidateCapabilityCall(call *executorv1.CapabilityCall) error {
	if call == nil || !validToken(call.CallId, MaxIdentifierBytes) ||
		!validToken(call.CapabilityId, MaxIdentifierBytes) || len(call.PayloadJson) == 0 ||
		len(call.PayloadJson) > MaxCapabilityPayloadBytes || !json.Valid(call.PayloadJson) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateOutputDelta(delta *executorv1.OutputDelta) error {
	if delta == nil || !validPayload(delta.Payload, true, MaxOutputDeltaBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateFinalResult(result *executorv1.FinalResult) error {
	if result == nil || !validPayload(result.Payload, true, MaxOutputPayloadBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateRunFailure(failure *executorv1.RunFailure) error {
	if failure == nil || !validToken(failure.Code, 64) || !codePattern.MatchString(failure.Code) ||
		!validText(failure.Message, 1, MaxFailureMessageBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateCapabilityResultFrame(frame *executorv1.ExecutorFrame, echoID, runID string, expectedSequence uint64) error {
	if err := validateEnvelope(frame, echoID, runID, expectedSequence); err != nil {
		return err
	}
	result := frame.GetCapabilityResult()
	if result == nil || !validToken(result.CallId, MaxIdentifierBytes) ||
		!validToken(result.CapabilityId, MaxIdentifierBytes) {
		return ErrInvalidFrame
	}
	if result.Success {
		if len(result.PayloadJson) == 0 || len(result.PayloadJson) > MaxResultPayloadBytes ||
			!json.Valid(result.PayloadJson) || result.ErrorCode != "" || result.ErrorMessage != "" {
			return ErrInvalidFrame
		}
		return nil
	}
	if len(result.PayloadJson) != 0 || !validToken(result.ErrorCode, 64) ||
		!codePattern.MatchString(result.ErrorCode) || !validText(result.ErrorMessage, 1, MaxFailureMessageBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func validateEnvelope(frame *executorv1.ExecutorFrame, echoID, runID string, expectedSequence uint64) error {
	if frame == nil {
		return ErrInvalidFrame
	}
	if proto.Size(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if hasUnknownFields(frame.ProtoReflect()) {
		return ErrInvalidFrame
	}
	if !validToken(frame.EchoId, MaxIdentifierBytes) || !validToken(frame.RunId, MaxIdentifierBytes) ||
		frame.EchoId != echoID || frame.RunId != runID || frame.Body == nil {
		return ErrInvalidFrame
	}
	if expectedSequence == 0 || frame.Sequence != expectedSequence {
		return fmt.Errorf("%w: got=%d want=%d", ErrSequence, frame.Sequence, expectedSequence)
	}
	return nil
}

func validPayload(payload *executorv1.Payload, required bool, maximum int) bool {
	if payload == nil {
		return !required
	}
	if len(payload.Data) == 0 {
		return !required && payload.ContentType == ""
	}
	if !validText(payload.ContentType, 1, 128) {
		return false
	}
	for _, character := range payload.ContentType {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return len(payload.Data) <= maximum
}

func hasUnknownFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return true
	}
	found := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsList() && field.Kind() == protoreflect.MessageKind {
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				if hasUnknownFields(list.Get(index).Message()) {
					found = true
					return false
				}
			}
			return true
		}
		if field.IsMap() && field.MapValue().Kind() == protoreflect.MessageKind {
			value.Map().Range(func(_ protoreflect.MapKey, child protoreflect.Value) bool {
				if hasUnknownFields(child.Message()) {
					found = true
					return false
				}
				return true
			})
			return !found
		}
		if field.Kind() == protoreflect.MessageKind && hasUnknownFields(value.Message()) {
			found = true
			return false
		}
		return true
	})
	return found
}

func validToken(value string, maximum int) bool {
	return validText(value, 1, maximum) && tokenPattern.MatchString(value)
}

func validText(value string, minimum, maximum int) bool {
	size := len(value)
	return size >= minimum && size <= maximum && utf8.ValidString(value)
}
