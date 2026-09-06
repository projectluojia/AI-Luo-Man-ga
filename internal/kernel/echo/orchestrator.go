package echo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contextasm"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

var (
	ErrEmptyMessage             = errors.New("message is required")
	ErrExecutorRunFailed        = errors.New("executor run failed")
	ErrNoFinalResult            = errors.New("executor stream ended without final message")
	ErrOutputBudgetExceeded     = errors.New("executor output exceeds run budget")
	ErrCapabilityBudgetExceeded = errors.New("executor Capability call budget exceeded")
	ErrRunInputMismatch         = errors.New("run input does not match persisted Echo")
	ErrRunConfigUnavailable     = errors.New("persisted run configuration is unavailable")
	ErrMessageTooLong           = errors.New("message exceeds the maximum length")
	ErrAppConfigUnavailable     = errors.New("app configuration is unavailable")
	ErrAppDisabled              = errors.New("app is disabled")
)

type EventEmitter func(Event) error

type Config struct {
	AppID           string
	RunTimeout      time.Duration
	LeaseDuration   time.Duration
	MaxRunAttempts  uint32
	RetryBaseDelay  time.Duration
	RetryMaxDelay   time.Duration
	QueueCapacity   int
	AppConfigSource appconfig.Source
	Context         contextasm.HistorySource
	ContextBudget   contextasm.Budget
}

type Orchestrator struct {
	executor    executor.Client
	registry    *registry.Registry
	dispatcher  *runtime.Dispatcher
	policy      runtime.AppPolicy
	ports       StorePorts
	idempotency *idempotency.Manager
	context     *contextasm.Assembler
	config      Config
	now         func() time.Time
}

func NewOrchestrator(
	executorClient executor.Client,
	reg *registry.Registry,
	dispatcher *runtime.Dispatcher,
	policy runtime.AppPolicy,
	ports StorePorts,
	config Config,
) *Orchestrator {
	if config.RunTimeout == 0 {
		config.RunTimeout = 90 * time.Second
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 15 * time.Second
	}
	if config.LeaseDuration <= 0 {
		panic("orchestrator lease duration must be positive")
	}
	if config.MaxRunAttempts == 0 {
		config.MaxRunAttempts = 1
	}
	if config.RetryBaseDelay == 0 {
		config.RetryBaseDelay = 500 * time.Millisecond
	}
	if config.RetryMaxDelay == 0 {
		config.RetryMaxDelay = 5 * time.Second
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = 128
	}
	if ports.Idempotency == nil || ports.Creation == nil || ports.Execution == nil ||
		ports.Children == nil || ports.Recovery == nil || ports.Cancellation == nil || ports.Events == nil || ports.Audit == nil {
		panic("orchestrator storage ports are incomplete")
	}
	if config.AppConfigSource == nil {
		// 装配期编程错误：持久配置来源缺失时显式终止，不做合成配置回退。
		panic("orchestrator requires an app config source")
	}
	assembler, err := contextasm.New(config.Context, config.ContextBudget)
	if err != nil {
		// 装配期编程错误：上下文来源缺失或预算非法必须显式终止，不做静默降级。
		panic(fmt.Sprintf("orchestrator context assembly misconfigured: %v", err))
	}
	return &Orchestrator{
		executor:    executorClient,
		registry:    reg,
		dispatcher:  dispatcher,
		policy:      policy,
		ports:       ports,
		idempotency: idempotency.NewManager(ports.Idempotency),
		context:     assembler,
		config:      config,
		now:         time.Now,
	}
}

func (o *Orchestrator) currentAppConfig(ctx context.Context) (appconfig.Config, error) {
	config, err := o.config.AppConfigSource.Current(ctx, o.config.AppID)
	if err != nil {
		return appconfig.Config{}, err
	}
	if err := appconfig.VerifyCurrent(config, o.config.AppID); err != nil {
		return appconfig.Config{}, err
	}
	return config, nil
}

func (o *Orchestrator) appConfigRevision(ctx context.Context, revision string) (appconfig.Config, error) {
	config, err := o.config.AppConfigSource.Revision(ctx, o.config.AppID, revision)
	if err != nil {
		return appconfig.Config{}, err
	}
	if err := appconfig.Verify(config, o.config.AppID, revision); err != nil {
		return appconfig.Config{}, err
	}
	return config, nil
}

// runMatchesAppConfig 校验 Run 与其创建时锁定的配置修订一致，且全部预算
// 不超过该修订的上限。root Run 的预算按配置原样写入（相等），child Run 的
// 预算是父 Run 的收窄结果（≤ 上限），同一规则同时覆盖两类来源。
// 复核发生在 claim 之后：防止持久层记录被篡改或在配置修订回退后继续执行。
func runMatchesAppConfig(run RunRecord, config appconfig.Config) bool {
	return run.ConfigRevision == config.Revision && run.ExecutorID == config.ExecutorID &&
		run.MaxSteps > 0 && run.MaxSteps <= config.MaxSteps &&
		run.MaxCapabilityCalls > 0 && run.MaxCapabilityCalls <= config.MaxCapabilityCalls &&
		run.MaxExecutionUnits > 0 && run.MaxExecutionUnits <= config.MaxExecutionUnits &&
		run.MaxOutputBytes > 0 && run.MaxOutputBytes <= config.MaxOutputBytes &&
		run.MaxCostMicrousd <= config.MaxCostMicrousd &&
		run.ExecutionTimeoutMS > 0 && run.ExecutionTimeoutMS <= uint32(config.ExecutionTimeout.Milliseconds())
}

func (o *Orchestrator) Recoverable(ctx context.Context) ([]RunWork, error) {
	failed, err := o.ports.Recovery.FailAbandonedRuns(ctx, o.config.AppID, o.now().UTC())
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		observe.Warn(ctx, "启动时已终止无法安全恢复的遗留 Run",
			observe.StringAttr("app_id", o.config.AppID),
			observe.Int64Attr("run_count", failed),
		)
	}
	work, err := o.ports.Recovery.ListQueuedRuns(ctx, o.config.AppID, 1000)
	if err == nil {
		observe.DefaultMetrics().SetQueuedRuns(len(work))
	}
	return work, err
}

