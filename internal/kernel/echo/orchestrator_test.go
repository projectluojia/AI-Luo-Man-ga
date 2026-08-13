package echo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/agent"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/campustest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/bus"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// newSessionSource 为上下文装配器构造真实的会话历史来源（SQLite 存储 + 安全
// Blob 存储）。装配器是必需的接线，测试同样走真实来源，不注入空壳。
func newSessionSource(t *testing.T, store *sqlite.Store) *session.Service {
	t.Helper()
	blobs, err := blob.Open(filepath.Join(t.TempDir(), "blobs"), session.MaxMessageContentBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	service, err := session.NewService(store, blobs)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type fakeAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	testing *testing.T
}

type boundaryAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	testing *testing.T
}

type duplicateCallAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
}

type lateFrameAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
}

type missingUsageAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
}

type retryOnceAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	calls atomic.Int32
}

type slowSuccessAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	delay time.Duration
}

type sideEffectFailureAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
}

type configCaptureAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	starts chan *agentv1.StartRun
}

type revokingPolicyAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	testing      *testing.T
	capabilityID string
	revoke       func() error
}

type grantingPolicyAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	testing              *testing.T
	acceptedCapability   string
	newCapability        string
	permissionCapability string
	grantNewScope        func() error
}

type renewalCountingStore struct {
	*sqlite.Store
	renewals atomic.Int32
}

type nestedRunAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	testing   *testing.T
	rootRunID atomic.Value
}

type cancellingNestedAgent struct {
	agentv1.UnimplementedAgentRuntimeServer
	childStarted chan struct{}
}

func (a *cancellingNestedAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if start.GetStartRun().GetParentRunId() != "" {
		if err := stream.Send(acceptedFrame(start, 1)); err != nil {
			return err
		}
		close(a.childStarted)
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if err := stream.Send(acceptedFrame(start, 1)); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "delegate-call", CapabilityId: agent.CapabilityID,
			PayloadJson: []byte(`{"task":"等待取消","capability_ids":["campus.bus.routes.list"]}`),
		}},
	}); err != nil {
		return err
	}
	_, err = stream.Recv()
	return err
}

func (a *nestedRunAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	startFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	start := startFrame.GetStartRun()
	if start.GetParentRunId() == "" {
		a.rootRunID.Store(startFrame.GetRunId())
		return a.runRoot(stream, startFrame)
	}
	rootRunID, _ := a.rootRunID.Load().(string)
	if start.GetParentRunId() != rootRunID || startFrame.GetRunId() == rootRunID {
		a.testing.Errorf("child parent=%q run=%q root=%q", start.GetParentRunId(), startFrame.GetRunId(), rootRunID)
	}
	if start.GetInputMessage() != "只查询线路并总结" {
		a.testing.Errorf("child input=%q", start.GetInputMessage())
	}
	if start.GetMaxSteps() >= 4 || start.GetMaxToolCalls() >= 8 {
		a.testing.Errorf("child budgets steps=%d tools=%d", start.GetMaxSteps(), start.GetMaxToolCalls())
	}
	if len(start.GetCapabilities()) != 1 || start.GetCapabilities()[0].GetId() != campus.BusRouteListCapabilityID {
		a.testing.Errorf("child capabilities=%#v", start.GetCapabilities())
	}
	if err := stream.Send(acceptedFrame(startFrame, 1)); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: startFrame.EchoId, RunId: startFrame.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "child-route-call", CapabilityId: campus.BusRouteListCapabilityID, PayloadJson: []byte(`{"limit":10}`),
		}},
	}); err != nil {
		return err
	}
	result, err := stream.Recv()
	if err != nil {
		return err
	}
	if !result.GetCapabilityResult().GetSuccess() {
		a.testing.Errorf("child capability result=%#v", result.GetCapabilityResult())
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: startFrame.EchoId, RunId: startFrame.RunId, Sequence: 3,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "nested-delegate-call", CapabilityId: agent.CapabilityID, PayloadJson: []byte(`{"task":"越权嵌套"}`),
		}},
	}); err != nil {
		return err
	}
	rejected, err := stream.Recv()
	if err != nil {
		return err
	}
	if rejected.GetCapabilityResult().GetSuccess() || rejected.GetCapabilityResult().GetErrorCode() != "capability_disabled" {
		a.testing.Errorf("nested delegation result=%#v", rejected.GetCapabilityResult())
	}
	if err := stream.Send(usageFrame(startFrame, 4, 4, 2)); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: startFrame.EchoId, RunId: startFrame.RunId, Sequence: 5,
		Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "子任务完成"}},
	})
}

func (a *nestedRunAgent) runRoot(stream agentv1.AgentRuntime_RunServer, start *agentv1.AgentFrame) error {
	hasSubagent := false
	for _, capability := range start.GetStartRun().GetCapabilities() {
		if capability.GetId() == agent.CapabilityID {
			hasSubagent = true
		}
	}
	if !hasSubagent {
		a.testing.Error("root did not receive agent.run")
	}
	if err := stream.Send(acceptedFrame(start, 1)); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "delegate-call", CapabilityId: agent.CapabilityID,
			PayloadJson: []byte(`{"task":"只查询线路并总结","capability_ids":["campus.bus.routes.list"]}`),
		}},
	}); err != nil {
		return err
	}
	result, err := stream.Recv()
	if err != nil {
		return err
	}
	if !result.GetCapabilityResult().GetSuccess() ||
		!strings.Contains(string(result.GetCapabilityResult().GetPayloadJson()), `"result":"子任务完成"`) {
		a.testing.Errorf("root subagent result=%#v", result.GetCapabilityResult())
	}
	if err := stream.Send(usageFrame(start, 3, 6, 3)); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
		Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "父任务完成"}},
	})
}

