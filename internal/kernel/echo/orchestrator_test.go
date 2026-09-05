package echo_test

import (
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

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// 编排测试使用普通 Go handler 能力与中性标识：内核层不依赖任何业务包；
// hosted 沙箱与宿主函数链路由 loader 测试覆盖，真实包链路由 e2e 覆盖。
const (
	testAppID         = "app.test"
	routeCapabilityID = "demo.routes.list"
)

// registerRouteCapability 以普通 handler 注册编排测试用的线路能力。
// calls 非空时统计调用次数（幂等/去重断言用）。
func registerRouteCapability(t *testing.T, reg *registry.Registry, calls *atomic.Int32) {
	t.Helper()
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: routeCapabilityID, Version: "1.0.0", Name: "线路查询", Description: "编排测试能力",
			SideEffect:      capability.SideEffectRead,
			InputSchemaJSON: `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`,
		},
		Handler: func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
			if calls != nil {
				calls.Add(1)
			}
			return json.RawMessage(`{"data_status":{"state":"authoritative_fresh"},"routes":[{"id":"r","name":"测试线路","direction":"去程"}]}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}

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

type fakeExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
	testing *testing.T
}

type boundaryExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
	testing *testing.T
}

type duplicateCallExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

type lateFrameExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

type missingUsageExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

type overOutputExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

type duplicateOutputExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

type retryOnceExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
	calls atomic.Int32
}

type slowSuccessExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
	ready   chan struct{}
	release chan struct{}
}

type sideEffectFailureExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
}

type configCaptureExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
	starts chan *executorv1.StartRun
}

type revokingPolicyExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
	testing      *testing.T
	capabilityID string
	revoke       func() error
}

type grantingPolicyExecutor struct {
	executorv1.UnimplementedExecutorRuntimeServer
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

func acceptedFrame(start *executorv1.ExecutorFrame, sequence uint64) *executorv1.ExecutorFrame {
	return &executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: sequence,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}
}

func finalResult(text string) *executorv1.FinalResult {
	return &executorv1.FinalResult{Payload: &executorv1.Payload{
		ContentType: "text/plain; charset=utf-8", Data: []byte(text),
	}}
}

func outputDelta(text string) *executorv1.OutputDelta {
	return &executorv1.OutputDelta{Payload: &executorv1.Payload{
		ContentType: "text/plain; charset=utf-8", Data: []byte(text),
	}}
}

func usageFrame(start *executorv1.ExecutorFrame, sequence, inputTokens, outputTokens uint64) *executorv1.ExecutorFrame {
	return &executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: sequence,
		Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
			ExecutionUnits: inputTokens + outputTokens,
		}},
	}
}

func (s *renewalCountingStore) RenewRunLease(ctx context.Context, run kernelecho.RunRecord, renewedAt, leaseExpiresAt time.Time) error {
	s.renewals.Add(1)
	return s.Store.RenewRunLease(ctx, run, renewedAt, leaseExpiresAt)
}

func (a *retryOnceExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if a.calls.Add(1) == 1 {
		return stream.Send(&executorv1.ExecutorFrame{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &executorv1.ExecutorFrame_RunFailure{RunFailure: &executorv1.RunFailure{
				Code: "execution_unavailable", Message: "temporary", Retryable: true,
			}},
		})
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
			ExecutionUnits: 5,
		}},
	}); err != nil {
		return err
	}
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("重试成功")},
	})
}

func (a *slowSuccessExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	close(a.ready)
	select {
	case <-a.release:
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	for _, frame := range []*executorv1.ExecutorFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
				ExecutionUnits: 5,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("续租成功")},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *sideEffectFailureExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	for _, frame := range []*executorv1.ExecutorFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: &executorv1.CapabilityCall{
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
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &executorv1.ExecutorFrame_RunFailure{RunFailure: &executorv1.RunFailure{
			Code: "execution_unavailable", Message: "temporary", Retryable: true,
		}},
	})
}

func (a *configCaptureExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	a.starts <- start.GetStartRun()
	for _, frame := range []*executorv1.ExecutorFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
				ExecutionUnits: 5,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("配置恢复成功")},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *revokingPolicyExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
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
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if err := a.revoke(); err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: &executorv1.CapabilityCall{
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
	for _, frame := range []*executorv1.ExecutorFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
				ExecutionUnits: 5,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
			Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("撤权已生效")},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *grantingPolicyExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
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
		if err := stream.Send(&executorv1.ExecutorFrame{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: uint64(index + 2),
			Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: &executorv1.CapabilityCall{
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
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 5,
		Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("授权未扩张")},
	})
}

func (a *missingUsageExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("缺少用量")},
	})
}

func (a *overOutputExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{ExecutionUnits: 1}},
	}); err != nil {
		return err
	}
	for sequence := uint64(3); sequence <= 4; sequence++ {
		if err := stream.Send(&executorv1.ExecutorFrame{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: sequence,
			Body: &executorv1.ExecutorFrame_OutputDelta{OutputDelta: outputDelta(strings.Repeat("x", 3000))},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *duplicateOutputExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	for _, frame := range []*executorv1.ExecutorFrame{
		acceptedFrame(start, 1),
		usageFrame(start, 2, 1, 0),
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &executorv1.ExecutorFrame_OutputDelta{OutputDelta: outputDelta("answer")},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
			Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("answer")},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *duplicateCallExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	call := &executorv1.CapabilityCall{
		CallId: "duplicate-call", CapabilityId: routeCapabilityID, PayloadJson: []byte(`{"limit":10}`),
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
			ExecutionUnits: 12,
		}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: call},
	}); err != nil {
		return err
	}
	if _, err := stream.Recv(); err != nil {
		return err
	}
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
		Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: call},
	})
}

func (a *lateFrameExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	frames := []*executorv1.ExecutorFrame{
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
			Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
			Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
				ExecutionUnits: 12,
			}},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("不得成为最终结果")},
		},
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
			Body: &executorv1.ExecutorFrame_OutputDelta{OutputDelta: outputDelta("迟到")},
		},
	}
	for _, frame := range frames {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	return nil
}

func (a *boundaryExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: &executorv1.CapabilityCall{
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
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &executorv1.ExecutorFrame_RunFailure{RunFailure: &executorv1.RunFailure{
			Code: "private_failure", Message: "upstream token api-key-secret", Retryable: true,
		}},
	})
}

func (f *fakeExecutor) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 1,
		Body: &executorv1.ExecutorFrame_RunAccepted{RunAccepted: &executorv1.RunAccepted{ProtocolVersion: executor.Version}},
	}); err != nil {
		return err
	}
	available := map[string]bool{}
	for _, capability := range start.GetStartRun().GetCapabilities() {
		available[capability.Id] = true
	}
	if !available[routeCapabilityID] {
		f.testing.Error("route capability was not projected")
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 2,
		Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
			ExecutionUnits: 12,
		}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
		Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: &executorv1.CapabilityCall{
			CallId: "call-1", CapabilityId: routeCapabilityID, PayloadJson: []byte(`{"limit":10}`),
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
	var decoded struct {
		Routes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(result.GetCapabilityResult().GetPayloadJson(), &decoded); err != nil || len(decoded.Routes) != 1 {
		f.testing.Fatalf("result=%s err=%v", result.GetCapabilityResult().GetPayloadJson(), err)
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
		Body: &executorv1.ExecutorFrame_OutputDelta{OutputDelta: outputDelta("当前有一条线路。")},
	}); err != nil {
		return err
	}
	if err := stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 5,
		Body: &executorv1.ExecutorFrame_ResourceUsage{ResourceUsage: &executorv1.ResourceUsage{
			ExecutionUnits: 35,
		}},
	}); err != nil {
		return err
	}
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 6,
		Body: &executorv1.ExecutorFrame_FinalResult{FinalResult: finalResult("当前有一条线路。")},
	})
}

func TestOrchestratorRunsExecutorCapabilityLoop(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &fakeExecutor{testing: t})
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
	routeCalls := &atomic.Int32{}
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(testAppID, routeCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	registerRouteCapability(t, reg, routeCalls)
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	events := []kernelecho.Event{}
	echoID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{Message: "有哪些线路", IdempotencyKey: "orchestrator-run"}, func(event kernelecho.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	record, persisted, err := store.GetEcho(ctx, testAppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "当前有一条线路。" {
		t.Fatalf("record=%#v", record)
	}
	if len(events) != 6 || len(persisted) != len(events) {
		t.Fatalf("events=%d persisted=%d", len(events), len(persisted))
	}
	if events[3].Type != "capability.completed" || events[len(events)-1].Type != "run.completed" {
		t.Fatalf("events=%#v", events)
	}
	if routeCalls.Load() != 1 {
		t.Fatalf("Executor frame invoked Tool %d times", routeCalls.Load())
	}
	audits, err := store.ListCapabilityCalls(ctx, testAppID, echoID)
	if err != nil || len(audits) != 1 {
		t.Fatalf("Executor call audits=%#v err=%v", audits, err)
	}
}

func TestOrchestratorAssemblesSessionContextIntoRun(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorServer := &configCaptureExecutor{starts: make(chan *executorv1.StartRun, 1)}
	executorv1.RegisterExecutorRuntimeServer(grpcServer, executorServer)
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
		AppID: testAppID, SessionID: "session-1", Type: session.SessionTypeDirect,
		Members:   []session.Member{{UserID: "anonymous", Role: session.MemberRoleOwner, JoinedAt: now}},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for index, text := range []string{"第一条历史", "第二条历史", "当前的问题"} {
		if _, _, err := store.CreateMessage(context.Background(), session.Message{
			AppID: testAppID, SessionID: "session-1", MessageID: "message-" + strconv.Itoa(index),
			SenderUserID: "anonymous", Type: session.MessageTypeText,
			ContentRef: session.ContentRef{Mode: session.ContentModeInline, Size: int64(len([]byte(text)))},
			CreatedAt:  now.Add(time.Duration(index) * time.Minute),
		}, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}

	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(testAppID, routeCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	registerRouteCapability(t, reg, nil)
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: sessionService},
	)
	events := []kernelecho.Event{}
	echoID, err := runOrchestrator(orchestrator, t.Context(), kernelecho.RunRequest{
		Message: "当前的问题", IdempotencyKey: "context-assembly-run",
		SessionID: "session-1", UserID: "anonymous", MessageID: "message-2",
	}, func(event kernelecho.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := <-executorServer.starts
	if !strings.Contains(string(start.GetContextPayload().GetData()), "第一条历史") || !strings.Contains(string(start.GetContextPayload().GetData()), "第二条历史") {
		t.Fatalf("StartRun 系统提示缺少会话历史: %q", string(start.GetContextPayload().GetData()))
	}
	if strings.Contains(string(start.GetContextPayload().GetData()), "当前的问题") {
		t.Fatalf("当前消息不得重复进入历史块: %q", string(start.GetContextPayload().GetData()))
	}
	runs, err := store.ListRuns(t.Context(), testAppID, echoID)
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

func TestOrchestratorRecoversHistoricalAppConfigRevision(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorServer := &configCaptureExecutor{starts: make(chan *executorv1.StartRun, 1)}
	executorv1.RegisterExecutorRuntimeServer(grpcServer, executorServer)
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
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, created, err := orchestrator.CreateIdempotent(t.Context(), kernelecho.RunRequest{
		Message: "恢复历史配置", IdempotencyKey: "historical-config",
	})
	if err != nil || !created {
		t.Fatalf("echo=%q created=%t err=%v", echoID, created, err)
	}
	replacement := historical
	replacement.ExecutorID = "executor.b"
	replacement.ExecutorConfig = json.RawMessage(`{"strategy":"current-b"}`)
	replacement.MaxSteps = 7
	replacement.MaxCapabilityCalls = 6
	current, err := store.CompareAndSwap(t.Context(), historical.Generation, replacement)
	if err != nil {
		t.Fatal(err)
	}
	work, err := orchestrator.Runnable(t.Context(), 1)
	if err != nil || len(work) != 1 {
		t.Fatalf("历史配置 Run 未进入持久队列：work=%#v err=%v", work, err)
	}
	if err := orchestrator.RunQueued(t.Context(), work[0], nil); err != nil {
		t.Fatal(err)
	}
	start := <-executorServer.starts
	if start.GetMaxSteps() != historical.MaxSteps ||
		start.GetMaxCapabilityCalls() != historical.MaxCapabilityCalls ||
		string(start.GetExecutorConfig().GetData()) != string(historical.ExecutorConfig) {
		t.Fatalf("StartRun 未使用历史配置：%#v", start)
	}
	runs, err := store.ListRuns(t.Context(), testAppID, echoID)
	if err != nil || len(runs) != 1 ||
		runs[0].ConfigRevision != historical.Revision ||
		runs[0].ConfigRevision == current.Revision {
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
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: capabilityID, Version: "1.0.0", Name: "测试读取",
			Description:     "验证运行中动态撤权",
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			called.Add(1)
			return json.RawMessage(`{}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &revokingPolicyExecutor{
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
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	if _, err := runOrchestrator(orchestrator, t.Context(), kernelecho.RunRequest{
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
		Spec    capability.CapabilitySpec
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
			Spec    capability.CapabilitySpec
			Handler registry.Handler
		}{
			Spec: capability.CapabilitySpec{
				ID: capabilityID, Version: "1.0.0", Name: "测试读取",
				Description:         "验证已接受 Run 的授权范围不可扩张",
				InputSchemaJSON:     `{"type":"object","additionalProperties":false}`,
				SideEffect:          capability.SideEffectRead,
				RequiredPermissions: requiredPermissions,
			},
			Handler: handler,
		}
	}
	for _, registration := range capabilities {
		if err := reg.Register(registry.CapabilityRegistration{Spec: registration.Spec, Handler: registration.Handler}); err != nil {
			t.Fatal(err)
		}
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &grantingPolicyExecutor{
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
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := runOrchestrator(orchestrator, t.Context(), kernelecho.RunRequest{
		Message: "验证新增授权不能扩张既有 Run", IdempotencyKey: "policy-late-grant",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newlyGrantedCalled.Load() != 0 {
		t.Fatalf("新增授权处理器调用次数=%d", newlyGrantedCalled.Load())
	}
	runs, err := store.ListRuns(t.Context(), testAppID, echoID)
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
		AppID: testAppID, Enabled: true, ExecutorID: "executor.a",
		ExecutorConfig: json.RawMessage(`{"strategy":"historical-a"}`), MaxSteps: 4, MaxCapabilityCalls: 4,
		MaxExecutionUnits: 1536,
		MaxOutputBytes:    4096, ExecutionTimeout: 5 * time.Second,
	}
}

// orchestratorSeed 生成与历史 Orchestrator 默认预算等价的测试种子配置。

func orchestratorSeed(executorID string) appconfig.Config {
	return appconfig.Config{
		AppID: testAppID, Enabled: true, ExecutorID: executorID,
		ExecutorConfig: json.RawMessage(`{"strategy":"test"}`), MaxSteps: 4, MaxCapabilityCalls: 8,
		MaxExecutionUnits: 40960,
		MaxOutputBytes:    65536, MaxCostMicrousd: 0, ExecutionTimeout: 30 * time.Second,
	}
}

// seedOrchestratorConfig 把测试种子写入持久配置，并以 store 作为 Orchestrator
// 的 AppConfigSource（Orchestrator 强制要求持久配置来源，无合成配置回退）。
func seedOrchestratorConfig(t *testing.T, store *sqlite.Store, seed appconfig.Config) {
	t.Helper()
	if _, _, err := store.Ensure(t.Context(), seed); err != nil {
		t.Fatalf("seed app config: %v", err)
	}
}

type allStorePorts interface {
	idempotency.Store
	kernelecho.EchoCreationStore
	kernelecho.RunExecutionStore
	kernelecho.RunRecoveryStore
	kernelecho.RunCancellationStore
	kernelecho.EchoEventStore
	kernelecho.CapabilityAuditStore
}

func storePorts(store allStorePorts) kernelecho.StorePorts {
	return kernelecho.StorePorts{
		Idempotency: store, Creation: store, Execution: store, Recovery: store,
		Cancellation: store, Events: store, Audit: store,
	}
}

// runOrchestrator 是测试便捷入口：通过持久队列执行一次新建的 Echo。
func runOrchestrator(orchestrator *kernelecho.Orchestrator, ctx context.Context, request kernelecho.RunRequest, emit kernelecho.EventEmitter) (string, error) {
	echoID, created, err := orchestrator.CreateIdempotent(ctx, request)
	if err != nil {
		return "", err
	}
	if !created {
		return echoID, nil
	}
	work, err := orchestrator.Runnable(ctx, 1)
	if err != nil {
		return echoID, err
	}
	if len(work) != 1 || work[0].Run.EchoID != echoID || work[0].Run.ParentRunID != "" {
		return echoID, errors.New("新建的 root Run 未进入持久队列")
	}
	return echoID, orchestrator.RunQueued(ctx, work[0], emit)
}

func TestOrchestratorDurablyRetriesOnlyRetryableRunAttempts(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorServer := &retryOnceExecutor{}
	executorv1.RegisterExecutorRuntimeServer(grpcServer, executorServer)
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
	policy := runtimetest.NewStaticAppPolicy()
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, storePorts(store),
		kernelecho.Config{
			AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, MaxRunAttempts: 2,
			RetryBaseDelay: 20 * time.Millisecond, RetryMaxDelay: 20 * time.Millisecond,
			Context: newSessionSource(t, store),
		},
	)
	events := make([]kernelecho.Event, 0)
	echoID, err := runOrchestrator(orchestrator, context.Background(), kernelecho.RunRequest{
		Message: "retry", IdempotencyKey: "retry-run",
	}, func(event kernelecho.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, kernelecho.ErrRunRetryScheduled) {
		t.Fatalf("first attempt error=%v", err)
	}
	record, _, err := store.GetEcho(context.Background(), testAppID, echoID)
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
			if work[0].Run.EchoID != echoID || work[0].Run.ParentRunID != "" {
				t.Fatal("持久队列返回了错误的 Run")
			}
			if err := orchestrator.RunQueued(context.Background(), work[0], nil); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("durable retry did not become runnable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _, err = store.GetEcho(context.Background(), testAppID, echoID)
	runs, listErr := store.ListRuns(context.Background(), testAppID, echoID)
	if err != nil || listErr != nil || record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "重试成功" ||
		len(runs) != 2 || runs[0].Status != kernelecho.RunStatusFailed || runs[1].Status != kernelecho.RunStatusSucceeded {
		t.Fatalf("record=%#v runs=%#v getErr=%v listErr=%v", record, runs, err, listErr)
	}
}

func TestOrchestratorRenewsActiveRunLease(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executor := &slowSuccessExecutor{ready: make(chan struct{}), release: make(chan struct{})}
	executorv1.RegisterExecutorRuntimeServer(grpcServer, executor)
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
	renewDir := t.TempDir()
	baseStore, err := sqlite.Open(filepath.Join(renewDir, "renew.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, baseStore, renewDir) })
	store := &renewalCountingStore{Store: baseStore}
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	seedOrchestratorConfig(t, baseStore, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, storePorts(store),
		kernelecho.Config{
			AppID: testAppID, AppConfigSource: baseStore, RunTimeout: 3 * time.Second, LeaseDuration: 400 * time.Millisecond,
			Context: newSessionSource(t, baseStore),
		},
	)
	runDone := make(chan error, 1)
	go func() {
		_, runErr := runOrchestrator(orchestrator, context.Background(), kernelecho.RunRequest{
			Message: "renew", IdempotencyKey: "renew-run",
		}, nil)
		runDone <- runErr
	}()
	select {
	case <-executor.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("executor did not receive Run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for store.renewals.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if renewals := store.renewals.Load(); renewals < 2 {
		close(executor.release)
		<-runDone
		t.Fatalf("lease renewals=%d, want at least 2", renewals)
	}
	close(executor.release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
}

func TestOrchestratorDoesNotAutomaticallyRetryAfterSideEffect(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &sideEffectFailureExecutor{})
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
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(testAppID, "test.external")
	var calls atomic.Int32
	if err := reg.Register(registry.CapabilityRegistration{
		Spec: capability.CapabilitySpec{
			ID: "test.external", Version: "1.0.0", Name: "外部测试", Description: "验证副作用重试边界",
			InputSchemaJSON: `{"type":"object","properties":{},"additionalProperties":false}`,
			SideEffect:      capability.SideEffectExternal,
		},
		Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"ok":true}`), nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{
			AppID: testAppID, AppConfigSource: store, RunTimeout: time.Second, MaxRunAttempts: 3,
			Context: newSessionSource(t, store),
		},
	)
	echoID, err := runOrchestrator(orchestrator, context.Background(), kernelecho.RunRequest{
		Message: "external", IdempotencyKey: "external-run",
	}, nil)
	if !errors.Is(err, kernelecho.ErrExecutorRunFailed) || errors.Is(err, kernelecho.ErrRunRetryScheduled) {
		t.Fatalf("side-effect run error=%v", err)
	}
	runs, listErr := store.ListRuns(context.Background(), testAppID, echoID)
	if listErr != nil || len(runs) != 1 || runs[0].Status != kernelecho.RunStatusFailed || calls.Load() != 1 {
		t.Fatalf("runs=%#v calls=%d err=%v", runs, calls.Load(), listErr)
	}
}

func TestOrchestratorRejectsDuplicateExecutorCallBeforeSecondEffect(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &duplicateCallExecutor{})
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
	routeCalls := &atomic.Int32{}
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(testAppID, routeCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	registerRouteCapability(t, reg, routeCalls)
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{Message: "duplicate", IdempotencyKey: "duplicate-run"}, nil)
	if !errors.Is(err, executor.ErrDuplicateCall) {
		t.Fatalf("run error=%v, want ErrDuplicateCall", err)
	}
	if routeCalls.Load() != 1 {
		t.Fatalf("duplicate call executed Tool %d times", routeCalls.Load())
	}
	record, _, getErr := store.GetEcho(ctx, testAppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.ErrorCode != "protocol_violation" {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
	runs, listErr := store.ListRuns(ctx, testAppID, echoID)
	if listErr != nil || len(runs) != 1 || runs[0].LastExecutorSequence != 3 {
		t.Fatalf("runs=%#v err=%v", runs, listErr)
	}
	audits, auditErr := store.ListCapabilityCalls(ctx, testAppID, echoID)
	if auditErr != nil || len(audits) != 1 {
		t.Fatalf("audits=%#v err=%v", audits, auditErr)
	}
}

func TestOrchestratorRejectsFramesAfterTerminalWithoutPublishingFinal(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &lateFrameExecutor{})
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
	policy := runtimetest.NewStaticAppPolicy()
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{Message: "late", IdempotencyKey: "late-run"}, nil)
	if !errors.Is(err, executor.ErrUnexpectedFrame) {
		t.Fatalf("run error=%v, want ErrUnexpectedFrame", err)
	}
	record, events, getErr := store.GetEcho(ctx, testAppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.FinalMessage != "" || record.ErrorCode != "protocol_violation" {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
	for _, event := range events {
		if event.Type == "run.completed" || event.Type == "output.delta" {
			t.Fatalf("late terminal content was published: %#v", events)
		}
	}
	runs, listErr := store.ListRuns(ctx, testAppID, echoID)
	if listErr != nil || len(runs) != 1 || runs[0].LastExecutorSequence != 3 {
		t.Fatalf("runs=%#v err=%v", runs, listErr)
	}
}

func TestOrchestratorRejectsSuccessfulTerminalWithoutUsage(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &missingUsageExecutor{})
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
	policy := runtimetest.NewStaticAppPolicy()
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{Message: "usage", IdempotencyKey: "missing-usage-run"}, nil)
	if !errors.Is(err, executor.ErrUnexpectedFrame) {
		t.Fatalf("run error=%v, want ErrUnexpectedFrame", err)
	}
	record, events, getErr := store.GetEcho(ctx, testAppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.ErrorCode != "protocol_violation" {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
	for _, event := range events {
		if event.Type == "run.completed" {
			t.Fatalf("usage-free final was published: %#v", events)
		}
	}
}

func TestOrchestratorEnforcesCumulativeOutputBudget(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &overOutputExecutor{})
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
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "output-budget.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seed := orchestratorSeed("executor.test")
	seed.MaxOutputBytes = 4096
	seedOrchestratorConfig(t, store, seed)
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg,
		runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := runOrchestrator(orchestrator, context.Background(), kernelecho.RunRequest{
		Message: "累积输出", IdempotencyKey: "cumulative-output-budget",
	}, nil)
	if !errors.Is(err, kernelecho.ErrOutputBudgetExceeded) {
		t.Fatalf("run error=%v, want ErrOutputBudgetExceeded", err)
	}
	record, events, getErr := store.GetEcho(context.Background(), testAppID, echoID)
	if getErr != nil || record.Status != kernelecho.StatusFailed || record.ErrorCode != "budget_exceeded" || record.FinalMessage != "" {
		t.Fatalf("record=%#v events=%#v err=%v", record, events, getErr)
	}
	for _, event := range events {
		if event.Type == "run.completed" {
			t.Fatalf("over-budget output was published: %#v", events)
		}
	}
}

func TestOrchestratorDoesNotDoubleCountFinalResult(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &duplicateOutputExecutor{})
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
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "duplicate-output.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seed := orchestratorSeed("executor.test")
	seed.MaxOutputBytes = 8
	seedOrchestratorConfig(t, store, seed)
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg,
		runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}), policy, storePorts(store),
		kernelecho.Config{AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second, Context: newSessionSource(t, store)},
	)
	echoID, err := runOrchestrator(orchestrator, t.Context(), kernelecho.RunRequest{
		Message: "重复输出不应重复计费", IdempotencyKey: "duplicate-final-output",
	}, nil)
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	record, _, err := store.GetEcho(t.Context(), testAppID, echoID)
	if err != nil || record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "answer" {
		t.Fatalf("record=%#v err=%v", record, err)
	}
}

func TestOrchestratorDoesNotExposeExecutorOrCapabilityInternalErrors(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorv1.RegisterExecutorRuntimeServer(grpcServer, &boundaryExecutor{testing: t})
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
	policy := runtimetest.NewStaticAppPolicy()
	seedOrchestratorConfig(t, store, orchestratorSeed("executor.test"))
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection),
		reg,
		runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{}),
		policy,
		storePorts(store),
		kernelecho.Config{
			AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second,
			Context: newSessionSource(t, store),
		},
	)
	echoID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{Message: "trigger boundary errors", IdempotencyKey: "boundary-run"}, nil)
	if !errors.Is(err, kernelecho.ErrExecutorRunFailed) {
		t.Fatalf("run error=%v, want ErrExecutorRunFailed", err)
	}
	record, events, err := store.GetEcho(ctx, testAppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusFailed || record.ErrorCode != "executor_failed" || record.ErrorMessage != "执行者 Run 执行失败" {
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
	audits, err := store.ListCapabilityCalls(ctx, testAppID, echoID)
	if err != nil || len(audits) != 1 {
		t.Fatalf("audits=%#v err=%v", audits, err)
	}
	if audits[0].ErrorMessage != "当前 App 未启用该 Capability" {
		t.Fatalf("audit stored unsafe error: %#v", audits[0])
	}
}

func TestOrchestratorSendsOpaqueExecutorConfigAndNeutralContext(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executorServer := &configCaptureExecutor{starts: make(chan *executorv1.StartRun, 1)}
	executorv1.RegisterExecutorRuntimeServer(grpcServer, executorServer)
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
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "neutral-executor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := orchestratorAppConfig()
	seed.ExecutorConfig = json.RawMessage(`{"strategy":"deterministic","version":1}`)
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
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, storePorts(store),
		kernelecho.Config{
			AppID: testAppID, AppConfigSource: store, RunTimeout: 5 * time.Second,
			Context: newSessionSource(t, store),
		},
	)
	if _, err := runOrchestrator(orchestrator, t.Context(), kernelecho.RunRequest{
		Message: "通用执行者配置测试", IdempotencyKey: "opaque-config-render", Channel: "qq_group",
	}, nil); err != nil {
		t.Fatal(err)
	}
	start := <-executorServer.starts
	if string(start.GetExecutorConfig().GetData()) != string(seed.ExecutorConfig) {
		t.Fatalf("opaque executor config changed: %q", start.GetExecutorConfig().GetData())
	}
	var contextPayload struct {
		SchemaVersion string `json:"schema_version"`
		Blocks        []struct {
			Type string `json:"type"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(start.GetContextPayload().GetData(), &contextPayload); err != nil ||
		contextPayload.SchemaVersion != "ailuo.context.v1" || len(contextPayload.Blocks) == 0 {
		t.Fatalf("neutral context payload=%q err=%v", start.GetContextPayload().GetData(), err)
	}
}

func TestNewOrchestratorRejectsNegativeLeaseDuration(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != "orchestrator lease duration must be positive" {
			t.Fatalf("panic=%v", recovered)
		}
	}()
	kernelecho.NewOrchestrator(nil, nil, nil, nil, kernelecho.StorePorts{}, kernelecho.Config{
		LeaseDuration: -time.Second,
	})
	t.Fatal("negative lease duration was accepted")
}