func (o *Orchestrator) Runnable(ctx context.Context, limit int) ([]RunWork, error) {
	work, err := o.ports.Recovery.ListRunnableRuns(ctx, o.config.AppID, o.now().UTC(), limit)
	if err == nil {
		observe.DefaultMetrics().SetQueuedRuns(len(work))
	}
	return work, err
}

func (o *Orchestrator) Cancel(ctx context.Context, echoID string) (bool, error) {
	cancelled, err := o.ports.Cancellation.CancelQueuedRun(ctx, o.config.AppID, echoID, o.now().UTC())
	if err == nil && cancelled {
		observe.DefaultMetrics().Cancellation()
		observe.DefaultMetrics().QueueRemoved()
	}
	return cancelled, err
}

func (o *Orchestrator) CancelQueuedRuns(ctx context.Context) error {
	return o.ports.Cancellation.CancelQueuedRuns(ctx, o.config.AppID, o.now().UTC())
}

func (o *Orchestrator) CreateIdempotent(ctx context.Context, request RunRequest) (string, bool, error) {
	if request.Message == "" {
		return "", false, ErrEmptyMessage
	}
	if utf8.RuneCountInString(request.Message) > 4000 {
		return "", false, ErrMessageTooLong
	}
	if err := idempotency.ValidateKey(request.IdempotencyKey); err != nil {
		return "", false, err
	}
	app, err := o.currentAppConfig(ctx)
	if err != nil {
		return "", false, errors.Join(ErrAppConfigUnavailable, err)
	}
	if !app.Enabled {
		return "", false, ErrAppDisabled
	}
	acceptedPolicy, err := o.policy.Snapshot(ctx, o.config.AppID)
	if err != nil {
		return "", false, errors.Join(ErrAppConfigUnavailable, err)
	}
	if err := acceptedPolicy.Verify(o.config.AppID); err != nil {
		return "", false, errors.Join(ErrAppConfigUnavailable, err)
	}
	if !acceptedPolicy.Enabled {
		return "", false, ErrAppDisabled
	}
	echoID := uuid.NewString()
	runID := uuid.NewString()
	createdAt := o.now().UTC()
	storedEchoID, created, err := o.ports.Creation.CreateEchoRunIdempotentLimited(ctx, request.IdempotencyKey, idempotency.Fingerprint([]byte(request.Message)), Record{
		ID:           echoID,
		AppID:        o.config.AppID,
		InputMessage: request.Message,
		Status:       StatusRunning,
		CreatedAt:    createdAt,
	}, RunRecord{
		ID:                 runID,
		RunGroupID:         runID,
		AppID:              o.config.AppID,
		EchoID:             echoID,
		SessionID:          request.SessionID,
		UserID:             request.UserID,
		MessageID:          request.MessageID,
		Channel:            request.Channel,
		Attempt:            1,
		Status:             RunStatusQueued,
		ExecutorID:         app.ExecutorID,
		ConfigRevision:     app.Revision,
		ProtocolVersion:    executor.Version,
		ExecutorConfig:     append(json.RawMessage(nil), app.ExecutorConfig...),
		InputPayload:       []byte(request.Message),
		InputContentType:   "text/plain; charset=utf-8",
		MaxSteps:           app.MaxSteps,
		MaxCapabilityCalls: app.MaxCapabilityCalls,
		MaxExecutionUnits:  app.MaxExecutionUnits,
		MaxOutputBytes:     app.MaxOutputBytes,
		MaxCostMicrousd:    app.MaxCostMicrousd,
		ExecutionTimeoutMS: uint32(app.ExecutionTimeout.Milliseconds()),
		Deadline:           createdAt.Add(o.config.RunTimeout),
		AvailableAt:        createdAt,
		CapabilityGrants:   append([]capability.Grant(nil), acceptedPolicy.CapabilityGrants...),
		RecoverableState:   json.RawMessage(`{}`),
		CreatedAt:          createdAt,
	}, o.config.QueueCapacity)
	if err != nil {
		observe.Error(ctx, "持久化新 Echo 和排队 Run 失败", err,
			observe.StringAttr("app_id", o.config.AppID),
			observe.StringAttr("echo_id", echoID),
			observe.StringAttr("run_id", runID),
		)
		return "", false, err
	}
	if !created {
		observe.Info(ctx, "Echo 创建请求命中持久化幂等结果",
			observe.StringAttr("app_id", o.config.AppID),
			observe.StringAttr("echo_id", storedEchoID),
		)
		return storedEchoID, false, nil
	}
	observe.Info(ctx, "Echo 与排队 Run 已原子创建",
		observe.StringAttr("app_id", o.config.AppID),
		observe.StringAttr("echo_id", echoID),
		observe.StringAttr("run_id", runID),
		observe.IntAttr("message_length", utf8.RuneCountInString(request.Message)),
	)
	observe.DefaultMetrics().QueueAdded()
	return storedEchoID, true, nil
}

// RunQueued 按持久队列返回的精确 run_id 认领并执行 Run。
// 输入 payload 只从持久 Run 读取，不能回退到其他 Echo 或本地状态。
func (o *Orchestrator) RunQueued(ctx context.Context, work RunWork, emit EventEmitter) (resultErr error) {
	run := work.Run
	if run.AppID != o.config.AppID || run.ID == "" || run.EchoID == "" ||
		run.Status != RunStatusQueued || len(run.InputPayload) == 0 || run.InputContentType == "" {
		return ErrRunInputMismatch
	}
	runStarted := o.now().UTC()
	leaseToken := uuid.NewString()
	claimed, err := o.ports.Execution.ClaimRun(ctx, o.config.AppID, run.EchoID, run.ID, leaseToken, runStarted, runStarted.Add(o.config.LeaseDuration))
	if err != nil {
		return err
	}
	if claimed.ID != run.ID {
		return ErrRunInputMismatch
	}
	if string(claimed.InputPayload) != string(run.InputPayload) || claimed.InputContentType != run.InputContentType {
		return errors.Join(ErrRunInputMismatch, o.fail(ctx, claimed, "recovery_failed", false))
	}
	return o.executeClaimedRun(ctx, emit, claimed, runStarted)
}

