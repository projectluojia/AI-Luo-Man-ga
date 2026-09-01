package executor_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
)

func TestStartFrameContract(t *testing.T) {
	t.Parallel()

	valid := startFrame()
	if err := executor.ValidateStartFrame(valid); err != nil {
		t.Fatalf("validate start frame: %v", err)
	}
	child := startFrame()
	child.GetStartRun().ParentRunId = "parent-run"
	if err := executor.ValidateStartFrame(child); err != nil {
		t.Fatalf("validate child start frame: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*executor.Frame)
		target error
	}{
		{
			name: "version mismatch",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().ProtocolVersion = "3.0"
			},
			target: executor.ErrVersionMismatch,
		},
		{
			name: "sequence",
			mutate: func(frame *executor.Frame) {
				frame.Sequence = 2
			},
			target: executor.ErrSequence,
		},
		{
			name: "unknown protobuf field",
			mutate: func(frame *executor.Frame) {
				frame.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "oversized input",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().InputPayload.Data = []byte(strings.Repeat("x", executor.MaxInputPayloadBytes+1))
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "invalid trace id",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().TraceId = "not-a-trace"
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "invalid parent span id",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().ParentSpanId = strings.Repeat("0", 15)
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "self parent run",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().ParentRunId = frame.GetRunId()
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "malformed parent run",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().ParentRunId = "invalid parent"
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "duplicate capability",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().Capabilities = append(frame.GetStartRun().Capabilities, frame.GetStartRun().Capabilities[0])
			},
			target: executor.ErrInvalidFrame,
		},
		{
			name: "oversized context payload",
			mutate: func(frame *executor.Frame) {
				frame.GetStartRun().ContextPayload.Data = []byte(strings.Repeat("x", executor.MaxContextPayloadBytes+1))
			},
			target: executor.ErrInvalidFrame,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			frame := startFrame()
			test.mutate(frame)
			if err := executor.ValidateStartFrame(frame); !errors.Is(err, test.target) {
				t.Fatalf("got error %v, want %v", err, test.target)
			}
		})
	}
}

func TestInboundFramePayloadContracts(t *testing.T) {
	t.Parallel()

	callFrame := &executor.Frame{
		EchoId: "echo", RunId: "run", Sequence: 2,
		Body: &executor.Frame_CapabilityCall{CapabilityCall: &executor.CapabilityCall{
			CallId: "call-1", CapabilityId: "capability", PayloadJson: []byte(`{"value":1}`),
		}},
	}
	if err := executor.ValidateInboundEnvelope(callFrame, "echo", "run", 2); err != nil {
		t.Fatalf("validate envelope: %v", err)
	}
	if err := executor.ValidateCapabilityCall(callFrame.GetCapabilityCall()); err != nil {
		t.Fatalf("validate call: %v", err)
	}
	if err := executor.ValidateInboundEnvelope(callFrame, "echo", "run", 3); !errors.Is(err, executor.ErrSequence) {
		t.Fatalf("sequence error=%v", err)
	}
	if err := executor.ValidateInboundEnvelope(callFrame, "echo", "other-run", 2); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("identity error=%v", err)
	}
	callFrame.GetCapabilityCall().ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if err := executor.ValidateInboundEnvelope(callFrame, "echo", "run", 2); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("nested unknown field error=%v", err)
	}
	callFrame.GetCapabilityCall().ProtoReflect().SetUnknown(nil)

	invalidCalls := []*executor.CapabilityCall{
		{CallId: "bad call", CapabilityId: "capability", PayloadJson: []byte(`{}`)},
		{CallId: "call", CapabilityId: "capability", PayloadJson: []byte(`not-json`)},
		{CallId: "call", CapabilityId: "capability", PayloadJson: make([]byte, executor.MaxCapabilityPayloadBytes+1)},
	}
	for _, call := range invalidCalls {
		if err := executor.ValidateCapabilityCall(call); !errors.Is(err, executor.ErrInvalidFrame) {
			t.Fatalf("invalid call %#v error=%v", call, err)
		}
	}
	if err := executor.ValidateOutputDelta(&executor.OutputDelta{}); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("empty delta error=%v", err)
	}
	if err := executor.ValidateFinalResult(&executor.FinalResult{Payload: &executor.Payload{ContentType: "text/plain", Data: []byte(strings.Repeat("x", executor.MaxOutputPayloadBytes+1))}}); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("oversized final error=%v", err)
	}
	if err := executor.ValidateRunFailure(&executor.RunFailure{Code: "unsafe/code", Message: "secret"}); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("unsafe failure error=%v", err)
	}
}

