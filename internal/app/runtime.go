package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/configui"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/ingress"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq"
	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	controlconfig "github.com/projectluojia/AI-Luo-Man-ga/internal/controlplane/config"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contextasm"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/prompt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// Run 启动配置控制面及其受监督的 Core 运行时。
func Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	baseConfig, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err := observe.Configure(observe.Config{
		Service:        "ailuo-kernel",
		Environment:    baseConfig.environment,
		Level:          baseConfig.logLevel,
		Format:         baseConfig.logFormat,
		AddSource:      baseConfig.logSource,
		MaxValueLength: baseConfig.logMaxValueLength,
		Writer:         os.Stdout,
	}); err != nil {
		return err
	}
	ctx = observe.With(ctx, observe.Component("kernel"))
	manager, err := controlconfig.NewService(baseConfig.localConfigRoot)
	if err != nil {
		return fmt.Errorf("open deployment configuration: %w", err)
	}
	select {
	case <-manager.Changes():
	default:
	}
	configServer, err := configui.NewServer(manager)
	if err != nil {
		return err
	}
	configServerErrors := make(chan error, 1)
	go func() {
		observe.Info(ctx, "本机配置控制台开始监听", observe.StringAttr("address", baseConfig.configUIAddress))
		configServerErrors <- configServer.Run(ctx, baseConfig.configUIAddress)
	}()
	for {
		resolved, err := manager.WaitReady(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		current, err := applyLocalConfig(baseConfig, resolved)
		if err != nil {
			manager.SetRuntime("failed", "配置文件无法安全读取，请重新保存", resolved.Settings.Revision)
			if err := waitForConfigurationChange(ctx, manager, configServerErrors); err != nil {
				return err
			}
			continue
		}
		manager.SetRuntime("starting", "正在启动内核与接入服务", resolved.Settings.Revision)
		coreContext, cancelCore := context.WithCancel(ctx)
		coreErrors := make(chan error, 1)
		go func() { coreErrors <- runCore(coreContext, cancelCore, current, manager) }()
		select {
		case <-ctx.Done():
			cancelCore()
			return <-coreErrors
		case err := <-configServerErrors:
			cancelCore()
			<-coreErrors
			return err
		case <-manager.Changes():
			manager.SetRuntime("restarting", "正在重启内核以应用新配置", manager.Snapshot().Settings.Revision)
			cancelCore()
			if err := <-coreErrors; err != nil && !errors.Is(err, context.Canceled) {
				observe.Error(ctx, "上一配置内核关闭时发生错误", err)
			}
		case err := <-coreErrors:
			if ctx.Err() != nil {
				return nil
			}
			observe.Error(ctx, "内核启动或运行失败，配置控制台继续可用", err)
			manager.SetRuntime("failed", "启动失败，请检查配置后重新保存", resolved.Settings.Revision)
			if err := waitForConfigurationChange(ctx, manager, configServerErrors); err != nil {
				return err
			}
		}
	}
}

func waitForConfigurationChange(ctx context.Context, manager *controlconfig.Service, serverErrors <-chan error) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-serverErrors:
		return err
	case <-manager.Changes():
		return nil
	}
}