func (o *Orchestrator) executeClaimedRun(ctx context.Context, emit EventEmitter, run RunRecord, runStarted time.Time) (resultErr error) {
	echoID := run.EchoID
	observe.DefaultMetrics().RunStarted()
	observe.DefaultMetrics().QueueRemoved()
	defer observe.DefaultMetrics().RunStopped()
	app, configErr := o.appConfigRevision(ctx, run.ConfigRevision)
	if configErr != nil || !runMatchesAppConfig(run, app) || run.ProtocolVersion != executor.Version {
		completeErr := o.completeRun(ctx, run, RunStatusFailed, StatusFailed, Output{}, publicerror.Echo("recovery_failed"))
		return errors.Join(ErrRunConfigUnavailable, configErr, completeErr)
	}
	runID := run.ID
	ctx = observe.With(ctx,
		observe.Component("echo_orchestrator"),
		observe.StringAttr("app_id", o.config.AppID),
		observe.StringAttr("echo_id", echoID),
		observe.StringAttr("run_id", runID),
		observe.StringAttr("parent_run_id", run.ParentRunID),
	)
	ctx, runSpan := observe.StartSpan(ctx, "executor.run")
	defer func() {
		runSpan.End(resultErr)
	}()
	runContext, cancel := context.WithDeadline(ctx, run.Deadline)
	defer cancel()
	leaseContext, stopLease := context.WithCancel(runContext)
	leaseFailure := make(chan error, 1)
	go o.renewLease(leaseContext, cancel, run, leaseFailure)
	defer stopLease()
	observe.Info(ctx, "开始执行 Run",
		observe.StringAttr("executor_id", run.ExecutorID),
		observe.StringAttr("config_revision", run.ConfigRevision),
		observe.IntAttr("attempt", int(run.Attempt)),
		observe.Int64Attr("deadline_unix_ms", run.Deadline.UnixMilli()),
		observe.IntAttr("max_steps", int(run.MaxSteps)),
	)
	// Echo 事件流是面向用户的执行叙事：child Run 是 root Run 的内部委托，
	// 其进度和结果只通过持久状态（run.get_child_status）返回给父 Run，
	// 不进入 Echo 事件流——否则子 Run 的 run.completed 会被聊天入口当作
	// 终态提前关闭用户流，其输出也会越过治理边界直接暴露给前端。
	emitEvent := func(eventType string, payload any) error {
		if run.ParentRunID != "" {
			return nil
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode echo event: %w", err)
		}
		event := Event{
			AppID:     o.config.AppID,
			EchoID:    echoID,
			RunID:     runID,
			Type:      eventType,
			Payload:   encoded,
			CreatedAt: o.now().UTC(),
		}
		// 事件追加脱离取消上下文：取消竞速时用已取消 ctx 写库会中断语句并
		// 遗留 SQLite 写锁（影响后续终态写入）；事件是持久日志，走有界独立上下文。
		appendCtx, cancelAppend := context.WithTimeout(context.WithoutCancel(runContext), 5*time.Second)
		event, err = o.ports.Events.AppendEchoEvent(appendCtx, event)
		cancelAppend()
		if err != nil {
			observe.Error(ctx, "持久化 Echo 事件失败", err,
				observe.StringAttr("event_type", eventType),
			)
			return err
		}
		observe.Debug(ctx, "Echo 事件已经记录",
			observe.StringAttr("event_type", eventType),
			observe.Int64Attr("event_sequence", int64(event.Sequence)),
		)
		if emit != nil {
			return emit(event)
		}
		return nil
	}

	if err := emitEvent("echo.started", map[string]any{"app_id": o.config.AppID}); err != nil {
		return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", true))
	}
	policy, err := o.policy.Snapshot(runContext, o.config.AppID)
	if err != nil {
		return errors.Join(ErrAppConfigUnavailable, err, o.fail(ctx, run, "app_policy_unavailable", true))
	}
	if err := policy.Verify(o.config.AppID); err != nil {
		return errors.Join(ErrAppConfigUnavailable, err, o.fail(ctx, run, "app_policy_unavailable", true))
	}
	if !policy.Enabled {
		return errors.Join(ErrAppDisabled, o.fail(ctx, run, "app_disabled", false))
	}
	capabilities := o.projectCapabilities(policy, run)
	// 上下文装配：由 Go 按治理来源裁剪本次执行上下文，执行者只接收
	// 已装配的中性 payload，不接触会话存储或 App 配置正文。
	snapshot, err := o.context.Assemble(runContext, contextasm.Input{
		AppID:            o.config.AppID,
		SessionID:        run.SessionID,
		CurrentMessageID: run.MessageID,
		ConfigRevision:   run.ConfigRevision,
		Channel:          run.Channel,
		InputContentType: run.InputContentType,
		InputPayload:     run.InputPayload,
		Capabilities:     capabilityVersions(capabilities),
		Now:              o.now().UTC(),
	})
	if err != nil {
		observe.Error(ctx, "装配 Run 上下文快照失败", err)
		return errors.Join(err, o.fail(ctx, run, "context_unavailable", true))
	}
	if err := o.ports.Execution.SetRunContext(runContext, run, snapshot.Digest, snapshot.SourcesJSON()); err != nil {
		observe.Error(ctx, "固化 Run 上下文来源版本失败", err)
		// 取消优先：上下文已取消时不得落入重试/失败路径。
		if errors.Is(runContext.Err(), context.Canceled) {
			return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
		}
		return errors.Join(err, o.fail(ctx, run, "internal_error", true))
	}
	if err := emitEvent("run.context", map[string]any{
		"digest":           snapshot.Digest,
		"config_revision":  run.ConfigRevision,
		"history_count":    len(snapshot.History.Entries),
		"history_chars":    snapshot.History.TotalChars,
		"history_trimmed":  snapshot.History.Trimmed,
		"capability_count": len(capabilities),
	}); err != nil {
		// 事件持久化失败往往是取消导致的存储副作用，不得把取消落成失败/重试。
		if errors.Is(runContext.Err(), context.Canceled) {
			return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
		}
		return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", true))
	}
	observe.Info(ctx, "Run 上下文快照已装配",
		observe.StringAttr("context_digest", snapshot.Digest),
		observe.IntAttr("history_count", len(snapshot.History.Entries)),
		observe.IntAttr("history_trimmed", snapshot.History.Trimmed),
	)
	stream, err := o.executor.Run(runContext)
	if err != nil {
		observe.Error(ctx, "创建执行者会话流失败", err)
		runErr := fmt.Errorf("open executor stream: %w", err)
		return errors.Join(runErr, o.fail(ctx, run, "executor_unavailable", true))
	}
	defer stream.CloseSend()
	startFrame := &executor.Frame{
		EchoId:   echoID,
		RunId:    runID,
		Sequence: 1,
		Body: &executor.Frame_StartRun{StartRun: &executor.StartRun{
			AppId:              o.config.AppID,
			InputPayload:       &executor.Payload{ContentType: run.InputContentType, Data: append([]byte(nil), run.InputPayload...)},
			Capabilities:       capabilities,
			MaxSteps:           run.MaxSteps,
			ProtocolVersion:    executor.Version,
			MaxCapabilityCalls: run.MaxCapabilityCalls,
			MaxExecutionUnits:  run.MaxExecutionUnits,
			MaxOutputBytes:     run.MaxOutputBytes,
			MaxCostMicrousd:    run.MaxCostMicrousd,
			TraceId:            observe.String(ctx, "trace_id"),
			ParentSpanId:       observe.String(ctx, "span_id"),
			ParentRunId:        run.ParentRunID,
			ContextPayload:     &executor.Payload{ContentType: "application/ailuo.context+json", Data: append([]byte(nil), snapshot.ContextPayload...)},
			ExecutorConfig:     &executor.Payload{ContentType: "application/json", Data: append([]byte(nil), run.ExecutorConfig...)},
		}},
	}
	if err := executor.ValidateStartFrame(startFrame); err != nil {
		observe.Error(ctx, "Run 启动帧未通过本地协议校验", err)
		return errors.Join(err, o.fail(ctx, run, "protocol_violation", true))
	}
	if err := stream.Send(startFrame); err != nil {
		observe.Error(ctx, "发送 Run 输入失败", err)
		runErr := fmt.Errorf("start executor run: %w", err)
		return errors.Join(runErr, o.fail(ctx, run, "executor_start_failed", true))
	}
	if err := emitEvent("run.started", map[string]any{"run_id": runID, "attempt": run.Attempt}); err != nil {
		if errors.Is(runContext.Err(), context.Canceled) {
			return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
		}
		if errors.Is(runContext.Err(), context.DeadlineExceeded) {
			return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, Output{}, publicerror.Echo("deadline_exceeded")))
		}
		return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", true))
	}
	observe.Info(ctx, "Run 输入已经发送",
		observe.IntAttr("capability_count", len(capabilities)),
	)

	var finalOutput Output
	var terminalFailure *publicerror.Error
	var terminalRunErr error
	handshakeAccepted := false
	usageReported := false
	firstOutputObserved := false
	expectedExecutorSequence := run.LastExecutorSequence + 1
	kernelSequence := uint64(1)
	seenCallIDs := make(map[string]struct{})
	// Grant 的 MaxCalls 按 Capability 各自计数（见 authorize 的预算比较）；
	// Run 级总量由 MaxCapabilityCalls 与 seenCallIDs 单独约束。
	capabilityCallCounts := make(map[string]uint32)
	automaticRetrySafe := true
	outputBytes := uint64(0)
	addOutput := func(output Output) error {
		if uint64(len(output.Data)) > run.MaxOutputBytes-outputBytes {
			return ErrOutputBudgetExceeded
		}
		outputBytes += uint64(len(output.Data))
		return nil
	}
	for {
		frame, receiveErr := stream.Recv()
		if receiveErr != nil {
			if errors.Is(runContext.Err(), context.Canceled) {
				select {
				case renewalErr := <-leaseFailure:
					observe.Error(ctx, "Run 租约续期失败，已停止当前执行", renewalErr)
					return errors.Join(renewalErr, o.fail(ctx, run, "lease_lost", automaticRetrySafe))
				default:
				}
				observe.Warn(ctx, "Run 已取消", observe.Duration(runStarted))
				return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
			}
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				observe.Error(ctx, "Run 执行超时", context.DeadlineExceeded, observe.Duration(runStarted))
				return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, Output{}, publicerror.Echo("deadline_exceeded")))
			}
			if errors.Is(receiveErr, io.EOF) && len(finalOutput.Data) > 0 {
				if err := emitEvent("run.completed", outputEventPayload(finalOutput)); err != nil {
					return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", automaticRetrySafe))
				}
				if err := o.completeRun(ctx, run, RunStatusSucceeded, StatusSucceeded, finalOutput, publicerror.Error{}); err != nil {
					observe.Error(ctx, "持久化 Echo 成功终态失败", err)
					return err
				}
				observe.Info(ctx, "Run 执行完成",
					observe.IntAttr("output_bytes", len(finalOutput.Data)),
					observe.Duration(runStarted),
				)
				return nil
			}
			if errors.Is(receiveErr, io.EOF) && terminalFailure != nil {
				var eventErr error
				if !o.canRetry(run, *terminalFailure, automaticRetrySafe) {
					eventErr = emitEvent("run.failed", map[string]any{
						"code":      terminalFailure.Code,
						"message":   terminalFailure.Message,
						"retryable": terminalFailure.Retryable,
						"attempt":   run.Attempt,
					})
				}
				failErr := o.failPublic(ctx, run, *terminalFailure, automaticRetrySafe)
				return errors.Join(terminalRunErr, failErr, eventErr)
			}
			if errors.Is(receiveErr, io.EOF) {
				receiveErr = ErrNoFinalResult
			}
			observe.Error(ctx, "接收执行者事件失败", receiveErr, observe.Duration(runStarted))
			runErr := fmt.Errorf("receive executor frame: %w", receiveErr)
			return errors.Join(runErr, o.fail(ctx, run, "executor_stream_failed", automaticRetrySafe))
		}
		if len(finalOutput.Data) > 0 || terminalFailure != nil {
			err = executor.ErrUnexpectedFrame
			observe.Error(ctx, "执行者在终态帧后继续发送数据", err)
			return errors.Join(err, o.fail(ctx, run, "protocol_violation", automaticRetrySafe))
		}
		if err := executor.ValidateInboundEnvelope(frame, echoID, runID, expectedExecutorSequence); err != nil {
			observe.Error(ctx, "执行者帧信封违反协议", err,
				observe.Int64Attr("expected_sequence", int64(expectedExecutorSequence)),
			)
			return errors.Join(err, o.fail(ctx, run, "protocol_violation", automaticRetrySafe))
		}
		switch body := frame.Body.(type) {
		case *executor.Frame_RunAccepted:
			if handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateRunAccepted(frame)
		case *executor.Frame_CapabilityCall:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			if err = executor.ValidateCapabilityCall(body.CapabilityCall); err != nil {
				break
			}
			if len(seenCallIDs) >= int(run.MaxCapabilityCalls) {
				err = ErrCapabilityBudgetExceeded
				break
			}
			if _, exists := seenCallIDs[body.CapabilityCall.CallId]; exists {
				err = executor.ErrDuplicateCall
				break
			}
			seenCallIDs[body.CapabilityCall.CallId] = struct{}{}
		case *executor.Frame_OutputDelta:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateOutputDelta(body.OutputDelta)
		case *executor.Frame_FinalResult:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			if !usageReported {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateFinalResult(body.FinalResult)
		case *executor.Frame_RunFailure:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateRunFailure(body.RunFailure)
		case *executor.Frame_ResourceUsage:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateResourceUsage(
				body.ResourceUsage,
				run.UsedExecutionUnits,
				run.UsedCostMicrousd,
				run.UsedRetries,
				run.MaxExecutionUnits,
				run.MaxCostMicrousd,
			)
		default:
			err = executor.ErrUnexpectedFrame
		}
		if err != nil {
			observe.Error(ctx, "执行者帧载荷或顺序违反协议", err,
				observe.Int64Attr("executor_sequence", int64(frame.Sequence)),
			)
			failureCode := "protocol_violation"
			if errors.Is(err, ErrCapabilityBudgetExceeded) {
				failureCode = "budget_exceeded"
			}
			return errors.Join(err, o.fail(ctx, run, failureCode, automaticRetrySafe))
		}
		var sequenceErr error
		if usage := frame.GetResourceUsage(); usage != nil {
			sequenceErr = o.ports.Execution.AdvanceRunExecutorSequenceWithUsage(
				runContext,
				run,
				frame.Sequence,
				usage.ExecutionUnits,
				usage.CostMicrousd,
				usage.Retries,
			)
		} else {
			sequenceErr = o.ports.Execution.AdvanceRunExecutorSequence(runContext, run, frame.Sequence)
		}
		if sequenceErr != nil {
			if errors.Is(runContext.Err(), context.Canceled) {
				return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
			}
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, Output{}, publicerror.Echo("deadline_exceeded")))
			}
			observe.Error(ctx, "持久化执行者帧序号失败", sequenceErr,
				observe.Int64Attr("executor_sequence", int64(frame.Sequence)),
			)
			return errors.Join(sequenceErr, o.fail(ctx, run, "internal_error", automaticRetrySafe))
		}
		run.LastExecutorSequence = frame.Sequence
		var executionUnitDelta uint64
		var costDelta uint64
		var retryDelta uint32
		if usage := frame.GetResourceUsage(); usage != nil {
			executionUnitDelta = usage.ExecutionUnits - run.UsedExecutionUnits
			costDelta = usage.CostMicrousd - run.UsedCostMicrousd
			retryDelta = usage.Retries - run.UsedRetries
			run.UsedExecutionUnits = usage.ExecutionUnits
			run.UsedCostMicrousd = usage.CostMicrousd
			run.UsedRetries = usage.Retries
		}
		expectedExecutorSequence++

		switch body := frame.Body.(type) {
		case *executor.Frame_RunAccepted:
			handshakeAccepted = true
			observe.Info(ctx, "执行者协议版本握手完成",
				observe.StringAttr("protocol_version", body.RunAccepted.ProtocolVersion),
			)
		case *executor.Frame_CapabilityCall:
			if spec, _, resolveErr := o.registry.ResolveCapability(body.CapabilityCall.CapabilityId); resolveErr == nil &&
				spec.Execution.Replay != capability.ReplaySafe {
				automaticRetrySafe = false
			}
			observe.Info(ctx, "执行者请求调用 Capability",
				observe.StringAttr("call_id", body.CapabilityCall.CallId),
				observe.StringAttr("capability_id", body.CapabilityCall.CapabilityId),
				observe.IntAttr("argument_bytes", len(body.CapabilityCall.PayloadJson)),
			)
			result := o.invokeCapability(runContext, run, body.CapabilityCall, capabilityCallCounts[body.CapabilityCall.CapabilityId])
			capabilityCallCounts[body.CapabilityCall.CapabilityId]++
			if errors.Is(runContext.Err(), context.Canceled) {
				return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
			}
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, Output{}, publicerror.Echo("deadline_exceeded")))
			}
			kernelSequence++
			resultFrame := &executor.Frame{
				EchoId:   echoID,
				RunId:    runID,
				Sequence: kernelSequence,
				Body:     &executor.Frame_CapabilityResult{CapabilityResult: result},
			}
			if err := executor.ValidateCapabilityResultFrame(resultFrame, echoID, runID, kernelSequence); err != nil {
				observe.Error(ctx, "CapabilityResult 未通过本地协议校验", err,
					observe.StringAttr("call_id", result.CallId),
				)
				return errors.Join(err, o.fail(ctx, run, "protocol_violation", automaticRetrySafe))
			}
			if err := stream.Send(resultFrame); err != nil {
				observe.Error(ctx, "向执行者返回 Capability 结果失败", err,
					observe.StringAttr("call_id", result.CallId),
				)
				// 取消优先于流错误：上下文已取消时落 cancelled 终态，避免二次失败篡改终态。
				if errors.Is(runContext.Err(), context.Canceled) {
					return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
				}
				runErr := fmt.Errorf("send capability result: %w", err)
				return errors.Join(runErr, o.fail(ctx, run, "executor_stream_failed", automaticRetrySafe))
			}
			if err := emitEvent("capability.completed", map[string]any{
				"call_id":       result.CallId,
				"capability_id": result.CapabilityId,
				"success":       result.Success,
			}); err != nil {
				// 取消优先于事件持久化失败：追加失败往往是取消导致的存储副作用。
				if errors.Is(runContext.Err(), context.Canceled) {
					return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
				}
				return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", automaticRetrySafe))
			}
		case *executor.Frame_OutputDelta:
			if !firstOutputObserved {
				observe.DefaultMetrics().ObserveFirstOutput(time.Since(runStarted))
				firstOutputObserved = true
			}
			output := outputFromPayload(body.OutputDelta.Payload)
			if err := addOutput(output); err != nil {
				observe.Warn(ctx, "执行者输出超过 Run 预算", observe.Int64Attr("output_bytes", int64(outputBytes)))
				return errors.Join(err, o.fail(ctx, run, "budget_exceeded", false))
			}
			observe.Debug(ctx, "收到执行者输出片段",
				observe.IntAttr("output_bytes", len(output.Data)),
			)
			if err := emitEvent("output.delta", outputEventPayload(output)); err != nil {
				if errors.Is(runContext.Err(), context.Canceled) {
					return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, Output{}, publicerror.Echo("cancelled")))
				}
				return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", automaticRetrySafe))
			}
		case *executor.Frame_FinalResult:
			if !firstOutputObserved {
				observe.DefaultMetrics().ObserveFirstOutput(time.Since(runStarted))
				firstOutputObserved = true
			}
			finalOutput = outputFromPayload(body.FinalResult.Payload)
			if uint64(len(finalOutput.Data)) > run.MaxOutputBytes {
				observe.Warn(ctx, "执行者最终输出超过 Run 预算", observe.Int64Attr("output_bytes", int64(outputBytes)))
				return errors.Join(ErrOutputBudgetExceeded, o.fail(ctx, run, "budget_exceeded", false))
			}
		case *executor.Frame_RunFailure:
			public := publicerror.Executor(body.RunFailure.Code, body.RunFailure.Retryable)
			runErr := fmt.Errorf("%w: code=%s", ErrExecutorRunFailed, public.Code)
			observe.Error(ctx, "执行者报告运行失败", runErr,
				observe.StringAttr("error_code", public.Code),
				observe.BoolAttr("retryable", public.Retryable),
				observe.Duration(runStarted),
			)
			terminalFailure = &public
			terminalRunErr = runErr
		case *executor.Frame_ResourceUsage:
			observe.DefaultMetrics().AddExecutorUsage(
				executionUnitDelta,
				costDelta,
			)
			for retry := uint32(0); retry < retryDelta; retry++ {
				observe.DefaultMetrics().ExecutorRetry()
			}
			observe.Info(ctx, "已记录执行者用量",
				observe.Int64Attr("execution_units", int64(body.ResourceUsage.ExecutionUnits)),
				observe.Int64Attr("cost_microusd", int64(body.ResourceUsage.CostMicrousd)),
			)
			usageReported = true
		}
	}
}