func TestCapabilityResultContract(t *testing.T) {
	t.Parallel()

	valid := &executor.Frame{
		EchoId: "echo", RunId: "run", Sequence: 2,
		Body: &executor.Frame_CapabilityResult{CapabilityResult: &executor.CapabilityResult{
			CallId: "call", CapabilityId: "capability", Success: true, PayloadJson: []byte(`{"ok":true}`),
		}},
	}
	if err := executor.ValidateCapabilityResultFrame(valid, "echo", "run", 2); err != nil {
		t.Fatalf("validate successful result: %v", err)
	}
	valid.GetCapabilityResult().ErrorCode = "internal_error"
	if err := executor.ValidateCapabilityResultFrame(valid, "echo", "run", 2); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("mixed result error=%v", err)
	}

	failure := &executor.Frame{
		EchoId: "echo", RunId: "run", Sequence: 2,
		Body: &executor.Frame_CapabilityResult{CapabilityResult: &executor.CapabilityResult{
			CallId: "call", CapabilityId: "capability", ErrorCode: "permission_denied", ErrorMessage: "Capability 权限不足",
		}},
	}
	if err := executor.ValidateCapabilityResultFrame(failure, "echo", "run", 2); err != nil {
		t.Fatalf("validate failed result: %v", err)
	}
	failure.GetCapabilityResult().PayloadJson = []byte(`{}`)
	if err := executor.ValidateCapabilityResultFrame(failure, "echo", "run", 2); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("failed result payload error=%v", err)
	}
}

func TestResourceUsageContract(t *testing.T) {
	t.Parallel()

	valid := &executor.ResourceUsage{ExecutionUnits: 12, CostMicrousd: 3, Retries: 1}
	if err := executor.ValidateResourceUsage(valid, 6, 2, 0, 100, 10); err != nil {
		t.Fatalf("valid usage error=%v", err)
	}
	tests := []*executor.ResourceUsage{
		nil,
		{ExecutionUnits: 5, CostMicrousd: 3},
		{ExecutionUnits: 6, CostMicrousd: 1},
		{ExecutionUnits: 101, CostMicrousd: 3},
		{ExecutionUnits: 12, CostMicrousd: 11},
		{ExecutionUnits: 12, CostMicrousd: 3, Retries: 321},
	}
	for _, usage := range tests {
		if err := executor.ValidateResourceUsage(usage, 6, 2, 0, 100, 10); !errors.Is(err, executor.ErrInvalidFrame) {
			t.Fatalf("invalid usage %#v error=%v", usage, err)
		}
	}
}

func startFrame() *executor.Frame {
	return &executor.Frame{
		EchoId: "echo", RunId: "run", Sequence: 1,
		Body: &executor.Frame_StartRun{StartRun: &executor.StartRun{
			AppId: "app", InputPayload: &executor.Payload{ContentType: "text/plain", Data: []byte("message")},
			ContextPayload: &executor.Payload{ContentType: "application/ailuo.context+json", Data: []byte(`{"schema_version":"ailuo.context.v1","blocks":[]}`)},
			MaxSteps:       4, ProtocolVersion: executor.Version,
			MaxCapabilityCalls: 4, MaxExecutionUnits: 2000, MaxOutputBytes: 4096,
			TraceId: "11111111111111111111111111111111", ParentSpanId: "2222222222222222",
			Capabilities: []*executor.Capability{{
				Id: "capability", Version: "1.0.0", Name: "能力", Description: "description",
				InputSchemaJson: `{"type":"object","additionalProperties":false}`,
			}},
		}},
	}
}
