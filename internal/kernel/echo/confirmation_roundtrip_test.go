package echo_test

import (
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const confirmationTestCapabilityID = "test.confirm"

// confirmationAgent 模拟执行者的公共确认往返行为：首轮不带确认标识调用高风险
// Capability，收到 confirmation_required（携带确认投影）后以最终消息等待用户
// 决策；后续 Run 在 StartRun 投影中看到已批准确认时附带 confirmation_id 重试。
type confirmationAgent struct {
	executorv1.UnimplementedExecutorRuntimeServer
	// forcedConfirmationID 非空时强制作为 CapabilityCall 的 confirmation_id，
	// 用于模拟执行者携带过期/无效/张冠李戴确认的场景。
	forcedConfirmationID atomic.Value
	// forcedArguments 非空时替换调用参数，用于模拟参数变化的调用。
	forcedArguments atomic.Value
}

func (a *confirmationAgent) Run(stream executorv1.ExecutorRuntime_RunServer) error {
	start, err := stream.Recv()
	if err != nil {
		return err
	}
	approved := ""
	if override, ok := a.forcedConfirmationID.Load().(string); ok && override != "" {
		approved = override
	} else {
		for _, pending := range start.GetStartRun().GetPendingConfirmations() {
			if pending.GetCapabilityId() == confirmationTestCapabilityID && pending.GetStatus() == confirmation.StatusApproved {
				approved = pending.GetConfirmationId()
			}
		}
	}
	payload := []byte(`{"value":1}`)
	if override, ok := a.forcedArguments.Load().(string); ok && override != "" {
		payload = []byte(override)
	}
	for _, frame := range []*executorv1.ExecutorFrame{
		acceptedFrame(start, 1),
		usageFrame(start, 2, 3, 2),
		{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 3,
			Body: &executorv1.ExecutorFrame_CapabilityCall{CapabilityCall: &executorv1.CapabilityCall{
				CallId:         "confirm-call-" + start.RunId,
				CapabilityId:   confirmationTestCapabilityID,
				PayloadJson:    payload,
				ConfirmationId: approved,
			}},
		},
	} {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
	result, err := stream.Recv()
	if err != nil {
		return err
	}
	capabilityResult := result.GetCapabilityResult()
	if capabilityResult == nil {
		_ = stream.Send(&executorv1.ExecutorFrame{
			EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
			Body: &executorv1.ExecutorFrame_RunFailure{RunFailure: &executorv1.RunFailure{
				Code: "protocol_violation", Message: "期待 Capability 结果",
			}},
		})
		return nil
	}
	finalText := "等待确认"
	if capabilityResult.GetSuccess() {
		finalText = "执行完成"
	}
	return stream.Send(&executorv1.ExecutorFrame{
		EchoId: start.EchoId, RunId: start.RunId, Sequence: 4,
		Body: &executorv1.ExecutorFrame_FinalMessage{FinalMessage: &executorv1.FinalMessage{Text: finalText}},
	})
}

// TestOrchestratorConfirmationRoundTrip 覆盖公共确认往返协议的内核链路：
// 高风险调用失败并自动创建持久确认（同 Echo 同参数重试去重）→ 用户决策批准 →
// 同会话新 Echo 的 Run 经 StartRun 投影附带 confirmation_id 重试成功。
func TestOrchestratorConfirmationRoundTrip(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	agent := &confirmationAgent{}
	executions := &atomic.Int32{}
	executorv1.RegisterExecutorRuntimeServer(grpcServer, agent)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	store := sqlitetest.NewStore(t, "confirmation-roundtrip.db")
	seedOrchestratorConfig(t, store, orchestratorSeed("test-model"))
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(campus.AppID, confirmationTestCapabilityID)
	reg := registry.New()
	registerEchoTestCapability(t, reg, func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
		executions.Add(1)
		return json.RawMessage(`{"sent":true}`), nil
	})
	confirmations := confirmation.NewService(store, confirmation.Config{})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: confirmations,
	})
	orchestrator := kernelecho.NewOrchestrator(
		executorv1.NewExecutorRuntimeClient(connection), reg, dispatcher, policy, store,
		kernelecho.Config{
			AppID: campus.AppID, AppConfigSource: store, RunTimeout: 10 * time.Second,
			Context: newSessionSource(t, store), Confirmations: confirmations,
		},
	)
	ctx := context.Background()

	// 首轮：无确认 → confirmation_required + 确认自动创建，副作用不执行。
	echoID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{
		Message: "发车提醒", IdempotencyKey: "confirmation-1", SessionID: "session-1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.GetEcho(ctx, campus.AppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusSucceeded || record.FinalMessage != "等待确认" {
		t.Fatalf("首轮 Echo 终态=%s final=%q", record.Status, record.FinalMessage)
	}
	if executions.Load() != 0 {
		t.Fatalf("未批准前副作用执行了 %d 次", executions.Load())
	}
	active, err := confirmations.ActiveByEcho(ctx, campus.AppID, echoID)
	if err != nil || len(active) != 1 || active[0].Status != confirmation.StatusWaiting {
		t.Fatalf("首轮确认记录=%d err=%v，want 1 条 waiting", len(active), err)
	}
	if active[0].SessionID != "session-1" || active[0].ArgumentDigest == "" {
		t.Fatalf("确认记录会话/摘要绑定缺失：%+v", active[0])
	}
	if _, err := confirmations.Decide(ctx, campus.AppID, active[0].ConfirmationID,
		confirmation.StatusApproved, "user-1", time.Now().UTC()); err != nil {
		t.Fatalf("decide approve: %v", err)
	}

	// 续跑：同会话新 Echo，StartRun 投影已批准确认，附带 confirmation_id 重试成功。
	continuationID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{
		Message: "继续", IdempotencyKey: "confirmation-2", SessionID: "session-1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 {
		t.Fatalf("批准后副作用执行 %d 次，want 1", executions.Load())
	}
	continued, _, err := store.GetEcho(ctx, campus.AppID, continuationID)
	if err != nil {
		t.Fatal(err)
	}
	if continued.Status != kernelecho.StatusSucceeded || continued.FinalMessage != "执行完成" {
		t.Fatalf("续跑 Echo 终态=%s final=%q", continued.Status, continued.FinalMessage)
	}
	// 重复首轮调用被去重：确认记录仍只有 1 条。
	stillActive, err := confirmations.ActiveBySession(ctx, campus.AppID, "session-1")
	if err != nil || len(stillActive) != 1 {
		t.Fatalf("会话内确认记录=%d err=%v，want 1", len(stillActive), err)
	}
	if executions.Load() != 1 {
		t.Fatalf("Capability handler 执行 %d 次，want 1", executions.Load())
	}

	// 参数摘要自选：执行者携带无效 confirmation_id 但参数与既有批准一致时，
	// 内核按（目标、参数摘要、会话归属）自选正确批准并执行，不新建确认。
	agent.forcedConfirmationID.Store("bogus-confirmation-1")
	selectionID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{
		Message: "携带无效确认重试", IdempotencyKey: "confirmation-3", SessionID: "session-1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, _, err := store.GetEcho(ctx, campus.AppID, selectionID)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Status != kernelecho.StatusSucceeded || selection.FinalMessage != "执行完成" {
		t.Fatalf("摘要自选 Echo 终态=%s final=%q", selection.Status, selection.FinalMessage)
	}
	if executions.Load() != 2 {
		t.Fatalf("摘要自选后副作用执行 %d 次，want 2", executions.Load())
	}
	afterSelection, err := confirmations.ActiveBySession(ctx, campus.AppID, "session-1")
	if err != nil || len(afterSelection) != 1 {
		t.Fatalf("摘要自选后会话内确认记录=%d err=%v，want 1", len(afterSelection), err)
	}

	// 自愈：参数变化且无对应批准时，内核创建新的 waiting 确认并携带投影，
	// 副作用仍不执行。
	agent.forcedArguments.Store(`{"value":3}`)
	selfHealID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{
		Message: "用新参数调用", IdempotencyKey: "confirmation-4", SessionID: "session-1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	selfHealed, _, err := store.GetEcho(ctx, campus.AppID, selfHealID)
	if err != nil {
		t.Fatal(err)
	}
	if selfHealed.Status != kernelecho.StatusSucceeded || selfHealed.FinalMessage != "等待确认" {
		t.Fatalf("自愈 Echo 终态=%s final=%q", selfHealed.Status, selfHealed.FinalMessage)
	}
	if executions.Load() != 2 {
		t.Fatalf("自愈阶段副作用执行 %d 次，want 2", executions.Load())
	}
	afterHeal, err := confirmations.ActiveBySession(ctx, campus.AppID, "session-1")
	if err != nil || len(afterHeal) != 2 {
		t.Fatalf("自愈后会话内确认记录=%d err=%v，want 2（approved args1 + waiting args3）", len(afterHeal), err)
	}

	// 多批准按参数选择：为不同参数铸造第二个已批准确认；执行者携带"张冠李戴"
	// 的 confirmation_id 调用原参数时，内核按摘要自选正确批准执行，不产生新确认。
	runs, err := store.ListRuns(ctx, campus.AppID, continuationID)
	if err != nil || len(runs) == 0 {
		t.Fatalf("list runs err=%v", err)
	}
	secondSpec := confirmation.RequestSpec{
		CapabilityID: confirmationTestCapabilityID, TargetType: confirmation.TargetTypeCapability,
		TargetID: confirmationTestCapabilityID, SideEffect: confirmation.SideEffectExternal,
		IdempotencyKey: "operation-args-2", SessionID: "session-1",
	}
	second, err := confirmations.Request(ctx, campus.AppID, continuationID, runs[0].ID, "call-args-2",
		secondSpec, []byte(`{"value":2}`), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := confirmations.Decide(ctx, campus.AppID, second.ConfirmationID,
		confirmation.StatusApproved, "user-1", time.Now().UTC()); err != nil {
		t.Fatalf("decide second approval: %v", err)
	}
	agent.forcedArguments.Store(`{"value":1}`)
	multiApprovalID, err := runOrchestrator(orchestrator, ctx, kernelecho.RunRequest{
		Message: "调用原参数", IdempotencyKey: "confirmation-5", SessionID: "session-1",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	multiApproval, _, err := store.GetEcho(ctx, campus.AppID, multiApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if multiApproval.Status != kernelecho.StatusSucceeded || multiApproval.FinalMessage != "执行完成" {
		t.Fatalf("多批准选择 Echo 终态=%s final=%q", multiApproval.Status, multiApproval.FinalMessage)
	}
	if executions.Load() != 3 {
		t.Fatalf("多批准选择后副作用执行 %d 次，want 3", executions.Load())
	}
	afterMulti, err := confirmations.ActiveBySession(ctx, campus.AppID, "session-1")
	if err != nil || len(afterMulti) != 3 {
		t.Fatalf("多批准选择后会话内确认记录=%d err=%v，want 3", len(afterMulti), err)
	}
}

// registerEchoTestCapability 注册测试用需要确认的高风险 Capability。
func registerEchoTestCapability(t *testing.T, reg *registry.Registry, handler registry.Handler) {
	t.Helper()
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "test", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			confirmationTestCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID: confirmationTestCapabilityID, Version: "1.0.0", ServiceID: "test",
					Name: "测试确认能力", Description: "需要确认的测试能力",
					InputSchemaJSON:      `{"type":"object","properties":{"value":{"type":"integer"}},"additionalProperties":false}`,
					SideEffect:           registry.SideEffectExternal,
					RequiresConfirmation: true,
				},
				Handler: handler,
			},
		},
	}); err != nil {
		t.Fatalf("register capability: %v", err)
	}
}