func (o *Orchestrator) projectCapabilities(policy appconfig.PolicySnapshot, run RunRecord) []*executor.Capability {
	all := o.registry.Capabilities()
	projected := make([]*executor.Capability, 0, len(all))
	scope := make(map[string]struct{}, len(run.CapabilityGrants))
	for _, grant := range run.CapabilityGrants {
		scope[grant.CapabilityID] = struct{}{}
	}
	for _, capability := range all {
		if !policy.HasCapability(capability.ID) {
			continue
		}
		if _, enabled := scope[capability.ID]; !enabled {
			continue
		}
		projected = append(projected, &executor.Capability{
			Id:              capability.ID,
			Version:         capability.Version,
			Name:            capability.Name,
			Description:     capability.Description,
			InputSchemaJson: capability.InputSchemaJSON,
		})
	}
	return projected
}

// capabilityVersions 把当前投影的 Capability 转换为 "id@version" 列表，
// 作为上下文装配的 Capability 来源（装配器内排序后固化版本）。
func capabilityVersions(capabilities []*executor.Capability) []string {
	versions := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		versions = append(versions, capability.Id+"@"+capability.Version)
	}
	return versions
}

func (o *Orchestrator) invokeCapability(ctx context.Context, run RunRecord, call *executor.CapabilityCall, callsUsed uint32) *executor.CapabilityResult {
	runID := run.ID
	if call == nil || call.CallId == "" || len(call.CallId) > 128 || idempotency.ValidateKey(call.CallId) != nil {
		public := publicerror.Executor("protocol_violation", false)
		return &executor.CapabilityResult{
			CallId:       call.GetCallId(),
			CapabilityId: call.GetCapabilityId(),
			ErrorCode:    public.Code,
			ErrorMessage: public.Message,
		}
	}
	encoded, replayed, err := o.idempotency.Execute(ctx, idempotency.Operation{
		AppID:       o.config.AppID,
		Scope:       "executor.call/" + runID,
		Key:         call.CallId,
		Fingerprint: idempotency.Fingerprint([]byte(runID), []byte(call.CapabilityId), call.PayloadJson),
		OwnerID:     runID + ":" + call.CallId,
	}, func(executionContext context.Context) ([]byte, error) {
		result := o.invokeCapabilityOnce(executionContext, run, call, callsUsed)
		return proto.Marshal(result)
	})
	if err != nil {
		public := publicerror.Capability(err)
		observe.Warn(ctx, "执行者 CapabilityCall 幂等治理失败",
			observe.StringAttr("call_id", call.CallId),
			observe.StringAttr("capability_id", call.CapabilityId),
			observe.StringAttr("error_code", public.Code),
		)
		return &executor.CapabilityResult{
			CallId:       call.CallId,
			CapabilityId: call.CapabilityId,
			ErrorCode:    public.Code,
			ErrorMessage: public.Message,
		}
	}
	var result executor.CapabilityResult
	if err := proto.Unmarshal(encoded, &result); err != nil {
		public := publicerror.Echo("protocol_violation")
		observe.Error(ctx, "读取持久化 CapabilityCall 幂等结果失败", err,
			observe.StringAttr("call_id", call.CallId),
			observe.StringAttr("capability_id", call.CapabilityId),
		)
		return &executor.CapabilityResult{
			CallId:       call.CallId,
			CapabilityId: call.CapabilityId,
			ErrorCode:    public.Code,
			ErrorMessage: public.Message,
		}
	}
	if replayed {
		observe.Info(ctx, "已复用持久化的 CapabilityCall 结果",
			observe.StringAttr("call_id", call.CallId),
			observe.StringAttr("capability_id", call.CapabilityId),
		)
	}
	return &result
}

