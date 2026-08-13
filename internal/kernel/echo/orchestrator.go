package echo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

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
	ErrEmptyMessage         = errors.New("message is required")
	ErrAgentRunFailed       = errors.New("agent run failed")
	ErrNoFinalMessage       = errors.New("agent stream ended without final message")
	ErrRunInputMismatch     = errors.New("run input does not match persisted Echo")
	ErrRunConfigUnavailable = errors.New("persisted run configuration is unavailable")
	ErrMessageTooLong       = errors.New("message exceeds the maximum length")
	ErrAppConfigUnavailable = errors.New("app configuration is unavailable")
	ErrAppDisabled          = errors.New("app is disabled")
	ErrInvalidChildRun      = errors.New("invalid child run request")
	ErrChildRunUnavailable  = errors.New("child run is unavailable")
)

type EventEmitter func(Event) error

type Config struct {
	AppID              string
	Model              string
	SystemPrompt       string
	Timezone           string
	MaxSteps           uint32
	MaxToolCalls       uint32
	MaxInputTokens     uint64
	MaxOutputTokens    uint64
	MaxTotalTokens     uint64
	MaxOutputBytes     uint64
	MaxCostMicrousd    uint64
	ProviderTimeout    time.Duration
	RunTimeout         time.Duration
	LeaseDuration      time.Duration
	MaxRunAttempts     uint32
	RetryBaseDelay     time.Duration
	RetryMaxDelay      time.Duration
	QueueCapacity      int
	ModelConfigVersion string
	PermissionScope    []string
	AppConfigSource    appconfig.Source
	Context            contextasm.HistorySource
	ContextBudget      contextasm.Budget
}

type Orchestrator struct {
	agent       executor.Client
	registry    *registry.Registry
	dispatcher  *runtime.Dispatcher
	policy      runtime.AppPolicy
	store       Store
	idempotency *idempotency.Manager
	context     *contextasm.Assembler
	config      Config
	now         func() time.Time
}

