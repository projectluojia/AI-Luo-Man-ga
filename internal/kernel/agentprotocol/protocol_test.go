package agentprotocol_test

import (
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
)

func TestStartFrameContract(t *testing.T) {
	t.Parallel()

	valid := startFrame()
	if err := agentprotocol.ValidateStartFrame(valid); err != nil {
		t.Fatalf("validate start frame: %v", err)
	}
	child := startFrame()
	child.GetStartRun().ParentRunId = "parent-run"
	if err := agentprotocol.ValidateStartFrame(child); err != nil {
		t.Fatalf("validate child start frame: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*agentv1.AgentFrame)
		target error
	}{
		{
			name: "version mismatch",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().ProtocolVersion = "3.0"
			},
			target: agentprotocol.ErrVersionMismatch,
		},
		{
			name: "sequence",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.Sequence = 2
			},
			target: agentprotocol.ErrSequence,
		},
		{
			name: "unknown protobuf field",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "oversized input",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().InputMessage = strings.Repeat("x", agentprotocol.MaxInputMessageBytes+1)
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "invalid trace id",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().TraceId = "not-a-trace"
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "invalid parent span id",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().ParentSpanId = strings.Repeat("0", 15)
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "self parent run",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().ParentRunId = frame.GetRunId()
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "malformed parent run",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().ParentRunId = "invalid parent"
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "duplicate capability",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().Capabilities = append(frame.GetStartRun().Capabilities, frame.GetStartRun().Capabilities[0])
			},
			target: agentprotocol.ErrInvalidFrame,
		},
		{
			name: "oversized frame",
			mutate: func(frame *agentv1.AgentFrame) {
				frame.GetStartRun().SystemPrompt = strings.Repeat("x", agentprotocol.MaxFrameBytes)
			},
			target: agentprotocol.ErrFrameTooLarge,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			frame := startFrame()
			test.mutate(frame)
			if err := agentprotocol.ValidateStartFrame(frame); !errors.Is(err, test.target) {
				t.Fatalf("got error %v, want %v", err, test.target)
			}
		})
	}
}

func TestInboundFramePayloadContracts(t *testing.T) {
	t.Parallel()

	callFrame := &agentv1.AgentFrame{
		EchoId: "echo", RunId: "run", Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "call-1", CapabilityId: "capability", PayloadJson: []byte(`{"value":1}`),
		}},
	}
	if err := agentprotocol.ValidateInboundEnvelope(callFrame, "echo", "run", 2); err != nil {
		t.Fatalf("validate envelope: %v", err)
	}
	if err := agentprotocol.ValidateCapabilityCall(callFrame.GetCapabilityCall()); err != nil {
		t.Fatalf("validate call: %v", err)
	}
	if err := agentprotocol.ValidateInboundEnvelope(callFrame, "echo", "run", 3); !errors.Is(err, agentprotocol.ErrSequence) {
		t.Fatalf("sequence error=%v", err)
	}
	if err := agentprotocol.ValidateInboundEnvelope(callFrame, "echo", "other-run", 2); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("identity error=%v", err)
	}
	callFrame.GetCapabilityCall().ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if err := agentprotocol.ValidateInboundEnvelope(callFrame, "echo", "run", 2); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("nested unknown field error=%v", err)
	}
	callFrame.GetCapabilityCall().ProtoReflect().SetUnknown(nil)

	invalidCalls := []*agentv1.CapabilityCall{
		{CallId: "bad call", CapabilityId: "capability", PayloadJson: []byte(`{}`)},
		{CallId: "call", CapabilityId: "capability", PayloadJson: []byte(`not-json`)},
		{CallId: "call", CapabilityId: "capability", PayloadJson: make([]byte, agentprotocol.MaxCapabilityPayloadBytes+1)},
	}
	for _, call := range invalidCalls {
		if err := agentprotocol.ValidateCapabilityCall(call); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
			t.Fatalf("invalid call %#v error=%v", call, err)
		}
	}
	if err := agentprotocol.ValidateReplyDelta(&agentv1.ReplyDelta{}); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("empty delta error=%v", err)
	}
	if err := agentprotocol.ValidateFinalMessage(&agentv1.FinalMessage{Text: strings.Repeat("x", agentprotocol.MaxFinalMessageBytes+1)}); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("oversized final error=%v", err)
	}
	if err := agentprotocol.ValidateRunFailure(&agentv1.RunFailure{Code: "unsafe/code", Message: "secret"}); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("unsafe failure error=%v", err)
	}
}