func (o *Orchestrator) invokeCapabilityOnce(ctx context.Context, run RunRecord, call *executor.CapabilityCall, callsUsed uint32) *executor.CapabilityResult {
	started := o.now()
	echoID := run.EchoID
	runID := run.ID
	ctx = observe.With(ctx,
		observe.StringAttr("call_id", call.CallId),
		observe.StringAttr("capability_id", call.CapabilityId),
	)
	requestID := uuid.NewString()
	ctx = observe.With(ctx, observe.StringAttr("request_id", requestID))
	policy, err := o.policy.Snapshot(ctx, o.config.AppID)
	if err != nil {
		public := publicerror.Capability(errors.Join(runtime.ErrAppPolicyUnavailable, err))
		return &executor.CapabilityResult{
			CallId: call.CallId, CapabilityId: call.CapabilityId,
			ErrorCode: public.Code, ErrorMessage: public.Message,
		}
	}
	if err := policy.Verify(o.config.AppID); err != nil {
		public := publicerror.Capability(errors.Join(runtime.ErrAppPolicyUnavailable, err))
		return &executor.CapabilityResult{
			CallId: call.CallId, CapabilityId: call.CapabilityId,
			ErrorCode: public.Code, ErrorMessage: public.Message,
		}
	}
	if !policy.Enabled {
		public := publicerror.Capability(runtime.ErrCapabilityDisabled)
		return &executor.CapabilityResult{
			CallId: call.CallId, CapabilityId: call.CapabilityId,
			ErrorCode: public.Code, ErrorMessage: public.Message,
		}
	}
	if _, allowed := capabilityGrantIDs(run.CapabilityGrants)[call.CapabilityId]; !allowed {
		return o.rejectedCapability(ctx, run, call, started, runtime.ErrCapabilityDisabled)
	}
	deadline := started.Add(30 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	traceID := observe.String(ctx, "trace_id")
	request := contracts.RequestContext{
		AppID:               o.config.AppID,
		EchoID:              echoID,
		RequestID:           requestID,
		TraceID:             traceID,
		UserID:              run.UserID,
		SessionID:           run.SessionID,
		RunID:               runID,
		ParentRunID:         run.ParentRunID,
		CallID:              call.CallId,
		LeaseToken:          run.LeaseToken,
		Deadline:            deadline,
		IdempotencyKey:      call.CallId,
		ProtocolVersion:     executor.Version,
		CapabilityCallsUsed: callsUsed,
		CapabilityCostUsed:  run.UsedCostMicrousd,
	}
	payload, err := o.dispatcher.InvokeCapability(ctx, request, call.CapabilityId, call.PayloadJson)
	result := &executor.CapabilityResult{
		CallId:       call.CallId,
		CapabilityId: call.CapabilityId,
		Success:      err == nil,
		PayloadJson:  payload,
	}
	var capabilityFailure publicerror.Error
	if err != nil {
		capabilityFailure = publicerror.Capability(err)
		result.ErrorCode = capabilityFailure.Code
		result.ErrorMessage = capabilityFailure.Message
	}
	auditPayload := observe.SanitizeAuditJSON(call.PayloadJson, 8192)
	auditContext, auditCancel := detachedContext(ctx)
	auditErr := o.ports.Audit.RecordCapabilityCall(auditContext, call.CallId, runID, echoID, o.config.AppID, call.CapabilityId, auditPayload, result.Success, capabilityFailure, o.now().Sub(started))
	auditCancel()
	if auditErr != nil && err == nil {
		public := publicerror.Echo("internal_error")
		result.Success = false
		result.ErrorCode = public.Code
		result.ErrorMessage = public.Message
		result.PayloadJson = nil
		observe.Error(ctx, "持久化 Capability 审计记录失败", auditErr)
	}
	if result.Success {
		observe.Info(ctx, "Capability 调用完成",
			observe.StringAttr("request_id", requestID),
			observe.IntAttr("result_bytes", len(result.PayloadJson)),
			observe.Duration(started),
		)
	} else {
		observe.Warn(ctx, "Capability 调用失败",
			observe.StringAttr("request_id", requestID),
			observe.StringAttr("error_code", result.ErrorCode),
			observe.StringAttr("error", result.ErrorMessage),
			observe.Duration(started),
		)
	}
	return result
}

func capabilityGrantIDs(grants []capability.Grant) map[string]struct{} {
	ids := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		ids[grant.CapabilityID] = struct{}{}
	}
	return ids
}

