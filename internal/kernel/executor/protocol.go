package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	Version = "3.0"

	MaxGRPCMessageBytes       = 512 << 10
	MaxFrameBytes             = 300 << 10
	MaxInputMessageBytes      = 16 << 10
	MaxSystemPromptBytes      = 32 << 10
	MaxCapabilities           = 64
	MaxCapabilitySchemaBytes  = 64 << 10
	MaxCapabilityPayloadBytes = 64 << 10
	MaxResultPayloadBytes     = 256 << 10
	MaxReplyDeltaBytes        = 16 << 10
	MaxFinalMessageBytes      = 64 << 10
	MaxFailureMessageBytes    = 1024
	MaxIdentifierBytes        = 128
	MaxDescriptionBytes       = 4096
	MaxNameBytes              = 256
	MaxProtocolSteps          = 64
	MaxToolCalls              = 256
	MaxTokenBudget            = 1_000_000_000
	MaxCostMicrousd           = 1_000_000_000_000_000
	MaxProviderTimeoutMS      = 120_000
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

func ValidateStartFrame(frame *Frame) error {
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
	if !validText(start.AppId, 1, MaxIdentifierBytes) ||
		!validText(start.InputMessage, 1, MaxInputMessageBytes) ||
		!validText(start.Timezone, 1, MaxIdentifierBytes) ||
		!validText(start.Model, 1, MaxIdentifierBytes) ||
		!validText(start.SystemPrompt, 1, MaxSystemPromptBytes) ||
		start.MaxSteps == 0 || start.MaxSteps > MaxProtocolSteps ||
		start.MaxToolCalls == 0 || start.MaxToolCalls > MaxToolCalls ||
		start.MaxInputTokens == 0 || start.MaxInputTokens > MaxTokenBudget ||
		start.MaxOutputTokens == 0 || start.MaxOutputTokens > MaxTokenBudget ||
		start.MaxTotalTokens == 0 || start.MaxTotalTokens > MaxTokenBudget ||
		start.MaxOutputBytes == 0 || start.MaxOutputBytes > MaxFinalMessageBytes ||
		start.MaxCostMicrousd > MaxCostMicrousd ||
		start.ProviderTimeoutMs < 100 || start.ProviderTimeoutMs > MaxProviderTimeoutMS ||
		(start.ParentRunId != "" && (!validToken(start.ParentRunId, MaxIdentifierBytes) || start.ParentRunId == frame.GetRunId())) ||
		!tracePattern.MatchString(start.TraceId) ||
		!spanPattern.MatchString(start.ParentSpanId) ||
		start.TraceId == "00000000000000000000000000000000" ||
		start.ParentSpanId == "0000000000000000" ||
		len(start.Capabilities) > MaxCapabilities {
		return ErrInvalidFrame
	}
	seen := make(map[string]struct{}, len(start.Capabilities))
	for _, capability := range start.Capabilities {
		if capability == nil ||
			!validText(capability.Id, 1, MaxIdentifierBytes) ||
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

func ValidateRunUsage(
	usage *RunUsage,
	previousInput uint64,
	previousOutput uint64,
	previousTotal uint64,
	previousCost uint64,
	previousProviderRetries uint32,
	maxInput uint64,
	maxOutput uint64,
	maxTotal uint64,
	maxCost uint64,
) error {
	if usage == nil ||
		usage.TotalTokens == 0 ||
		usage.InputTokens > MaxTokenBudget ||
		usage.OutputTokens > MaxTokenBudget ||
		usage.TotalTokens > MaxTokenBudget ||
		usage.CostMicrousd > MaxCostMicrousd ||
		usage.InputTokens < previousInput ||
		usage.OutputTokens < previousOutput ||
		usage.TotalTokens < previousTotal ||
		usage.CostMicrousd < previousCost ||
		usage.ProviderRetries < previousProviderRetries ||
		usage.ProviderRetries > 320 ||
		usage.TotalTokens != usage.InputTokens+usage.OutputTokens ||
		usage.InputTokens > maxInput ||
		usage.OutputTokens > maxOutput ||
		usage.TotalTokens > maxTotal ||
		(maxCost > 0 && usage.CostMicrousd > maxCost) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateInboundEnvelope(frame *Frame, echoID, runID string, expectedSequence uint64) error {
	return validateEnvelope(frame, echoID, runID, expectedSequence)
}

func ValidateRunAccepted(frame *Frame) error {
	accepted := frame.GetRunAccepted()
	if accepted == nil {
		return ErrUnexpectedFrame
	}
	if accepted.ProtocolVersion != Version {
		return ErrVersionMismatch
	}
	return nil
}

func ValidateCapabilityCall(call *CapabilityCall) error {
	if call == nil ||
		!validToken(call.CallId, MaxIdentifierBytes) ||
		!validToken(call.CapabilityId, MaxIdentifierBytes) ||
		len(call.PayloadJson) == 0 ||
		len(call.PayloadJson) > MaxCapabilityPayloadBytes ||
		!json.Valid(call.PayloadJson) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateReplyDelta(delta *ReplyDelta) error {
	if delta == nil || !validText(delta.Text, 1, MaxReplyDeltaBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateFinalMessage(message *FinalMessage) error {
	if message == nil || !validText(message.Text, 1, MaxFinalMessageBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateRunFailure(failure *RunFailure) error {
	if failure == nil ||
		!validToken(failure.Code, 64) ||
		!codePattern.MatchString(failure.Code) ||
		!validText(failure.Message, 1, MaxFailureMessageBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func ValidateCapabilityResultFrame(frame *Frame, echoID, runID string, expectedSequence uint64) error {
	if err := validateEnvelope(frame, echoID, runID, expectedSequence); err != nil {
		return err
	}
	result := frame.GetCapabilityResult()
	if result == nil ||
		!validToken(result.CallId, MaxIdentifierBytes) ||
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
	if len(result.PayloadJson) != 0 ||
		!validToken(result.ErrorCode, 64) ||
		!codePattern.MatchString(result.ErrorCode) ||
		!validText(result.ErrorMessage, 1, MaxFailureMessageBytes) {
		return ErrInvalidFrame
	}
	return nil
}

func validateEnvelope(frame *Frame, echoID, runID string, expectedSequence uint64) error {
	if frame == nil {
		return ErrInvalidFrame
	}
	if proto.Size(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if hasUnknownFields(frame.ProtoReflect()) {
		return ErrInvalidFrame
	}
	if !validToken(frame.EchoId, MaxIdentifierBytes) ||
		!validToken(frame.RunId, MaxIdentifierBytes) ||
		frame.EchoId != echoID || frame.RunId != runID ||
		frame.Body == nil {
		return ErrInvalidFrame
	}
	if expectedSequence == 0 || frame.Sequence != expectedSequence {
		return fmt.Errorf("%w: got=%d want=%d", ErrSequence, frame.Sequence, expectedSequence)
	}
	return nil
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