func NewOrchestrator(
	agent executor.Client,
	reg *registry.Registry,
	dispatcher *runtime.Dispatcher,
	policy runtime.AppPolicy,
	store Store,
	config Config,
) *Orchestrator {
	if config.MaxSteps == 0 {
		config.MaxSteps = 8
	}
	if config.MaxToolCalls == 0 {
		config.MaxToolCalls = 8
	}
	if config.MaxInputTokens == 0 {
		config.MaxInputTokens = 32768
	}
	if config.MaxOutputTokens == 0 {
		config.MaxOutputTokens = 8192
	}
	if config.MaxTotalTokens == 0 {
		config.MaxTotalTokens = 40960
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = executor.MaxFinalMessageBytes
	}
	if config.ProviderTimeout == 0 {
		config.ProviderTimeout = 30 * time.Second
	}
	if config.RunTimeout == 0 {
		config.RunTimeout = 90 * time.Second
	}
	if config.LeaseDuration == 0 {
		config.LeaseDuration = 15 * time.Second
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
	config.PermissionScope = append([]string(nil), config.PermissionScope...)
	sort.Strings(config.PermissionScope)
	if config.ModelConfigVersion == "" {
		configBytes, _ := json.Marshal(struct {
			Model           string
			SystemPrompt    string
			Timezone        string
			MaxSteps        uint32
			MaxToolCalls    uint32
			MaxInputTokens  uint64
			MaxOutputTokens uint64
			MaxTotalTokens  uint64
			MaxOutputBytes  uint64
			MaxCostMicrousd uint64
			ProviderTimeout time.Duration
			LeaseDuration   time.Duration
			MaxRunAttempts  uint32
			RetryBaseDelay  time.Duration
			RetryMaxDelay   time.Duration
			PermissionScope []string
		}{
			Model:           config.Model,
			SystemPrompt:    config.SystemPrompt,
			Timezone:        config.Timezone,
			MaxSteps:        config.MaxSteps,
			MaxToolCalls:    config.MaxToolCalls,
			MaxInputTokens:  config.MaxInputTokens,
			MaxOutputTokens: config.MaxOutputTokens,
			MaxTotalTokens:  config.MaxTotalTokens,
			MaxOutputBytes:  config.MaxOutputBytes,
			MaxCostMicrousd: config.MaxCostMicrousd,
			ProviderTimeout: config.ProviderTimeout,
			LeaseDuration:   config.LeaseDuration,
			MaxRunAttempts:  config.MaxRunAttempts,
			RetryBaseDelay:  config.RetryBaseDelay,
			RetryMaxDelay:   config.RetryMaxDelay,
			PermissionScope: config.PermissionScope,
		})
		config.ModelConfigVersion = fmt.Sprintf("%x", sha256.Sum256(configBytes))
	}
	assembler, err := contextasm.New(config.Context, config.ContextBudget)
	if err != nil {
		// 装配期编程错误：上下文来源缺失或预算非法必须显式终止，不做静默降级。
		panic(fmt.Sprintf("orchestrator context assembly misconfigured: %v", err))
	}
	return &Orchestrator{
		agent:       agent,
		registry:    reg,
		dispatcher:  dispatcher,
		policy:      policy,
		store:       store,
		idempotency: idempotency.NewManager(store),
		context:     assembler,
		config:      config,
		now:         time.Now,
	}
}

func (o *Orchestrator) currentAppConfig(ctx context.Context) (appconfig.Config, error) {
	if o.config.AppConfigSource != nil {
		config, err := o.config.AppConfigSource.Current(ctx, o.config.AppID)
		if err != nil {
			return appconfig.Config{}, err
		}
		if err := appconfig.VerifyCurrent(config, o.config.AppID); err != nil {
			return appconfig.Config{}, err
		}
		return config, nil
	}
	return o.fallbackAppConfig(), nil
}

func (o *Orchestrator) appConfigRevision(ctx context.Context, revision string) (appconfig.Config, error) {
	if o.config.AppConfigSource != nil {
		config, err := o.config.AppConfigSource.Revision(ctx, o.config.AppID, revision)
		if err != nil {
			return appconfig.Config{}, err
		}
		if err := appconfig.Verify(config, o.config.AppID, revision); err != nil {
			return appconfig.Config{}, err
		}
		return config, nil
	}
	config := o.fallbackAppConfig()
	if config.Revision != revision {
		return appconfig.Config{}, appconfig.ErrNotFound
	}
	return config, nil
}

func (o *Orchestrator) fallbackAppConfig() appconfig.Config {
	return appconfig.Config{
		AppID: o.config.AppID, Revision: o.config.ModelConfigVersion, Generation: 1, Enabled: true,
		Model: o.config.Model, SystemPrompt: o.config.SystemPrompt, Timezone: o.config.Timezone,
		MaxSteps: o.config.MaxSteps, MaxToolCalls: o.config.MaxToolCalls,
		MaxInputTokens: o.config.MaxInputTokens, MaxOutputTokens: o.config.MaxOutputTokens,
		MaxTotalTokens: o.config.MaxTotalTokens, MaxOutputBytes: o.config.MaxOutputBytes,
		MaxCostMicrousd: o.config.MaxCostMicrousd, ProviderTimeout: o.config.ProviderTimeout,
		PermissionScope: append([]string(nil), o.config.PermissionScope...),
	}
}

func runMatchesAppConfig(run RunRecord, config appconfig.Config) bool {
	if run.ModelConfigVersion != config.Revision || run.Model != config.Model {
		return false
	}
	if run.ParentRunID == "" {
		return run.MaxSteps == config.MaxSteps && run.MaxToolCalls == config.MaxToolCalls &&
			run.MaxInputTokens == config.MaxInputTokens && run.MaxOutputTokens == config.MaxOutputTokens &&
			run.MaxTotalTokens == config.MaxTotalTokens && run.MaxOutputBytes == config.MaxOutputBytes &&
			run.MaxCostMicrousd == config.MaxCostMicrousd &&
			run.ProviderTimeoutMS == uint32(config.ProviderTimeout.Milliseconds())
	}
	return run.MaxSteps > 0 && run.MaxSteps <= config.MaxSteps &&
		run.MaxToolCalls > 0 && run.MaxToolCalls <= config.MaxToolCalls &&
		run.MaxInputTokens > 0 && run.MaxInputTokens <= config.MaxInputTokens &&
		run.MaxOutputTokens > 0 && run.MaxOutputTokens <= config.MaxOutputTokens &&
		run.MaxTotalTokens > 0 && run.MaxTotalTokens <= config.MaxTotalTokens &&
		run.MaxOutputBytes > 0 && run.MaxOutputBytes <= config.MaxOutputBytes &&
		run.MaxCostMicrousd <= config.MaxCostMicrousd &&
		run.ProviderTimeoutMS >= 100 && run.ProviderTimeoutMS <= uint32(config.ProviderTimeout.Milliseconds())
}

func (o *Orchestrator) Run(ctx context.Context, request RunRequest, emit EventEmitter) (string, error) {
	echoID, created, err := o.CreateIdempotent(ctx, request)
	if err != nil {
		return "", err
	}
	if !created {
		return echoID, nil
	}
	return echoID, o.RunExisting(ctx, echoID, request, emit)
}

func (o *Orchestrator) RunChild(ctx context.Context, request ChildRunRequest) (ChildRunResult, error) {
	if request.ParentRunID == "" || idempotency.ValidateKey(request.OriginCallID) != nil ||
		request.Task == "" || strings.TrimSpace(request.Task) == "" || strings.ContainsRune(request.Task, '\x00') ||
		!utf8.ValidString(request.Task) || utf8.RuneCountInString(request.Task) > 4000 ||
		len(request.CapabilityScope) > 16 {
		return ChildRunResult{}, ErrInvalidChildRun
	}
	parent, err := o.store.GetRun(ctx, o.config.AppID, request.ParentRunID)
	if err != nil {
		return ChildRunResult{}, errors.Join(ErrChildRunUnavailable, err)
	}
	if parent.ParentRunID != "" || parent.Status != RunStatusRunning || parent.LeaseToken == "" {
		return ChildRunResult{}, ErrChildRunUnavailable
	}
	app, err := o.currentAppConfig(ctx)
	if err != nil {
		return ChildRunResult{}, errors.Join(ErrAppConfigUnavailable, err)
	}
	if !app.Enabled {
		return ChildRunResult{}, ErrAppDisabled
	}
	policy, err := o.policy.Snapshot(ctx, o.config.AppID)
	if err != nil {
		return ChildRunResult{}, errors.Join(ErrAppConfigUnavailable, err)
	}
	if err := policy.Verify(o.config.AppID); err != nil {
		return ChildRunResult{}, errors.Join(ErrAppConfigUnavailable, err)
	}
	if !policy.Enabled {
		return ChildRunResult{}, ErrAppDisabled
	}
	capabilityScope, permissionScope, err := o.childScopes(policy, parent, request.CapabilityScope)
	if err != nil {
		return ChildRunResult{}, errors.Join(ErrInvalidChildRun, err)
	}
	now := o.now().UTC()
	remaining := parent.Deadline.Sub(now)
	if remaining < 200*time.Millisecond {
		return ChildRunResult{}, context.DeadlineExceeded
	}
	childDuration := remaining / 2
	childDeadline := now.Add(childDuration)
	providerTimeoutMS := halfAtLeastOne(uint64(parent.ProviderTimeoutMS))
	if maximum := uint64(childDuration.Milliseconds()); providerTimeoutMS > maximum {
		providerTimeoutMS = maximum
	}
	if providerTimeoutMS < 100 {
		providerTimeoutMS = 100
	}
	childID := uuid.NewString()
	child := RunRecord{
		ID:                 childID,
		RunGroupID:         childID,
		AppID:              parent.AppID,
		EchoID:             parent.EchoID,
		ParentRunID:        parent.ID,
		OriginCallID:       request.OriginCallID,
		Attempt:            1,
		Status:             RunStatusQueued,
		Model:              parent.Model,
		ModelConfigVersion: parent.ModelConfigVersion,
		ProtocolVersion:    parent.ProtocolVersion,
		MaxSteps:           uint32(halfAtLeastOne(uint64(parent.MaxSteps))),
		MaxToolCalls:       uint32(halfAtLeastOne(uint64(parent.MaxToolCalls))),
		MaxInputTokens:     halfAtLeastOne(parent.MaxInputTokens),
		MaxOutputTokens:    halfAtLeastOne(parent.MaxOutputTokens),
		MaxTotalTokens:     halfAtLeastOne(parent.MaxTotalTokens),
		MaxOutputBytes:     halfAtLeastOne(parent.MaxOutputBytes),
		MaxCostMicrousd:    halfUnlessUnlimited(parent.MaxCostMicrousd),
		ProviderTimeoutMS:  uint32(providerTimeoutMS),
		Deadline:           childDeadline,
		AvailableAt:        now,
		CapabilityScope:    capabilityScope,
		PermissionScope:    permissionScope,
		RecoverableState:   json.RawMessage(`{}`),
		CreatedAt:          now,
	}
	if err := o.store.CreateChildRun(ctx, parent, child); err != nil {
		return ChildRunResult{}, errors.Join(ErrChildRunUnavailable, err)
	}
	leaseToken := uuid.NewString()
	claimed, err := o.store.ClaimChildRun(ctx, child.AppID, child.EchoID, child.ID, parent.ID, leaseToken, now, now.Add(o.config.LeaseDuration))
	if err != nil {
		persisted, readErr := o.store.GetRun(ctx, child.AppID, child.ID)
		if readErr == nil && persisted.Status == RunStatusRunning && persisted.LeaseToken == leaseToken {
			claimed = persisted
			err = nil
		}
	}
	if err != nil {
		cleanupContext, cleanupCancel := detachedContext(ctx)
		cleanupErr := o.store.FailQueuedChildRun(cleanupContext, child, publicerror.Echo("recovery_failed"), o.now().UTC())
		cleanupCancel()
		return ChildRunResult{}, errors.Join(ErrChildRunUnavailable, err, cleanupErr)
	}
	result := ""
	runErr := o.executeClaimedRun(ctx, RunRequest{Message: request.Task}, nil, claimed, now, &result)
	if runErr != nil {
		return ChildRunResult{}, runErr
	}
	return ChildRunResult{RunID: child.ID, Result: result}, nil
}

func (o *Orchestrator) childScopes(policy appconfig.PolicySnapshot, parent RunRecord, requested []string) ([]string, []string, error) {
	if len(requested) == 0 {
		for _, capability := range o.projectCapabilities(policy, parent) {
			if capability.Id != SubagentCapabilityID {
				requested = append(requested, capability.Id)
			}
		}
	}
	if len(requested) > 16 {
		return nil, nil, ErrInvalidChildRun
	}
	requested = append([]string(nil), requested...)
	sort.Strings(requested)
	permissions := make(map[string]struct{})
	for index, capabilityID := range requested {
		if capabilityID == SubagentCapabilityID || (index > 0 && requested[index-1] == capabilityID) ||
			!policy.CapabilityEnabled(capabilityID) {
			return nil, nil, registry.ErrPermissionDenied
		}
		parentIndex := sort.SearchStrings(parent.CapabilityScope, capabilityID)
		if parentIndex >= len(parent.CapabilityScope) || parent.CapabilityScope[parentIndex] != capabilityID {
			return nil, nil, registry.ErrPermissionDenied
		}
		spec, _, err := o.registry.ResolveCapability(capabilityID)
		if err != nil {
			return nil, nil, err
		}
		if _, err := registry.NarrowPermissions(policy.PermissionScope, spec.RequiredPermissions); err != nil {
			return nil, nil, err
		}
		if _, err := registry.NarrowPermissions(parent.PermissionScope, spec.RequiredPermissions); err != nil {
			return nil, nil, err
		}
		for _, permission := range spec.RequiredPermissions {
			permissions[permission] = struct{}{}
		}
	}
	permissionScope := make([]string, 0, len(permissions))
	for permission := range permissions {
		permissionScope = append(permissionScope, permission)
	}
	sort.Strings(permissionScope)
	return requested, permissionScope, nil
}

func halfAtLeastOne(value uint64) uint64 {
	if value <= 1 {
		return 1
	}
	return value / 2
}

func halfUnlessUnlimited(value uint64) uint64 {
	if value == 0 {
		return 0
	}
	return halfAtLeastOne(value)
}

func (o *Orchestrator) Recoverable(ctx context.Context) ([]RunWork, error) {
	failed, err := o.store.FailAbandonedRuns(ctx, o.config.AppID, o.now().UTC())
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		observe.Warn(ctx, "启动时已终止无法安全恢复的遗留 Run",
			observe.StringAttr("app_id", o.config.AppID),
			observe.Int64Attr("run_count", failed),
		)
	}
	work, err := o.store.ListQueuedRuns(ctx, o.config.AppID, 1000)
	if err == nil {
		observe.DefaultMetrics().SetQueuedRuns(len(work))
	}
	return work, err
}