func acceptedFrame(start *agentv1.AgentFrame, sequence uint64) *agentv1.AgentFrame {
	return &agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: sequence,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}
}

func usageFrame(start *agentv1.AgentFrame, sequence, inputTokens, outputTokens uint64) *agentv1.AgentFrame {
	return &agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: sequence,
		Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
			InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens,
		}},
	}
}

func (s *renewalCountingStore) RenewRunLease(ctx context.Context, run kernelecho.RunRecord, renewedAt, leaseExpiresAt time.Time) error {
	s.renewals.Add(1)
	return s.Store.RenewRunLease(ctx, run, renewedAt, leaseExpiresAt)
}

func (a *retryOnceAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if a.calls.Add(1) == 1 {
		return stream.Send(&agentv1.AgentFrame{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &agentv1.AgentFrame_RunFailure{RunFailure: &agentv1.RunFailure{
				Code: "provider_unavailable", Message: "temporary", Retryable: true,
			}},
		})
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
			InputTokens: 3, OutputTokens: 2, TotalTokens: 5,
		}},
	}); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "重试成功"}},
	})
}

func (a *slowSuccessAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	select {
	case <-time.After(a.delay):
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	for _, frame := range []*agentv1.AgentFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
				InputTokens: 3, OutputTokens: 2, TotalTokens: 5,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "续租成功"}},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *sideEffectFailureAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	for _, frame := range []*agentv1.AgentFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
				CallId: "external-call", CapabilityId: "test.external", PayloadJson: []byte(`{}`),
			}},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &agentv1.AgentFrame_RunFailure{RunFailure: &agentv1.RunFailure{
			Code: "provider_unavailable", Message: "temporary", Retryable: true,
		}},
	})
}

func (a *configCaptureAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	a.starts <- start.GetStartRun()
	for _, frame := range []*agentv1.AgentFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
				InputTokens: 3, OutputTokens: 2, TotalTokens: 5,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "配置恢复成功"}},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *revokingPolicyAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	projected := false
	for _, capability := range start.GetStartRun().GetCapabilities() {
		if capability.GetId() == a.capabilityID {
			projected = true
		}
	}
	if !projected {
		a.testing.Error("Capability 未在撤权前投影")
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if err := a.revoke(); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "revoked-call", CapabilityId: a.capabilityID, PayloadJson: []byte(`{}`),
		}},
	}); err != nil {
		return err
	}
	resultFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	result := resultFrame.GetCapabilityResult()
	if result.GetSuccess() || result.GetErrorCode() != "capability_disabled" {
		a.testing.Errorf("撤权后的 Capability 结果=%#v", result)
	}
	for _, frame := range []*agentv1.AgentFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
				InputTokens: 3, OutputTokens: 2, TotalTokens: 5,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
			Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "撤权已生效"}},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *grantingPolicyAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	acceptedProjected := false
	newProjected := false
	permissionProjected := false
	for _, capability := range start.GetStartRun().GetCapabilities() {
		switch capability.GetId() {
		case a.acceptedCapability:
			acceptedProjected = true
		case a.newCapability:
			newProjected = true
		case a.permissionCapability:
			permissionProjected = true
		}
	}
	if !acceptedProjected || newProjected || permissionProjected {
		a.testing.Errorf("Run 接受时的 Capability 投影错误：%#v", start.GetStartRun().GetCapabilities())
	}
	if err := stream.Send(acceptedFrame(start, 1)); err != nil {
		return err
	}
	if err := a.grantNewScope(); err != nil {
		return err
	}
	for index, test := range []struct {
		callID       string
		capabilityID string
		errorCode    string
	}{
		{callID: "late-capability-grant", capabilityID: a.newCapability, errorCode: "capability_disabled"},
		{callID: "late-permission-grant", capabilityID: a.permissionCapability, errorCode: "permission_denied"},
	} {
		if err := stream.Send(&agentv1.AgentFrame{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: uint64(index + 2),
			Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
				CallId: test.callID, CapabilityId: test.capabilityID, PayloadJson: []byte(`{}`),
			}},
		}); err != nil {
			return err
		}
		resultFrame, err := stream.Recv()
		if err != nil {
			return err
		}
		result := resultFrame.GetCapabilityResult()
		if result.GetSuccess() || result.GetErrorCode() != test.errorCode {
			a.testing.Errorf("Run 接受后的新增授权结果=%#v", result)
		}
	}
	if err := stream.Send(usageFrame(start, 4, 3, 2)); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 5,
		Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "授权未扩张"}},
	})
}

func (a *missingUsageAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "缺少用量"}},
	})
}