func (o *Orchestrator) rejectedCapability(ctx context.Context, run RunRecord, call *executor.CapabilityCall, started time.Time, cause error) *executor.CapabilityResult {
	public := publicerror.Capability(cause)
	result := &executor.CapabilityResult{
		CallId: call.CallId, CapabilityId: call.CapabilityId,
		ErrorCode: public.Code, ErrorMessage: public.Message,
	}
	auditPayload := observe.SanitizeAuditJSON(call.PayloadJson, 8192)
	auditContext, auditCancel := detachedContext(ctx)
	auditErr := o.ports.Audit.RecordCapabilityCall(
		auditContext, call.CallId, run.ID, run.EchoID, run.AppID, call.CapabilityId,
		auditPayload, false, public, o.now().Sub(started),
	)
	auditCancel()
	if auditErr != nil {
		observe.Error(ctx, "持久化被拒绝 Capability 的审计记录失败", auditErr)
	}
	observe.Warn(ctx, "Run 请求了未投影、越权或已撤权的 Capability",
		observe.StringAttr("error_code", public.Code),
		observe.Duration(started),
	)
	return result
}

func (o *Orchestrator) fail(ctx context.Context, run RunRecord, code string, automaticRetrySafe bool) error {
	public := publicerror.Echo(code)
	return o.failPublic(ctx, run, public, automaticRetrySafe)
}