func runCore(ctx context.Context, stop context.CancelFunc, config config, localConfig *controlconfig.Service) (resultErr error) {
	lifecycle := coreLifecycle{}
	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, lifecycle.Shutdown(cleanupContext))
	}()
	observe.Info(ctx, "正在启动AI珞（爱珞）内核",
		observe.StringAttr("http_address", config.httpAddress),
		observe.StringAttr("agent_address", config.agentAddress),
		observe.StringAttr("model", config.model),
	)
	if config.databasePath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(config.databasePath), 0o750); err != nil {
			return fmt.Errorf("create database directory: %w", err)
		}
	}
	store, err := sqlite.Open(config.databasePath)
	if err != nil {
		return err
	}
	lifecycle.store = store
	observe.Info(ctx, "统一数据库已经就绪")
	blobStore, err := blob.Open(filepath.Join(filepath.Dir(config.databasePath), "blobs"), session.MaxMessageContentBytes)
	if err != nil {
		return fmt.Errorf("open blob storage: %w", err)
	}
	lifecycle.blobStore = blobStore
	sessionService, err := session.NewService(store, blobStore)
	if err != nil {
		return fmt.Errorf("create session service: %w", err)
	}
	if config.loadDemoData {
		if err := demo.LoadBusData(ctx, store.PackageDocuments(), time.Now()); err != nil {
			return fmt.Errorf("load demo bus data: %w", err)
		}
		observe.Warn(ctx, "已载入非权威校巴演示数据",
			observe.BoolAttr("authoritative", false),
			observe.StringAttr("source", "demo-fixture-not-zhihui-luojia"),
		)
	}

	reg := registry.New()
	app, created, err := store.Ensure(ctx, appconfig.Config{
		AppID: campus.AppID, Enabled: true, Model: config.model,
		SystemPrompt: config.baseSystemPrompt, ChannelPrompts: config.channelPrompts,
		Timezone: config.agentRun.Timezone, MaxSteps: config.agentRun.MaxSteps,
		MaxToolCalls: config.agentRun.MaxToolCalls, MaxInputTokens: config.agentRun.MaxInputTokens,
		MaxOutputTokens: config.agentRun.MaxOutputTokens, MaxTotalTokens: config.agentRun.MaxTotalTokens,
		MaxOutputBytes: config.agentRun.MaxOutputBytes, ProviderTimeout: config.modelRequestTimeout,
		EnabledCapabilities: []string{
			campus.BusStopSearchCapabilityID, campus.BusRouteListCapabilityID,
			campus.BusJourneySearchCapabilityID, kernelecho.CreateChildRunCapabilityID,
			kernelecho.GetChildStatusCapabilityID, promptservice.PreferenceGetID,
			promptservice.PreferenceSetID, promptservice.PreferenceResetID,
		},
	})
	if err != nil {
		return fmt.Errorf("ensure campus App config: %w", err)
	}
	if !created {
		replacement := app
		replacement.Model = config.model
		replacement.ProviderTimeout = config.modelRequestTimeout
		replacement.Timezone = config.agentRun.Timezone
		replacement.MaxSteps = config.agentRun.MaxSteps
		replacement.MaxToolCalls = config.agentRun.MaxToolCalls
		replacement.MaxInputTokens = config.agentRun.MaxInputTokens
		replacement.MaxOutputTokens = config.agentRun.MaxOutputTokens
		replacement.MaxTotalTokens = config.agentRun.MaxTotalTokens
		replacement.MaxOutputBytes = config.agentRun.MaxOutputBytes
		replacement.SystemPrompt = config.baseSystemPrompt
		replacement.ChannelPrompts = config.channelPrompts
		app, err = store.CompareAndSwap(ctx, app.Generation, replacement)
		if err != nil {
			return fmt.Errorf("update campus App configuration: %w", err)
		}
	}
	observe.Info(ctx, "校园 App 持久配置已经就绪",
		observe.StringAttr("app_id", app.AppID), observe.StringAttr("config_revision", app.Revision),
		observe.Int64Attr("config_generation", int64(app.Generation)), observe.BoolAttr("created", created),
	)
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		return err
	}
	promptService := promptservice.NewService(config.promptCatalog, store)
	if err := promptservice.Register(reg, promptService); err != nil {
		return fmt.Errorf("register prompt Service: %w", err)
	}
	confirmations := confirmation.NewService(store, confirmation.Config{})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		MaxCallDepth: config.orchestration.MaxCallDepth, IdempotencyStore: store,
		ConfirmationVerifier: confirmations,
	})
	// 内置 agent 经虚拟包源走与安装目录完全一致的记录/解析/校验/注册管线：
	// 组合根只提供"这个 App 的 executor 由哪个包充当"的部署知识。
	builtinAgent, err := newBuiltinAgentSource(config, app.Model)
	if err != nil {
		return fmt.Errorf("resolve built-in agent package: %w", err)
	}
	executorHost, err := loader.NewProcessHost(loader.ProcessHostConfig{
		Resolve: builtinAgent.ResolveProcess, Verify: builtinAgent.VerifyProcess,
		Spawn: config.manageAgent, ExecutorHealthModel: app.Model,
		Stdout: os.Stdout, Stderr: os.Stderr,
		DialTimeout:    secondsDuration(config.agentProcess.DialTimeoutSeconds),
		StopGrace:      secondsDuration(config.agentProcess.StopGraceSeconds),
		TerminateGrace: secondsDuration(config.agentProcess.TerminateGraceSeconds),
	})
	if err != nil {
		return fmt.Errorf("create executor runtime host: %w", err)
	}
	installedHosts, installedRecords, err := configureInstalledRuntimes(ctx, config, store.PackageDocuments())
	if err != nil {
		return err
	}
	hosts := make([]loader.Host, 0, 1+len(installedHosts))
	hosts = append(hosts, executorHost)
	hosts = append(hosts, installedHosts...)
	runtimeLoader, err := loader.New(hosts...)
	if err != nil {
		return fmt.Errorf("create runtime loader: %w", err)
	}
	lifecycle.runtimeLoader = runtimeLoader
	// 内置 agent 与安装包统一注册：同一发现/校验/注册管线，无第二条路径。
	allRecords := append([]loader.InstalledRecord{builtinAgent.Record()}, installedRecords...)
	if err := loader.RegisterInstalled(ctx, runtimeLoader, reg, allRecords); err != nil {
		return fmt.Errorf("register installed runtimes: %w", err)
	}
	campusInstalled := false
	for _, record := range installedRecords {
		if record.PackageID == campus.ServiceID && record.ComponentID == campus.BusComponentID {
			campusInstalled = true
			break
		}
	}
	if !campusInstalled {
		return fmt.Errorf("campus.bus 未安装：请先安装其 .tgz 发布物或 owner/repo[@version] 发布包")
	}
	pinnedRuntimes := runtimeLoader.Pinned()
	if err := runtimeLoader.Warmup(ctx, pinnedRuntimes, min(len(pinnedRuntimes), 4)); err != nil {
		return fmt.Errorf("warm pinned runtimes: %w", err)
	}
	executorLease, err := runtimeLoader.Executor(ctx)
	if err != nil {
		return fmt.Errorf("resolve AI executor runtime: %w", err)
	}
	lifecycle.executorLease = executorLease
	executorRuntime := executorLease.Runtime()
	clientProvider, ok := executorRuntime.(executor.ClientProvider)
	if !ok {
		return fmt.Errorf("executor runtime does not expose an executor client")
	}
	executorClient := clientProvider.Client()
	if config.manageAgent {
		if lifecycle, ok := executorRuntime.(executor.ProcessLifecycle); ok {
			go func() {
				<-lifecycle.Done()
				if processErr := lifecycle.Err(); processErr != nil && ctx.Err() == nil {
					observe.Error(ctx, "AI 执行者进程异常退出", processErr)
					stop()
				}
			}()
		}
	}
	observe.Info(ctx, "内置 AI 执行者已经就绪",
		observe.StringAttr("runtime_id", executorLease.ID()), observe.StringAttr("address", config.agentAddress),
		observe.BoolAttr("managed_process", config.manageAgent),
	)

	orchestrator := kernelecho.NewOrchestrator(executorClient, reg, dispatcher, policy, kernelecho.StorePorts{
		Idempotency: store, Creation: store, Execution: store, Recovery: store,
		Cancellation: store, Events: store, Audit: store,
	}, kernelecho.Config{
		AppID: campus.AppID, AppConfigSource: store, Context: sessionService,
		ContextBudget: contextasm.Budget{
			MaxMessages: config.contextAssembly.MaxMessages, MaxCharsPerMsg: config.contextAssembly.MaxCharsPerMsg,
			MaxTotalChars: config.contextAssembly.MaxTotalChars, MaxPromptBytes: config.contextAssembly.MaxPromptBytes,
		},
		Prompts:        promptServiceRenderer{promptService},
		RunTimeout:     secondsDuration(config.orchestration.RunTimeoutSeconds),
		MaxRunAttempts: config.orchestration.MaxRunAttempts, QueueCapacity: config.orchestration.QueueCapacity,
		MaxChildRuns: int(config.agentRun.MaxChildRuns),
	})
	if err := kernelecho.RegisterChildCapabilities(reg, orchestrator); err != nil {
		return fmt.Errorf("register governed child Run capabilities: %w", err)
	}
	observe.Info(ctx, "校园服务与受治理子 Run Capability 注册完成",
		observe.IntAttr("service_count", len(reg.Services())), observe.IntAttr("tool_count", len(reg.Tools())),
		observe.IntAttr("capability_count", len(reg.Capabilities())),
	)
	taskTypes := task.NewTypeRegistry()
	taskScheduler := task.NewScheduler(store, taskTypes, task.Config{})
	lifecycle.taskScheduler = taskScheduler
	confirmationSweepInterval := secondsDuration(config.governance.ConfirmationSweepSeconds)
	if err := registerGovernanceTaskTypes(taskTypes, confirmations, taskScheduler, confirmationSweepInterval); err != nil {
		return fmt.Errorf("register governance task types: %w", err)
	}
	if err := taskScheduler.Start(ctx); err != nil {
		return fmt.Errorf("start background task scheduler: %w", err)
	}
	if err := seedConfirmationSweep(ctx, taskScheduler, campus.AppID, confirmationSweepInterval); err != nil {
		return fmt.Errorf("seed confirmation sweep: %w", err)
	}
	observe.Info(ctx, "后台任务调度器与确认过期清扫已就绪",
		observe.StringAttr("app_id", campus.AppID), observe.StringAttr("sweep_type", confirmationSweepType),
		observe.Int64Attr("sweep_interval_ms", confirmationSweepInterval.Milliseconds()),
	)
	echoEvents := access.NewEventHub()
	runScheduler := kernelecho.NewScheduler(ctx, orchestrator, store, echoEvents, campus.AppID,
		kernelecho.WithScheduler(config.scheduler.Workers, time.Duration(config.scheduler.PollMs)*time.Millisecond, config.scheduler.BatchSize))
	lifecycle.runScheduler = runScheduler
	if _, err := runScheduler.Recover(ctx); err != nil {
		return fmt.Errorf("recover durable runs: %w", err)
	}
	echoAdmission := kernelecho.NewAdmission(orchestrator, runScheduler)
	readiness := health.Combined{store, health.ExecutorAppChecker{Client: executorClient, Source: store, AppID: campus.AppID}}
	identities := identity.NewService(store)
	platformHub, err := access.NewHub(campus.AppID, store, identities)
	if err != nil {
		return fmt.Errorf("configure platform access hub: %w", err)
	}
	qqProvisioner, err := qq.NewProvisioner(identities)
	if err != nil {
		return fmt.Errorf("configure QQ identity provisioner: %w", err)
	}
	qqManager, err := qq.NewManager(store, func(settings qqsettings.Settings, connectionChange func(bool)) (qq.Runner, error) {
		return qq.New(qq.Config{
			AppID: settings.AppID, WSURL: settings.WSURL, Token: config.qqToken, BotQQID: settings.BotQQID,
			AllowedGroupIDs: settings.AllowedGroupIDs, AllowedPrivateUserIDs: settings.AllowedPrivateUserIDs,
			QuickReplies: config.qqQuickReplies, PokeReplies: config.qqPokeReplies,
			Provisioner: qqProvisioner, Admission: echoAdmission, OnConnectionChange: connectionChange,
			DialTimeout:    secondsDuration(config.qqConnection.DialTimeoutSeconds),
			ReconnectDelay: secondsDuration(config.qqConnection.ReconnectDelaySeconds),
			RunTimeout:     secondsDuration(config.qqConnection.RunTimeoutSeconds),
		}, platformHub, echoEvents, store)
	}, secondsDuration(config.qqConnection.ManagerStopTimeoutSeconds))
	if err != nil {
		return fmt.Errorf("configure QQ access manager: %w", err)
	}
	lifecycle.qqManager = qqManager
	webAccess := web.NewServer(echoAdmission, store, readiness, reg, policy, campus.AppID, platformHub, runScheduler, echoEvents,
		web.WithDispatcher(dispatcher))
	ingressServer := ingress.NewServer(campus.AppID, platformHub, echoAdmission)
	lifecycle.webAccess = webAccess
	lifecycle.ingressServer = ingressServer
	if err := qqManager.Start(ctx, qqsettings.Settings{
		AppID: campus.AppID, Enabled: config.qqEnabled, WSURL: config.qqWSURL, BotQQID: config.qqBotID,
		AllowedGroupIDs: config.qqAllowedGroupIDs, AllowedPrivateUserIDs: config.qqAllowedPrivateIDs,
	}); err != nil {
		return fmt.Errorf("start QQ access manager: %w", err)
	}
	qqDesired := qqsettings.Settings{
		AppID: campus.AppID, Enabled: config.qqEnabled, WSURL: config.qqWSURL, BotQQID: config.qqBotID,
		AllowedGroupIDs: config.qqAllowedGroupIDs, AllowedPrivateUserIDs: config.qqAllowedPrivateIDs,
	}
	qqCurrent, qqStatus, err := qqManager.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("read QQ access configuration: %w", err)
	}
	if !qqsettings.EqualContent(qqCurrent, qqDesired) {
		qqCurrent, qqStatus, err = qqManager.Update(ctx, qqCurrent.Generation, qqDesired)
		if err != nil {
			return fmt.Errorf("apply QQ access configuration: %w", err)
		}
	}
	observe.Info(ctx, "QQ Access 管理器已就绪",
		observe.BoolAttr("enabled", qqCurrent.Enabled), observe.BoolAttr("running", qqStatus.Running),
		observe.IntAttr("allowed_group_count", len(qqCurrent.AllowedGroupIDs)),
		observe.IntAttr("allowed_private_user_count", len(qqCurrent.AllowedPrivateUserIDs)),
	)
	outer := http.NewServeMux()
	outer.Handle("/api/v1/ingress/", ingressServer.Handler())
	outer.Handle("/", webAccess.Handler())
	server := &http.Server{
		Addr: config.httpAddress, Handler: outer, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	lifecycle.server = server
	serverErrors := make(chan error, 1)
	go func() {
		observe.Info(ctx, "Web Access 开始监听", observe.StringAttr("address", config.httpAddress))
		serverErrors <- server.ListenAndServe()
	}()
	localConfig.SetRuntime("ready", "内核与接入服务正在运行", localConfig.Snapshot().Settings.Revision)
	select {
	case <-ctx.Done():
		observe.Info(ctx, "收到停止信号，正在关闭服务")
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		shutdownErr := lifecycle.Shutdown(shutdownContext)
		cancel()
		cleaned = true
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		observe.Info(context.Background(), "AI珞（爱珞）服务已经安全关闭")
		return nil
	case err := <-serverErrors:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