func (a *duplicateCallAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	call := &agentv1.CapabilityCall{
		CallId: "duplicate-call", CapabilityId: campus.BusRouteListCapabilityID, PayloadJson: []byte(`{"limit":10}`),
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
			InputTokens: 10, OutputTokens: 2, TotalTokens: 12,
		}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: call},
	}); err != nil {
		return err
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: call},
	})
}

func (a *lateFrameAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	frames := []*agentv1.AgentFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
				InputTokens: 10, OutputTokens: 2, TotalTokens: 12,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "不得成为最终结果"}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
			Body: &agentv1.AgentFrame_ReplyDelta{ReplyDelta: &agentv1.ReplyDelta{Text: "迟到"}},
		},
	}
	for _, frame := range frames {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *boundaryAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "boundary-call", CapabilityId: "private.capability", PayloadJson: []byte(`{"path":"/srv/private.db"}`),
		}},
	}); err != nil {
		return err
	}
	resultFrame, err := stream.Recv()
	if err != nil {
		return err
	}
	result := resultFrame.GetCapabilityResult()
	if result.GetSuccess() || result.GetErrorCode() != "capability_disabled" || result.GetErrorMessage() != "当前 App 未启用该 Capability" {
		a.testing.Errorf("unsafe capability result: %#v", result)
	}
	if strings.Contains(result.GetErrorMessage(), "private.capability") || strings.Contains(result.GetErrorMessage(), "campus-services") {
		a.testing.Errorf("capability result disclosed internal details: %#v", result)
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &agentv1.AgentFrame_RunFailure{RunFailure: &agentv1.RunFailure{
			Code: "private_failure", Message: "provider token api-key-secret", Retryable: true,
		}},
	})
}

func (f *fakeAgent) Run(stream agentv1.AgentRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &agentv1.AgentFrame_RunAccepted{RunAccepted: &agentv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if start.GetStartRun().GetModel() != "test-model" {
		f.testing.Errorf("model=%q", start.GetStartRun().GetModel())
	}
	available := map[string]bool{}
	for _, capability := range start.GetStartRun().GetCapabilities() {
		available[capability.Id] = true
	}
	if !available[campus.BusRouteListCapabilityID] {
		f.testing.Error("route capability was not projected")
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
			InputTokens: 10, OutputTokens: 2, TotalTokens: 12,
		}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &agentv1.AgentFrame_CapabilityCall{CapabilityCall: &agentv1.CapabilityCall{
			CallId: "call-1", CapabilityId: campus.BusRouteListCapabilityID, PayloadJson: []byte(`{"limit":10}`),
		}},
	}); err != nil {
		return err
	}
	result, err := stream.Recv()
	if err != nil {
		return err
	}
	if !result.GetCapabilityResult().GetSuccess() {
		f.testing.Fatalf("capability failed: %s", result.GetCapabilityResult().GetErrorMessage())
	}
	var decoded bus.RouteListResult
	if err := json.Unmarshal(result.GetCapabilityResult().GetPayloadJson(), &decoded); err != nil || len(decoded.Routes) != 1 {
		f.testing.Fatalf("result=%s err=%v", result.GetCapabilityResult().GetPayloadJson(), err)
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
		Body: &agentv1.AgentFrame_ReplyDelta{ReplyDelta: &agentv1.ReplyDelta{Text: "当前有一条线路。"}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 5,
		Body: &agentv1.AgentFrame_RunUsage{RunUsage: &agentv1.RunUsage{
			InputTokens: 30, OutputTokens: 5, TotalTokens: 35,
		}},
	}); err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 6,
		Body: &agentv1.AgentFrame_FinalMessage{FinalMessage: &agentv1.FinalMessage{Text: "当前有一条线路。"}},
	})
}