func (o *Orchestrator) Runnable(ctx context.Context, limit int) ([]RunWork, error) {
	work, err := o.store.ListRunnableRuns(ctx, o.config.AppID, o.now().UTC(), limit)
	if err == nil {
		observe.DefaultMetrics().SetQueuedRuns(len(work))
	}
	return work, err
}

func (o *Orchestrator) Cancel(ctx context.Context, echoID string) (bool, error) {
	cancelled, err := o.store.CancelQueuedRun(ctx, o.config.AppID, echoID, o.now().UTC())
	if err == nil && cancelled {
		observe.DefaultMetrics().Cancellation()
		observe.DefaultMetrics().QueueRemoved()
	}
	return cancelled, err
}

func (o *Orchestrator) Create(ctx context.Context, request RunRequest) (string, error) {
	echoID, _, err := o.CreateIdempotent(ctx, request)
	return echoID, err
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
	storedEchoID, created, err := o.store.CreateEchoRunIdempotentLimited(ctx, request.IdempotencyKey, idempotency.Fingerprint([]byte(request.Message)), Record{
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
		Model:              app.Model,
		ModelConfigVersion: app.Revision,
		ProtocolVersion:    executor.Version,
		MaxSteps:           app.MaxSteps,
		MaxToolCalls:       app.MaxToolCalls,
		MaxInputTokens:     app.MaxInputTokens,
		MaxOutputTokens:    app.MaxOutputTokens,
		MaxTotalTokens:     app.MaxTotalTokens,
		MaxOutputBytes:     app.MaxOutputBytes,
		MaxCostMicrousd:    app.MaxCostMicrousd,
		ProviderTimeoutMS:  uint32(app.ProviderTimeout.Milliseconds()),
		Deadline:           createdAt.Add(o.config.RunTimeout),
		AvailableAt:        createdAt,
		CapabilityScope:    append([]string(nil), acceptedPolicy.EnabledCapabilities...),
		PermissionScope:    append([]string(nil), acceptedPolicy.PermissionScope...),
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

func (o *Orchestrator) RunExisting(ctx context.Context, echoID string, request RunRequest, emit EventEmitter) (resultErr error) {
	echoRecord, _, err := o.store.GetEcho(ctx, o.config.AppID, echoID)
	if err != nil {
		return err
	}
	if request.Message == "" {
		request.Message = echoRecord.InputMessage
	} else if request.Message != echoRecord.InputMessage {
		return ErrRunInputMismatch
	}
	runStarted := o.now().UTC()
	leaseToken := uuid.NewString()
	run, err := o.store.ClaimRun(ctx, o.config.AppID, echoID, leaseToken, runStarted, runStarted.Add(o.config.LeaseDuration))
	if err != nil {
		return err
	}
	return o.executeClaimedRun(ctx, request, emit, run, runStarted, nil)
}

func (o *Orchestrator) executeClaimedRun(ctx context.Context, request RunRequest, emit EventEmitter, run RunRecord, runStarted time.Time, childResult *string) (resultErr error) {
	echoID := run.EchoID
	observe.DefaultMetrics().RunStarted()
	if run.ParentRunID == "" {
		observe.DefaultMetrics().QueueRemoved()
	}
	defer observe.DefaultMetrics().RunStopped()
	app, configErr := o.appConfigRevision(ctx, run.ModelConfigVersion)
	if configErr != nil || !runMatchesAppConfig(run, app) || run.ProtocolVersion != executor.Version {
		completeErr := o.completeRun(ctx, run, RunStatusFailed, StatusFailed, "", publicerror.Echo("recovery_failed"))
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
	ctx, runSpan := observe.StartSpan(ctx, "agent.run")
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
		observe.StringAttr("model", run.Model),
		observe.StringAttr("model_config_version", run.ModelConfigVersion),
		observe.IntAttr("attempt", int(run.Attempt)),
		observe.Int64Attr("deadline_unix_ms", run.Deadline.UnixMilli()),
		observe.IntAttr("max_steps", int(run.MaxSteps)),
	)
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
		event, err = o.store.AppendEchoEvent(runContext, event)
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
	// 上下文装配：由 Go 决定模型本次看到的内容（配置系统提示 + 渠道提示 +
	// 当前标准消息 + 受限会话历史 + 当前 Capability 投影），Python 只接收
	// 装配完成的系统提示。
	basePrompt := app.SystemPrompt + "\n只能根据 Capability 返回的数据回答，不得编造班次、站点或线路。"
	if channelPrompt := app.ChannelPrompts[run.Channel]; channelPrompt != "" {
		basePrompt += "\n" + channelPrompt
	}
	if run.ParentRunID != "" {
		basePrompt += "\n这是受治理的子 Run。只完成父 Run 指定任务；最终结果仅返回父 Run，不直接面向用户。"
	}
	snapshot, err := o.context.Assemble(runContext, contextasm.Input{
		AppID:            o.config.AppID,
		SessionID:        run.SessionID,
		CurrentMessageID: run.MessageID,
		ConfigRevision:   run.ModelConfigVersion,
		SystemPrompt:     basePrompt,
		Timezone:         app.Timezone,
		Capabilities:     capabilityVersions(capabilities),
		InputMessage:     request.Message,
		Now:              o.now().UTC(),
	})
	if err != nil {
		observe.Error(ctx, "装配 Run 上下文快照失败", err)
		return errors.Join(err, o.fail(ctx, run, "context_unavailable", true))
	}
	if err := o.store.SetRunContext(runContext, run, snapshot.Digest, snapshot.SourcesJSON()); err != nil {
		observe.Error(ctx, "固化 Run 上下文来源版本失败", err)
		return errors.Join(err, o.fail(ctx, run, "internal_error", true))
	}
	if err := emitEvent("run.context", map[string]any{
		"digest":           snapshot.Digest,
		"config_revision":  run.ModelConfigVersion,
		"history_count":    len(snapshot.History.Entries),
		"history_chars":    snapshot.History.TotalChars,
		"history_trimmed":  snapshot.History.Trimmed,
		"capability_count": len(capabilities),
	}); err != nil {
		return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", true))
	}
	observe.Info(ctx, "Run 上下文快照已装配",
		observe.StringAttr("context_digest", snapshot.Digest),
		observe.IntAttr("history_count", len(snapshot.History.Entries)),
		observe.IntAttr("history_trimmed", snapshot.History.Trimmed),
	)
	stream, err := o.agent.Run(runContext)
	if err != nil {
		observe.Error(ctx, "创建执行者会话流失败", err)
		runErr := fmt.Errorf("open agent stream: %w", err)
		return errors.Join(runErr, o.fail(ctx, run, "agent_unavailable", true))
	}
	defer stream.CloseSend()
	systemPrompt := snapshot.SystemPrompt
	startFrame := &executor.Frame{
		EchoId:   echoID,
		RunId:    runID,
		Sequence: 1,
		Body: &executor.Frame_StartRun{StartRun: &executor.StartRun{
			AppId:             o.config.AppID,
			InputMessage:      request.Message,
			Timezone:          app.Timezone,
			Capabilities:      capabilities,
			Model:             run.Model,
			SystemPrompt:      systemPrompt,
			MaxSteps:          run.MaxSteps,
			ProtocolVersion:   executor.Version,
			MaxToolCalls:      run.MaxToolCalls,
			MaxInputTokens:    run.MaxInputTokens,
			MaxOutputTokens:   run.MaxOutputTokens,
			MaxTotalTokens:    run.MaxTotalTokens,
			MaxOutputBytes:    run.MaxOutputBytes,
			MaxCostMicrousd:   run.MaxCostMicrousd,
			ProviderTimeoutMs: run.ProviderTimeoutMS,
			TraceId:           observe.String(ctx, "trace_id"),
			ParentSpanId:      observe.String(ctx, "span_id"),
			ParentRunId:       run.ParentRunID,
		}},
	}
	if err := executor.ValidateStartFrame(startFrame); err != nil {
		observe.Error(ctx, "Run 启动帧未通过本地协议校验", err)
		return errors.Join(err, o.fail(ctx, run, "protocol_violation", true))
	}
	if err := stream.Send(startFrame); err != nil {
		observe.Error(ctx, "发送 Run 输入失败", err)
		runErr := fmt.Errorf("start agent run: %w", err)
		return errors.Join(runErr, o.fail(ctx, run, "agent_start_failed", true))
	}
	if err := emitEvent("run.started", map[string]any{"run_id": runID, "model": run.Model, "attempt": run.Attempt}); err != nil {
		return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", true))
	}
	observe.Info(ctx, "Run 输入已经发送",
		observe.IntAttr("capability_count", len(capabilities)),
	)

	finalMessage := ""
	var terminalFailure *publicerror.Error
	var terminalRunErr error
	handshakeAccepted := false
	usageReported := false
	firstTokenObserved := false
	expectedAgentSequence := run.LastAgentSequence + 1
	kernelSequence := uint64(1)
	seenCallIDs := make(map[string]struct{})
	automaticRetrySafe := true
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
				return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, "", publicerror.Echo("cancelled")))
			}
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				observe.Error(ctx, "Run 执行超时", context.DeadlineExceeded, observe.Duration(runStarted))
				return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, "", publicerror.Echo("deadline_exceeded")))
			}
			if errors.Is(receiveErr, io.EOF) && finalMessage != "" {
				if err := emitEvent("reply.final", map[string]string{"text": finalMessage}); err != nil {
					return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", automaticRetrySafe))
				}
				if err := o.completeRun(ctx, run, RunStatusSucceeded, StatusSucceeded, finalMessage, publicerror.Error{}); err != nil {
					observe.Error(ctx, "持久化 Echo 成功终态失败", err)
					return err
				}
				if childResult != nil {
					*childResult = finalMessage
				}
				observe.Info(ctx, "Run 执行完成",
					observe.IntAttr("reply_length", utf8.RuneCountInString(finalMessage)),
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
				receiveErr = ErrNoFinalMessage
			}
			observe.Error(ctx, "接收执行者事件失败", receiveErr, observe.Duration(runStarted))
			runErr := fmt.Errorf("receive agent frame: %w", receiveErr)
			return errors.Join(runErr, o.fail(ctx, run, "agent_stream_failed", automaticRetrySafe))
		}
		if finalMessage != "" || terminalFailure != nil {
			err = executor.ErrUnexpectedFrame
			observe.Error(ctx, "执行者在终态帧后继续发送数据", err)
			return errors.Join(err, o.fail(ctx, run, "protocol_violation", automaticRetrySafe))
		}
		if err := executor.ValidateInboundEnvelope(frame, echoID, runID, expectedAgentSequence); err != nil {
			observe.Error(ctx, "执行者帧信封违反协议", err,
				observe.Int64Attr("expected_sequence", int64(expectedAgentSequence)),
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
			if _, exists := seenCallIDs[body.CapabilityCall.CallId]; exists {
				err = executor.ErrDuplicateCall
				break
			}
			seenCallIDs[body.CapabilityCall.CallId] = struct{}{}
		case *executor.Frame_ReplyDelta:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateReplyDelta(body.ReplyDelta)
		case *executor.Frame_FinalMessage:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			if !usageReported {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateFinalMessage(body.FinalMessage)
		case *executor.Frame_RunFailure:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateRunFailure(body.RunFailure)
		case *executor.Frame_RunUsage:
			if !handshakeAccepted {
				err = executor.ErrUnexpectedFrame
				break
			}
			err = executor.ValidateRunUsage(
				body.RunUsage,
				run.UsedInputTokens,
				run.UsedOutputTokens,
				run.UsedTotalTokens,
				run.UsedCostMicrousd,
				run.UsedProviderRetries,
				run.MaxInputTokens,
				run.MaxOutputTokens,
				run.MaxTotalTokens,
				run.MaxCostMicrousd,
			)
		default:
			err = executor.ErrUnexpectedFrame
		}
		if err != nil {
			observe.Error(ctx, "执行者帧载荷或顺序违反协议", err,
				observe.Int64Attr("agent_sequence", int64(frame.Sequence)),
			)
			return errors.Join(err, o.fail(ctx, run, "protocol_violation", automaticRetrySafe))
		}
		var sequenceErr error
		if usage := frame.GetRunUsage(); usage != nil {
			sequenceErr = o.store.AdvanceRunAgentSequenceWithUsage(
				runContext,
				run,
				frame.Sequence,
				usage.InputTokens,
				usage.OutputTokens,
				usage.TotalTokens,
				usage.CostMicrousd,
				usage.ProviderRetries,
			)
		} else {
			sequenceErr = o.store.AdvanceRunAgentSequence(runContext, run, frame.Sequence)
		}
		if sequenceErr != nil {
			if errors.Is(runContext.Err(), context.Canceled) {
				return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, "", publicerror.Echo("cancelled")))
			}
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, "", publicerror.Echo("deadline_exceeded")))
			}
			observe.Error(ctx, "持久化 Agent 帧序号失败", sequenceErr,
				observe.Int64Attr("agent_sequence", int64(frame.Sequence)),
			)
			return errors.Join(sequenceErr, o.fail(ctx, run, "internal_error", automaticRetrySafe))
		}
		run.LastAgentSequence = frame.Sequence
		var inputTokenDelta uint64
		var outputTokenDelta uint64
		var costDelta uint64
		var providerRetryDelta uint32
		if usage := frame.GetRunUsage(); usage != nil {
			inputTokenDelta = usage.InputTokens - run.UsedInputTokens
			outputTokenDelta = usage.OutputTokens - run.UsedOutputTokens
			costDelta = usage.CostMicrousd - run.UsedCostMicrousd
			providerRetryDelta = usage.ProviderRetries - run.UsedProviderRetries
			run.UsedInputTokens = usage.InputTokens
			run.UsedOutputTokens = usage.OutputTokens
			run.UsedTotalTokens = usage.TotalTokens
			run.UsedCostMicrousd = usage.CostMicrousd
			run.UsedProviderRetries = usage.ProviderRetries
		}
		expectedAgentSequence++

		switch body := frame.Body.(type) {
		case *executor.Frame_RunAccepted:
			handshakeAccepted = true
			observe.Info(ctx, "执行者协议版本握手完成",
				observe.StringAttr("protocol_version", body.RunAccepted.ProtocolVersion),
			)
		case *executor.Frame_CapabilityCall:
			if spec, _, resolveErr := o.registry.ResolveCapability(body.CapabilityCall.CapabilityId); resolveErr == nil &&
				(spec.SideEffect == registry.SideEffectWrite || spec.SideEffect == registry.SideEffectExternal) {
				automaticRetrySafe = false
			}
			observe.Info(ctx, "模型请求调用 Capability",
				observe.StringAttr("call_id", body.CapabilityCall.CallId),
				observe.StringAttr("capability_id", body.CapabilityCall.CapabilityId),
				observe.IntAttr("argument_bytes", len(body.CapabilityCall.PayloadJson)),
			)
			result := o.invokeCapability(runContext, run, body.CapabilityCall)
			if errors.Is(runContext.Err(), context.Canceled) {
				return errors.Join(context.Canceled, o.completeRun(ctx, run, RunStatusCancelled, StatusCancelled, "", publicerror.Echo("cancelled")))
			}
			if errors.Is(runContext.Err(), context.DeadlineExceeded) {
				return errors.Join(context.DeadlineExceeded, o.completeRun(ctx, run, RunStatusTimedOut, StatusFailed, "", publicerror.Echo("deadline_exceeded")))
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
				runErr := fmt.Errorf("send capability result: %w", err)
				return errors.Join(runErr, o.fail(ctx, run, "agent_stream_failed", automaticRetrySafe))
			}
			if err := emitEvent("capability.completed", map[string]any{
				"call_id":       result.CallId,
				"capability_id": result.CapabilityId,
				"success":       result.Success,
			}); err != nil {
				return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", automaticRetrySafe))
			}
		case *executor.Frame_ReplyDelta:
			if !firstTokenObserved {
				observe.DefaultMetrics().ObserveFirstToken(time.Since(runStarted))
				firstTokenObserved = true
			}
			observe.Debug(ctx, "收到模型回复片段",
				observe.IntAttr("delta_length", utf8.RuneCountInString(body.ReplyDelta.Text)),
			)
			if err := emitEvent("reply.delta", map[string]string{"text": body.ReplyDelta.Text}); err != nil {
				return errors.Join(err, o.fail(ctx, run, "event_delivery_failed", automaticRetrySafe))
			}
		case *executor.Frame_FinalMessage:
			if !firstTokenObserved {
				observe.DefaultMetrics().ObserveFirstToken(time.Since(runStarted))
				firstTokenObserved = true
			}
			finalMessage = body.FinalMessage.Text
		case *executor.Frame_RunFailure:
			public := publicerror.Agent(body.RunFailure.Code, body.RunFailure.Retryable)
			runErr := fmt.Errorf("%w: code=%s", ErrAgentRunFailed, public.Code)
			observe.Error(ctx, "执行者报告运行失败", runErr,
				observe.StringAttr("error_code", public.Code),
				observe.BoolAttr("retryable", public.Retryable),
				observe.Duration(runStarted),
			)
			terminalFailure = &public
			terminalRunErr = runErr
		case *executor.Frame_RunUsage:
			observe.DefaultMetrics().AddModelUsage(
				inputTokenDelta,
				outputTokenDelta,
				costDelta,
			)
			for retry := uint32(0); retry < providerRetryDelta; retry++ {
				observe.DefaultMetrics().ProviderRetry()
			}
			observe.Info(ctx, "已记录模型用量",
				observe.Int64Attr("input_tokens", int64(body.RunUsage.InputTokens)),
				observe.Int64Attr("output_tokens", int64(body.RunUsage.OutputTokens)),
				observe.Int64Attr("total_tokens", int64(body.RunUsage.TotalTokens)),
				observe.Int64Attr("cost_microusd", int64(body.RunUsage.CostMicrousd)),
			)
			usageReported = true
		}
	}
}