func (o *Orchestrator) failPublic(ctx context.Context, run RunRecord, public publicerror.Error, automaticRetrySafe bool) error {
	if o.canRetry(run, public, automaticRetrySafe) {
		if err := o.retryRun(ctx, run, public); err == nil {
			return ErrRunRetryScheduled
		} else {
			observe.Error(ctx, "持久化 Run 自动重试失败", err,
				observe.StringAttr("echo_id", run.EchoID),
				observe.StringAttr("run_id", run.ID),
				observe.StringAttr("error_code", public.Code),
			)
			terminalErr := o.completeRun(ctx, run, RunStatusFailed, StatusFailed, Output{}, public)
			return errors.Join(err, terminalErr)
		}
	}
	storeErr := o.completeRun(ctx, run, RunStatusFailed, StatusFailed, Output{}, public)
	if storeErr != nil {
		observe.Error(ctx, "持久化 Run/Echo 失败终态失败", storeErr,
			observe.StringAttr("echo_id", run.EchoID),
			observe.StringAttr("run_id", run.ID),
			observe.StringAttr("error_code", public.Code),
		)
	}
	return storeErr
}

func (o *Orchestrator) canRetry(run RunRecord, public publicerror.Error, automaticRetrySafe bool) bool {
	return public.Retryable && automaticRetrySafe && run.Attempt < o.config.MaxRunAttempts
}