func TestOrchestratorRunsAgentCapabilityLoop(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &fakeAgent{testing: t})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx := context.Background()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	baseBusStore := memory.NewBusStore()
	baseBusStore.ReplaceCatalog(campus.AppID, nil, []bus.Route{{ID: "r", Name: "测试线路", Direction: "去程"}})
	busStore := &countingBusStore{Store: baseBusStore}
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	campustest.RegisterHosted(t, reg, busStore)
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai", MaxSteps: 4, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	events := []kernelecho.Event{}
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{Message: "有哪些线路", IdempotencyKey: "orchestrator-run"}, func(event kernelecho.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	record, persisted, err := store.GetEcho(ctx, campus.AppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "当前有一条线路。" {
		t.Fatalf("record=%#v", record)
	}
	if len(events) != 6 || len(persisted) != len(events) {
		t.Fatalf("events=%d persisted=%d", len(events), len(persisted))
	}
	if events[3].Type != "capability.completed" || events[len(events)-1].Type != "reply.final" {
		t.Fatalf("events=%#v", events)
	}
	if busStore.routeCalls.Load() != 1 {
		t.Fatalf("Agent frame invoked Tool %d times", busStore.routeCalls.Load())
	}
	audits, err := store.ListCapabilityCalls(ctx, campus.AppID, echoID)
	if err != nil || len(audits) != 1 {
		t.Fatalf("Agent call audits=%#v err=%v", audits, err)
	}
}

func TestOrchestratorAssemblesSessionContextIntoRun(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentServer := &configCaptureAgent{starts: make(chan *agentv1.StartRun, 1)}
	agentv1.RegisterAgentRuntimeServer(grpcServer, agentServer)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	connection, err := grpc.DialContext(t.Context(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "context-assembly.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessionService := newSessionSource(t, store)
	// 模拟平台 Intake 后的会话台账：两条历史消息 + 当前消息（均已在库）。
	now := time.Now().UTC()
	if err := store.CreateSession(context.Background(), session.Session{
		AppID: campus.AppID, SessionID: "session-1", Type: session.SessionTypeDirect,
		Members:   []session.Member{{UserID: "anonymous", Role: session.MemberRoleOwner, JoinedAt: now}},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for index, text := range []string{"第一条历史", "第二条历史", "当前的问题"} {
		if _, _, err := sessionService.CreateMessage(context.Background(), session.Message{
			AppID: campus.AppID, SessionID: "session-1", MessageID: "message-" + strconv.Itoa(index),
			SenderUserID: "anonymous", Type: session.MessageTypeText,
			ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len([]byte(text)))},
			CreatedAt:  now.Add(time.Duration(index) * time.Minute),
		}, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}

	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	campustest.RegisterHosted(t, reg, memory.NewBusStore())
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai", MaxSteps: 4, RunTimeout: 5 * time.Second, Context: sessionService},
	)
	events := []kernelecho.Event{}
	echoID, err := orchestrator.Run(t.Context(), kernelecho.RunRequest{
		Message: "当前的问题", IdempotencyKey: "context-assembly-run",
		SessionID: "session-1", UserID: "anonymous", MessageID: "message-2",
	}, func(event kernelecho.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := <-agentServer.starts
	if !strings.Contains(start.GetSystemPrompt(), "第一条历史") || !strings.Contains(start.GetSystemPrompt(), "第二条历史") {
		t.Fatalf("StartRun 系统提示缺少会话历史: %q", start.GetSystemPrompt())
	}
	if strings.Contains(start.GetSystemPrompt(), "当前的问题") {
		t.Fatalf("当前消息不得重复进入历史块: %q", start.GetSystemPrompt())
	}
	runs, err := store.ListRuns(t.Context(), campus.AppID, echoID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	run := runs[0]
	if len(run.ContextDigest) != 64 {
		t.Fatalf("Run 未固化上下文摘要: %#v", run)
	}
	if run.SessionID != "session-1" || run.UserID != "anonymous" || run.MessageID != "message-2" {
		t.Fatalf("Run 未持久化会话上下文: %#v", run)
	}
	var sources map[string]any
	if err := json.Unmarshal(run.ContextSources, &sources); err != nil {
		t.Fatalf("Run 来源版本不是合法 JSON: %v", err)
	}
	contextEvent := false
	for _, event := range events {
		if event.Type == "run.context" {
			contextEvent = true
			if !strings.Contains(string(event.Payload), run.ContextDigest) {
				t.Fatalf("run.context 事件缺少摘要: %s", event.Payload)
			}
		}
	}
	if !contextEvent {
		t.Fatal("缺少 run.context 事件")
	}
}

func TestOrchestratorRunsOneGovernedChildWithNarrowedProjection(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentServer := &nestedRunAgent{testing: t}
	agentv1.RegisterAgentRuntimeServer(grpcServer, agentServer)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx := context.Background()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "subagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	baseBusStore := memory.NewBusStore()
	baseBusStore.ReplaceCatalog(campus.AppID, nil, []bus.Route{{ID: "r", Name: "测试线路", Direction: "去程"}})
	busStore := &countingBusStore{Store: baseBusStore}
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	policy.Enable(campus.AppID, agent.CapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	campustest.RegisterHosted(t, reg, busStore)
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{
			AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai",
			MaxSteps: 4, MaxToolCalls: 8, MaxInputTokens: 1000, MaxOutputTokens: 500, MaxTotalTokens: 1500,
			MaxOutputBytes: 4096, ProviderTimeout: time.Second, RunTimeout: 5 * time.Second,
			Context: newSessionSource(t, store),
		},
	)
	if err := agent.Register(reg, orchestrator); err != nil {
		t.Fatal(err)
	}
	var delivered []kernelecho.Event
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{
		Message: "委派线路查询", IdempotencyKey: "governed-child-run",
	}, func(event kernelecho.Event) error {
		delivered = append(delivered, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	record, persisted, err := store.GetEcho(ctx, campus.AppID, echoID)
	if err != nil || record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "父任务完成" {
		t.Fatalf("Echo=%#v err=%v", record, err)
	}
	if len(delivered) != 5 || len(persisted) != len(delivered) {
		t.Fatalf("delivered=%#v persisted=%#v", delivered, persisted)
	}
	runs, err := store.ListRuns(ctx, campus.AppID, echoID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	var root, child kernelecho.RunRecord
	for _, run := range runs {
		if run.ParentRunID == "" {
			root = run
		} else {
			child = run
		}
	}
	if root.ID == "" || child.ParentRunID != root.ID || child.OriginCallID != "delegate-call" ||
		child.ResultMessage != "子任务完成" || child.Status != kernelecho.RunStatusSucceeded ||
		len(child.CapabilityScope) != 1 || child.CapabilityScope[0] != campus.BusRouteListCapabilityID ||
		child.MaxSteps >= root.MaxSteps || child.MaxToolCalls >= root.MaxToolCalls {
		t.Fatalf("root=%#v child=%#v", root, child)
	}
	// 子 Run 是干净工作区：不携带会话上下文，但仍固化自身的上下文摘要。
	if child.SessionID != "" || child.MessageID != "" || len(child.ContextDigest) != 64 {
		t.Fatalf("child 会话上下文或摘要错误: %#v", child)
	}
	if busStore.routeCalls.Load() != 1 {
		t.Fatalf("route calls=%d", busStore.routeCalls.Load())
	}
	audits, err := store.ListCapabilityCalls(ctx, campus.AppID, echoID)
	if err != nil || len(audits) != 3 {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
	delegationAudits := 0
	for _, audit := range audits {
		if audit.CapabilityID != agent.CapabilityID {
			continue
		}
		delegationAudits++
		var payload map[string]any
		if err := json.Unmarshal(audit.Payload, &payload); err != nil ||
			payload["task"] != "[已脱敏]" ||
			bytes.Contains(audit.Payload, []byte("只查询线路并总结")) ||
			bytes.Contains(audit.Payload, []byte("越权嵌套")) {
			t.Fatalf("子 Run 任务审计未净化：payload=%s err=%v", audit.Payload, err)
		}
	}
	if delegationAudits != 2 {
		t.Fatalf("子 Run 审计数量=%d，audits=%#v", delegationAudits, audits)
	}
}

func TestParentCancellationPropagatesAndPersistsChildThenRootTerminalState(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentServer := &cancellingNestedAgent{childStarted: make(chan struct{})}
	agentv1.RegisterAgentRuntimeServer(grpcServer, agentServer)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "subagent-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	policy.Enable(campus.AppID, agent.CapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	campustest.RegisterHosted(t, reg, memory.NewBusStore())
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{
			AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai",
			MaxSteps: 4, MaxToolCalls: 8, MaxInputTokens: 1000, MaxOutputTokens: 500, MaxTotalTokens: 1500,
			MaxOutputBytes: 4096, ProviderTimeout: time.Second, RunTimeout: 5 * time.Second,
			Context: newSessionSource(t, store),
		},
	)
	if err := agent.Register(reg, orchestrator); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-agentServer.childStarted
		cancel()
	}()
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{
		Message: "取消父任务", IdempotencyKey: "cancel-governed-child",
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
	record, _, readErr := store.GetEcho(context.Background(), campus.AppID, echoID)
	if readErr != nil || record.Status != kernelecho.StatusCancelled {
		t.Fatalf("Echo=%#v err=%v", record, readErr)
	}
	runs, readErr := store.ListRuns(context.Background(), campus.AppID, echoID)
	if readErr != nil || len(runs) != 2 {
		t.Fatalf("runs=%#v err=%v", runs, readErr)
	}
	for _, run := range runs {
		if run.Status != kernelecho.RunStatusCancelled {
			t.Fatalf("run=%#v", run)
		}
	}
}

func TestOrchestratorRecoversHistoricalAppConfigRevision(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentServer := &configCaptureAgent{starts: make(chan *agentv1.StartRun, 1)}
	agentv1.RegisterAgentRuntimeServer(grpcServer, agentServer)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(t.Context(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "historical-app-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	historical, _, err := store.Ensure(t.Context(), orchestratorAppConfig())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, created, err := orchestrator.CreateIdempotent(t.Context(), kernelecho.RunRequest{
		Message: "恢复历史配置", IdempotencyKey: "historical-config",
	})
	if err != nil || !created {
		t.Fatalf("echo=%q created=%t err=%v", echoID, created, err)
	}
	replacement := historical
	replacement.Model = "model-b"
	replacement.SystemPrompt = "当前配置 B"
	replacement.Timezone = "UTC"
	replacement.MaxSteps = 7
	replacement.MaxToolCalls = 6
	current, err := store.CompareAndSwap(t.Context(), historical.Generation, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.RunExisting(t.Context(), echoID, kernelecho.RunRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	start := <-agentServer.starts
	if start.GetModel() != historical.Model ||
		start.GetTimezone() != historical.Timezone ||
		start.GetMaxSteps() != historical.MaxSteps ||
		start.GetMaxToolCalls() != historical.MaxToolCalls ||
		!strings.Contains(start.GetSystemPrompt(), historical.SystemPrompt) ||
		strings.Contains(start.GetSystemPrompt(), replacement.SystemPrompt) {
		t.Fatalf("StartRun 未使用历史配置：%#v", start)
	}
	runs, err := store.ListRuns(t.Context(), campus.AppID, echoID)
	if err != nil || len(runs) != 1 ||
		runs[0].ModelConfigVersion != historical.Revision ||
		runs[0].ModelConfigVersion == current.Revision {
		t.Fatalf("runs=%#v current=%#v err=%v", runs, current, err)
	}
}

func TestOrchestratorRevalidatesCapabilityPolicyAfterProjection(t *testing.T) {
	const capabilityID = "test.read"
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy-revocation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := orchestratorAppConfig()
	seed.EnabledCapabilities = []string{capabilityID}
	current, _, err := store.Ensure(t.Context(), seed)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	var called atomic.Int32
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "test", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			capabilityID: {
				Spec: registry.CapabilitySpec{
					ID: capabilityID, Version: "1.0.0", Name: "测试读取",
					Description: "验证运行中动态撤权", ServiceID: "test",
					InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
					called.Add(1)
					return json.RawMessage(`{}`), nil
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &revokingPolicyAgent{
		testing: t, capabilityID: capabilityID,
		revoke: func() error {
			replacement := current
			replacement.EnabledCapabilities = nil
			_, updateErr := store.CompareAndSwap(context.Background(), current.Generation, replacement)
			return updateErr
		},
	})
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(t.Context(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	if _, err := orchestrator.Run(t.Context(), kernelecho.RunRequest{
		Message: "验证动态撤权", IdempotencyKey: "policy-revocation",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if called.Load() != 0 {
		t.Fatalf("撤权后处理器调用次数=%d", called.Load())
	}
}

func TestOrchestratorAcceptedRunScopeCannotExpandAfterGrant(t *testing.T) {
	const (
		acceptedCapability   = "test.alpha"
		newCapability        = "test.beta"
		permissionCapability = "test.gamma"
		permission           = "test.gamma.read"
	)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "policy-grant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := orchestratorAppConfig()
	seed.EnabledCapabilities = []string{acceptedCapability, permissionCapability}
	current, _, err := store.Ensure(t.Context(), seed)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	var newlyGrantedCalled atomic.Int32
	capabilities := make(map[string]struct {
		Spec    registry.CapabilitySpec
		Handler registry.Handler
	})
	for _, capabilityID := range []string{acceptedCapability, newCapability, permissionCapability} {
		handler := registry.Handler(func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			if capabilityID != acceptedCapability {
				newlyGrantedCalled.Add(1)
			}
			return json.RawMessage(`{}`), nil
		})
		var requiredPermissions []string
		if capabilityID == permissionCapability {
			requiredPermissions = []string{permission}
		}
		capabilities[capabilityID] = struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			Spec: registry.CapabilitySpec{
				ID: capabilityID, Version: "1.0.0", Name: "测试读取",
				Description: "验证已接受 Run 的授权范围不可扩张", ServiceID: "test",
				InputSchemaJSON:     `{"type":"object","additionalProperties":false}`,
				SideEffect:          registry.SideEffectRead,
				RequiredPermissions: requiredPermissions,
			},
			Handler: handler,
		}
	}
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID: "test", Version: "1.0.0", RequestedPermissions: []string{permission},
		},
		Capabilities: capabilities,
	}); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &grantingPolicyAgent{
		testing: t, acceptedCapability: acceptedCapability, newCapability: newCapability,
		permissionCapability: permissionCapability,
		grantNewScope: func() error {
			replacement := current
			replacement.EnabledCapabilities = []string{acceptedCapability, newCapability, permissionCapability}
			replacement.PermissionScope = []string{permission}
			_, updateErr := store.CompareAndSwap(context.Background(), current.Generation, replacement)
			return updateErr
		},
	})
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(t.Context(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := orchestrator.Run(t.Context(), kernelecho.RunRequest{
		Message: "验证新增授权不能扩张既有 Run", IdempotencyKey: "policy-late-grant",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newlyGrantedCalled.Load() != 0 {
		t.Fatalf("新增授权处理器调用次数=%d", newlyGrantedCalled.Load())
	}
	runs, err := store.ListRuns(t.Context(), campus.AppID, echoID)
	if err != nil || len(runs) != 1 ||
		len(runs[0].CapabilityScope) != 2 ||
		runs[0].CapabilityScope[0] != acceptedCapability ||
		runs[0].CapabilityScope[1] != permissionCapability ||
		len(runs[0].PermissionScope) != 0 {
		t.Fatalf("持久化 Run Scope=%#v err=%v", runs, err)
	}
}

func orchestratorAppConfig() appconfig.Config {
	return appconfig.Config{
		AppID: campus.AppID, Enabled: true, Model: "model-a", SystemPrompt: "历史配置 A",
		Timezone: "Asia/Shanghai", MaxSteps: 4, MaxToolCalls: 4,
		MaxInputTokens: 1024, MaxOutputTokens: 512, MaxTotalTokens: 1536,
		MaxOutputBytes: 4096, ProviderTimeout: 5 * time.Second,
	}
}

// TestOrchestratorAppendsChannelPromptFromPersistedConfig 验证渠道提示来自
// 持久化 App 配置（app_config_revisions.channel_prompts），并随 Run 渠道
// 进入装配后的系统提示；无渠道提示时行为与迁移前一致。
func TestOrchestratorAppendsChannelPromptFromPersistedConfig(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentServer := &configCaptureAgent{starts: make(chan *agentv1.StartRun, 1)}
	agentv1.RegisterAgentRuntimeServer(grpcServer, agentServer)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.DialContext(t.Context(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "channel-prompt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := orchestratorAppConfig()
	seed.ChannelPrompts = map[string]string{"qq_group": "【群聊规则】禁止 Markdown，像真实群聊。", "web": "【网页规则】适合连续阅读。"}
	if _, _, err := store.Ensure(t.Context(), seed); err != nil {
		t.Fatal(err)
	}
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := orchestrator.Run(t.Context(), kernelecho.RunRequest{
		Message: "群聊问题", IdempotencyKey: "channel-qq-group", Channel: "qq_group",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := <-agentServer.starts
	if !strings.Contains(start.GetSystemPrompt(), "【群聊规则】") ||
		strings.Contains(start.GetSystemPrompt(), "【网页规则】") {
		t.Fatalf("群聊渠道提示未按渠道装配: %q", start.GetSystemPrompt())
	}
	runs, err := store.ListRuns(t.Context(), campus.AppID, echoID)
	if err != nil || len(runs) != 1 || runs[0].Channel != "qq_group" {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
}

func TestOrchestratorDurablyRetriesOnlyRetryableRunAttempts(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentServer := &retryOnceAgent{}
	agentv1.RegisterAgentRuntimeServer(grpcServer, agentServer)
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, store,
		kernelecho.Config{
			AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai",
			MaxSteps: 4, RunTimeout: time.Second, MaxRunAttempts: 2,
			RetryBaseDelay: 20 * time.Millisecond, RetryMaxDelay: 20 * time.Millisecond,
			Context: newSessionSource(t, store),
		},
	)
	events := make([]kernelecho.Event, 0)
	echoID, err := orchestrator.Run(context.Background(), kernelecho.RunRequest{
		Message: "retry", IdempotencyKey: "retry-run",
	}, func(event kernelecho.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, kernelecho.ErrRunRetryScheduled) {
		t.Fatalf("first attempt error=%v", err)
	}
	record, _, err := store.GetEcho(context.Background(), campus.AppID, echoID)
	if err != nil || record.Status != kernelecho.StatusRunning {
		t.Fatalf("retrying record=%#v err=%v", record, err)
	}
	if len(events) == 0 {
		t.Fatalf("retry events=%#v", events)
	}
	for _, event := range events {
		if event.Type == "run.failed" {
			t.Fatalf("intermediate retry was exposed as terminal failure: %#v", events)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		work, listErr := orchestrator.Runnable(context.Background(), 1)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(work) == 1 {
			if err := orchestrator.RunExisting(context.Background(), echoID, kernelecho.RunRequest{}, nil); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("durable retry did not become runnable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _, err = store.GetEcho(context.Background(), campus.AppID, echoID)
	runs, listErr := store.ListRuns(context.Background(), campus.AppID, echoID)
	if err != nil || listErr != nil || record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "重试成功" ||
		len(runs) != 2 || runs[0].Status != kernelecho.RunStatusFailed || runs[1].Status != kernelecho.RunStatusSucceeded {
		t.Fatalf("record=%#v runs=%#v getErr=%v listErr=%v", record, runs, err, listErr)
	}
}

func TestOrchestratorRenewsActiveRunLease(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &slowSuccessAgent{delay: 320 * time.Millisecond})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	baseStore, err := sqlite.Open(filepath.Join(t.TempDir(), "renew.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer baseStore.Close()
	store := &renewalCountingStore{Store: baseStore}
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, store,
		kernelecho.Config{
			AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai",
			MaxSteps: 4, RunTimeout: 2 * time.Second, LeaseDuration: 150 * time.Millisecond,
			Context: newSessionSource(t, baseStore),
		},
	)
	if _, err := orchestrator.Run(context.Background(), kernelecho.RunRequest{
		Message: "renew", IdempotencyKey: "renew-run",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if store.renewals.Load() < 2 {
		t.Fatalf("lease renewals=%d, want at least 2", store.renewals.Load())
	}
}

func TestOrchestratorDoesNotAutomaticallyRetryAfterSideEffect(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &sideEffectFailureAgent{})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "side-effect-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, "test.external")
	var calls atomic.Int32
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "test", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			"test.external": {
				Spec: registry.CapabilitySpec{
					ID: "test.external", Version: "1.0.0", Name: "外部测试", Description: "验证副作用重试边界", ServiceID: "test",
					InputSchemaJSON: `{"type":"object","properties":{},"additionalProperties":false}`,
					SideEffect:      registry.SideEffectExternal,
				},
				Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
					calls.Add(1)
					return json.RawMessage(`{"ok":true}`), nil
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{
			AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai",
			MaxSteps: 4, RunTimeout: time.Second, MaxRunAttempts: 3,
			Context: newSessionSource(t, store),
		},
	)
	echoID, err := orchestrator.Run(context.Background(), kernelecho.RunRequest{
		Message: "external", IdempotencyKey: "external-run",
	}, nil)
	if !errors.Is(err, kernelecho.ErrAgentRunFailed) || errors.Is(err, kernelecho.ErrRunRetryScheduled) {
		t.Fatalf("side-effect run error=%v", err)
	}
	runs, listErr := store.ListRuns(context.Background(), campus.AppID, echoID)
	if listErr != nil || len(runs) != 1 || runs[0].Status != kernelecho.RunStatusFailed || calls.Load() != 1 {
		t.Fatalf("runs=%#v calls=%d err=%v", runs, calls.Load(), listErr)
	}
}

func TestOrchestratorRejectsDuplicateAgentCallBeforeSecondEffect(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &duplicateCallAgent{})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx := context.Background()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "duplicate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	baseBusStore := memory.NewBusStore()
	baseBusStore.ReplaceCatalog(campus.AppID, nil, []bus.Route{{ID: "r", Name: "测试线路", Direction: "去程"}})
	busStore := &countingBusStore{Store: baseBusStore}
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	campustest.RegisterHosted(t, reg, busStore)
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai", MaxSteps: 4, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{Message: "duplicate", IdempotencyKey: "duplicate-run"}, nil)
	if !errors.Is(err, executor.ErrDuplicateCall) {
		t.Fatalf("run error=%v, want ErrDuplicateCall", err)
	}
	if busStore.routeCalls.Load() != 1 {
		t.Fatalf("duplicate call executed Tool %d times", busStore.routeCalls.Load())
	}
	record, _, getErr := store.GetEcho(ctx, campus.AppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.ErrorCode != "protocol_violation" {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
	runs, listErr := store.ListRuns(ctx, campus.AppID, echoID)
	if listErr != nil || len(runs) != 1 || runs[0].LastAgentSequence != 3 {
		t.Fatalf("runs=%#v err=%v", runs, listErr)
	}
	audits, auditErr := store.ListCapabilityCalls(ctx, campus.AppID, echoID)
	if auditErr != nil || len(audits) != 1 {
		t.Fatalf("audits=%#v err=%v", audits, auditErr)
	}
}

func TestOrchestratorRejectsFramesAfterTerminalWithoutPublishingFinal(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &lateFrameAgent{})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx := context.Background()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "late.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, store,
		kernelecho.Config{AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai", MaxSteps: 4, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{Message: "late", IdempotencyKey: "late-run"}, nil)
	if !errors.Is(err, executor.ErrUnexpectedFrame) {
		t.Fatalf("run error=%v, want ErrUnexpectedFrame", err)
	}
	record, events, getErr := store.GetEcho(ctx, campus.AppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.FinalMessage != "" || record.ErrorCode != "protocol_violation" {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
	for _, event := range events {
		if event.Type == "reply.final" || event.Type == "reply.delta" {
			t.Fatalf("late terminal content was published: %#v", events)
		}
	}
	runs, listErr := store.ListRuns(ctx, campus.AppID, echoID)
	if listErr != nil || len(runs) != 1 || runs[0].LastAgentSequence != 3 {
		t.Fatalf("runs=%#v err=%v", runs, listErr)
	}
}

func TestOrchestratorRejectsSuccessfulTerminalWithoutUsage(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &missingUsageAgent{})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx := context.Background()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "missing-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, store,
		kernelecho.Config{AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai", MaxSteps: 4, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{Message: "usage", IdempotencyKey: "missing-usage-run"}, nil)
	if !errors.Is(err, executor.ErrUnexpectedFrame) {
		t.Fatalf("run error=%v, want ErrUnexpectedFrame", err)
	}
	record, events, getErr := store.GetEcho(ctx, campus.AppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.ErrorCode != "protocol_violation" {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
	for _, event := range events {
		if event.Type == "reply.final" {
			t.Fatalf("usage-free final was published: %#v", events)
		}
	}
}

type countingBusStore struct {
	bus.Store
	routeCalls atomic.Int32
}

func (s *countingBusStore) ListRoutes(ctx context.Context, appID string, request bus.RouteListRequest) (bus.RouteSnapshot, error) {
	s.routeCalls.Add(1)
	return s.Store.ListRoutes(ctx, appID, request)
}

func TestOrchestratorDoesNotExposeAgentOrCapabilityInternalErrors(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentRuntimeServer(grpcServer, &boundaryAgent{testing: t})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()
	ctx := context.Background()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		agentv1.NewAgentRuntimeClient(connection),
		reg,
		runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}),
		policy,
		store,
		kernelecho.Config{
			AppID: campus.AppID, Model: "test-model", SystemPrompt: "test",
			Timezone: "Asia/Shanghai", MaxSteps: 4, RunTimeout: 5 * time.Second,
			Context: newSessionSource(t, store),
		},
	)
	echoID, err := orchestrator.Run(ctx, kernelecho.RunRequest{Message: "trigger boundary errors", IdempotencyKey: "boundary-run"}, nil)
	if !errors.Is(err, kernelecho.ErrAgentRunFailed) {
		t.Fatalf("run error=%v, want ErrAgentRunFailed", err)
	}
	record, events, err := store.GetEcho(ctx, campus.AppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusFailed || record.ErrorCode != "agent_run_failed" || record.ErrorMessage != "Agent Run 执行失败" {
		t.Fatalf("unsafe terminal record: %#v", record)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"/srv/private.db", "api-key-secret", "sql_"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(record.ErrorMessage, secret) {
			t.Fatalf("public Echo data disclosed %q: record=%#v events=%s", secret, record, encoded)
		}
	}
	audits, err := store.ListCapabilityCalls(ctx, campus.AppID, echoID)
	if err != nil || len(audits) != 1 {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
	if audits[0].ErrorMessage != "当前 App 未启用该 Capability" {
		t.Fatalf("audit stored unsafe error: %#v", audits[0])
	}
}