func (o *Orchestrator) projectCapabilities(policy appconfig.PolicySnapshot, run RunRecord) []*executor.Capability {
	all := o.registry.Capabilities()
	projected := make([]*executor.Capability, 0, len(all))
	scope := make(map[string]struct{}, len(run.CapabilityScope))
	for _, capabilityID := range run.CapabilityScope {
		scope[capabilityID] = struct{}{}
	}
	for _, capability := range all {
		if !policy.CapabilityEnabled(capability.ID) {
			continue
		}
		if _, enabled := scope[capability.ID]; !enabled {
			continue
		}
		if run.ParentRunID != "" && capability.ID == SubagentCapabilityID {
			continue
		}
		if _, err := registry.NarrowPermissions(policy.PermissionScope, capability.RequiredPermissions); err != nil {
			continue
		}
		if _, err := registry.NarrowPermissions(run.PermissionScope, capability.RequiredPermissions); err != nil {
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

func (o *Orchestrator) invokeCapability(ctx context.Context, run RunRecord, call *executor.CapabilityCall) *executor.CapabilityResult {
	runID := run.ID
	if call == nil || call.CallId == "" || len(call.CallId) > 128 || idempotency.ValidateKey(call.CallId) != nil {
		public := publicerror.Agent("protocol_violation", false)
		return &executor.CapabilityResult{
			CallId:       call.GetCallId(),
			CapabilityId: call.GetCapabilityId(),
			ErrorCode:    public.Code,
			ErrorMessage: public.Message,
		}
	}
	encoded, replayed, err := o.idempotency.Execute(ctx, idempotency.Operation{
		AppID:       o.config.AppID,
		Scope:       "agent.call/" + runID,
		Key:         call.CallId,
		Fingerprint: idempotency.Fingerprint([]byte(runID), []byte(call.CapabilityId), call.PayloadJson),
		OwnerID:     runID + ":" + call.CallId,
	}, func(executionContext context.Context) ([]byte, error) {
		result := o.invokeCapabilityOnce(executionContext, run, call)
		return proto.Marshal(result)
	})
	if err != nil {
		public := publicerror.Capability(err)
		observe.Warn(ctx, "Agent CapabilityCall 幂等治理失败",
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

func (o *Orchestrator) invokeCapabilityOnce(ctx context.Context, run RunRecord, call *executor.CapabilityCall) *executor.CapabilityResult {
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
	index := sort.SearchStrings(run.CapabilityScope, call.CapabilityId)
	if (run.ParentRunID != "" && call.CapabilityId == SubagentCapabilityID) ||
		index >= len(run.CapabilityScope) || run.CapabilityScope[index] != call.CapabilityId {
		return o.rejectedCapability(ctx, run, call, started, runtime.ErrCapabilityDisabled)
	}
	permissionScope, err := registry.NarrowPermissions(policy.PermissionScope, run.PermissionScope)
	if err != nil {
		return o.rejectedCapability(ctx, run, call, started, registry.ErrPermissionDenied)
	}
	deadline := started.Add(30 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	request := contracts.RequestContext{
		AppID:           o.config.AppID,
		EchoID:          echoID,
		RequestID:       requestID,
		TraceID:         firstNonEmpty(observe.String(ctx, "trace_id"), echoID),
		RunID:           runID,
		ParentRunID:     run.ParentRunID,
		CallID:          call.CallId,
		Deadline:        deadline,
		IdempotencyKey:  call.CallId,
		ProtocolVersion: executor.Version,
		PermissionScope: permissionScope,
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
	auditErr := o.store.RecordCapabilityCall(auditContext, call.CallId, runID, echoID, o.config.AppID, call.CapabilityId, auditPayload, result.Success, capabilityFailure, o.now().Sub(started))
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

func (o *Orchestrator) rejectedCapability(ctx context.Context, run RunRecord, call *executor.CapabilityCall, started time.Time, cause error) *executor.CapabilityResult {
	public := publicerror.Capability(cause)
	result := &executor.CapabilityResult{
		CallId: call.CallId, CapabilityId: call.CapabilityId,
		ErrorCode: public.Code, ErrorMessage: public.Message,
	}
	auditPayload := observe.SanitizeAuditJSON(call.PayloadJson, 8192)
	auditContext, auditCancel := detachedContext(ctx)
	auditErr := o.store.RecordCapabilityCall(
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
			terminalErr := o.completeRun(ctx, run, RunStatusFailed, StatusFailed, "", public)
			return errors.Join(err, terminalErr)
		}
	}
	storeErr := o.completeRun(ctx, run, RunStatusFailed, StatusFailed, "", public)
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
	return run.ParentRunID == "" && public.Retryable && automaticRetrySafe && run.Attempt < o.config.MaxRunAttempts
}

func (o *Orchestrator) retryRun(ctx context.Context, run RunRecord, failure publicerror.Error) error {
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
	next.LastAgentSequence = 0
	next.RecoverableState = json.RawMessage(`{}`)
	next.ContextDigest = ""
	next.ContextSources = json.RawMessage(`{}`)
	next.ErrorCode = ""
	next.ErrorMessage = ""
	next.CreatedAt = completedAt
	next.StartedAt = nil
	next.CompletedAt = nil
	next.UsedInputTokens = 0
	next.UsedOutputTokens = 0
	next.UsedTotalTokens = 0
	next.UsedCostMicrousd = 0
	next.UsedProviderRetries = 0
	finishContext, cancel := detachedContext(ctx)
	defer cancel()
	if err := o.store.RetryRun(finishContext, run, next, failure, completedAt); err != nil {
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
			renewContext, renewCancel := context.WithTimeout(ctx, o.config.LeaseDuration)
			err := o.store.RenewRunLease(renewContext, run, renewedAt.UTC(), renewedAt.UTC().Add(o.config.LeaseDuration))
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

func (o *Orchestrator) completeRun(ctx context.Context, run RunRecord, runStatus, echoStatus, finalMessage string, public publicerror.Error) error {
	finishContext, cancel := detachedContext(ctx)
	defer cancel()
	completedAt := o.now().UTC()
	var err error
	if run.ParentRunID == "" {
		err = o.store.CompleteRun(finishContext, run, runStatus, echoStatus, finalMessage, public, completedAt)
	} else {
		err = o.store.CompleteChildRun(finishContext, run, runStatus, finalMessage, public, completedAt)
	}
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

func detachedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