func (o *Orchestrator) retryRun(ctx context.Context, run RunRecord, failure publicerror.Error) error {
	// 重试是"全新 attempt"语义：新 run.ID 下预算（units/cost/retries）与
	// Capability 调用幂等 scope（executor.call/<runID>）都从零开始，attempt
	// 数由 canRetry 的 MaxRunAttempts 封顶。这是有意设计——跨 attempt 累积
	// 预算会把 RunGroup 变成无上限的长生命周期能力预算池。
	completedAt := o.now().UTC()
	availableAt := completedAt.Add(o.retryDelay(run))
	next := run
	next.ID = uuid.NewString()
	next.Attempt++
	next.Status = RunStatusQueued
	next.Deadline = availableAt.Add(o.config.RunTimeout)
	next.AvailableAt = availableAt
	next.LeaseToken = ""
	next.LeaseExpiresAt = nil
	next.LastExecutorSequence = 0
	next.RecoverableState = json.RawMessage(`{}`)
	next.ContextDigest = ""
	next.ContextSources = json.RawMessage(`{}`)
	next.ErrorCode = ""
	next.ErrorMessage = ""
	next.CreatedAt = completedAt
	next.StartedAt = nil
	next.CompletedAt = nil
	next.UsedExecutionUnits = 0
	next.UsedCostMicrousd = 0
	next.UsedRetries = 0
	finishContext, cancel := detachedContext(ctx)
	defer cancel()
	if err := o.ports.Execution.RetryRun(finishContext, run, next, failure, completedAt); err != nil {
		return err
	}
	startedAt := run.CreatedAt
	if run.StartedAt != nil {
		startedAt = *run.StartedAt
	}
	observe.DefaultMetrics().ObserveRun(RunStatusFailed, completedAt.Sub(startedAt))
	observe.DefaultMetrics().RunRetry()
	observe.DefaultMetrics().QueueAdded()
	observe.Info(ctx, "已持久化安排 Run 自动重试",
		observe.StringAttr("run_id", next.ID),
		observe.IntAttr("attempt", int(next.Attempt)),
		observe.Int64Attr("available_unix_ms", availableAt.UnixMilli()),
		observe.StringAttr("error_code", failure.Code),
	)
	return nil
}

func (o *Orchestrator) retryDelay(run RunRecord) time.Duration {
	delay := o.config.RetryBaseDelay
	for attempt := uint32(1); attempt < run.Attempt && delay < o.config.RetryMaxDelay; attempt++ {
		delay *= 2
		if delay > o.config.RetryMaxDelay {
			delay = o.config.RetryMaxDelay
		}
	}
	sum := sha256.Sum256([]byte(run.ID))
	jitterPermille := 800 + int(sum[0])*400/255
	return time.Duration(int64(delay) * int64(jitterPermille) / 1000)
}

func (o *Orchestrator) renewLease(ctx context.Context, cancel context.CancelFunc, run RunRecord, failure chan<- error) {
	interval := o.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	renewBudget := o.config.LeaseDuration + interval
	if renewBudget < o.config.LeaseDuration {
		renewBudget = o.config.LeaseDuration
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case renewedAt := <-ticker.C:
			// 续期预算取完整租约窗口（而非 1/3）：续期只需在租约到期前完成，
			// 1/3 窗口在负载下会把瞬时存储慢写误判为续期失败并取消整个 Run。
			// 每次续期把租约延长到 renewedAt+LeaseDuration，慢续期不丢所有权。
			renewContext, renewCancel := context.WithTimeout(ctx, renewBudget)
			err := o.ports.Execution.RenewRunLease(renewContext, run, renewedAt.UTC(), renewedAt.UTC().Add(o.config.LeaseDuration))
			renewCancel()
			if err != nil {
				select {
				case failure <- err:
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (o *Orchestrator) completeRun(ctx context.Context, run RunRecord, runStatus, echoStatus string, output Output, public publicerror.Error) error {
	finishContext, cancel := detachedContext(ctx)
	defer cancel()
	completedAt := o.now().UTC()
	err := o.ports.Execution.CompleteRun(finishContext, run, runStatus, echoStatus, output, public, completedAt)
	if err == nil {
		startedAt := run.CreatedAt
		if run.StartedAt != nil {
			startedAt = *run.StartedAt
		}
		observe.DefaultMetrics().ObserveRun(runStatus, completedAt.Sub(startedAt))
		if runStatus == RunStatusCancelled {
			observe.DefaultMetrics().Cancellation()
		}
	}
	return err
}

func outputFromPayload(payload *executor.Payload) Output {
	if payload == nil {
		return Output{}
	}
	return Output{ContentType: payload.ContentType, Data: append([]byte(nil), payload.Data...)}
}

func outputEventPayload(output Output) map[string]any {
	return map[string]any{
		"content_type": output.ContentType,
		"data":         output.Data,
	}
}

func detachedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