func TestCapabilityResultContract(t *testing.T) {
	t.Parallel()

	valid := &agentv1.AgentFrame{
		EchoId: "echo", RunId: "run", Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityResult{CapabilityResult: &agentv1.CapabilityResult{
			CallId: "call", CapabilityId: "capability", Success: true, PayloadJson: []byte(`{"ok":true}`),
		}},
	}
	if err := agentprotocol.ValidateCapabilityResultFrame(valid, "echo", "run", 2); err != nil {
		t.Fatalf("validate successful result: %v", err)
	}
	valid.GetCapabilityResult().ErrorCode = "internal_error"
	if err := agentprotocol.ValidateCapabilityResultFrame(valid, "echo", "run", 2); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("mixed result error=%v", err)
	}

	failure := &agentv1.AgentFrame{
		EchoId: "echo", RunId: "run", Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityResult{CapabilityResult: &agentv1.CapabilityResult{
			CallId: "call", CapabilityId: "capability", ErrorCode: "permission_denied", ErrorMessage: "Capability 权限不足",
		}},
	}
	if err := agentprotocol.ValidateCapabilityResultFrame(failure, "echo", "run", 2); err != nil {
		t.Fatalf("validate failed result: %v", err)
	}
	failure.GetCapabilityResult().PayloadJson = []byte(`{}`)
	if err := agentprotocol.ValidateCapabilityResultFrame(failure, "echo", "run", 2); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
		t.Fatalf("failed result payload error=%v", err)
	}
}

func TestRunUsageContract(t *testing.T) {
	t.Parallel()

	valid := &agentv1.RunUsage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, CostMicrousd: 3, ProviderRetries: 1}
	if err := agentprotocol.ValidateRunUsage(valid, 5, 1, 6, 2, 0, 100, 100, 200, 10); err != nil {
		t.Fatalf("valid usage error=%v", err)
	}
	tests := []*agentv1.RunUsage{
		nil,
		{InputTokens: 4, OutputTokens: 2, TotalTokens: 6, CostMicrousd: 3},
		{InputTokens: 10, OutputTokens: 2, TotalTokens: 13, CostMicrousd: 3},
		{InputTokens: 101, OutputTokens: 2, TotalTokens: 103, CostMicrousd: 3},
		{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, CostMicrousd: 11},
		{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, CostMicrousd: 3, ProviderRetries: 321},
	}
	for _, usage := range tests {
		if err := agentprotocol.ValidateRunUsage(usage, 5, 1, 6, 2, 0, 100, 100, 200, 10); !errors.Is(err, agentprotocol.ErrInvalidFrame) {
			t.Fatalf("invalid usage %#v error=%v", usage, err)
		}
	}
}

func startFrame() *agentv1.AgentFrame {
	return &agentv1.AgentFrame{
		EchoId: "echo", RunId: "run", Sequence: 1,
		Body: &agentv1.AgentFrame_StartRun{StartRun: &agentv1.StartRun{
			AppId: "app", InputMessage: "message", Timezone: "Asia/Shanghai",
			Model: "model", SystemPrompt: "prompt", MaxSteps: 4, ProtocolVersion: agentprotocol.Version,
			MaxToolCalls: 4, MaxInputTokens: 1000, MaxOutputTokens: 1000,
			MaxTotalTokens: 2000, MaxOutputBytes: 4096, ProviderTimeoutMs: 5000,
			TraceId: "11111111111111111111111111111111", ParentSpanId: "2222222222222222",
			Capabilities: []*agentv1.Capability{{
				Id: "capability", Version: "1.0.0", Name: "能力", Description: "description",
				InputSchemaJson: `{"type":"object","additionalProperties":false}`,
			}},
		}},
	}
}
