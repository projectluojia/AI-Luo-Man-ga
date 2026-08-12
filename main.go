package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/ingress"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/agenthost"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/bootstrap"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/subagent"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/task"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"

	"google.golang.org/grpc"
)

func main() {
	handled, err := runMaintenanceCommand(os.Args[1:], os.Stdout)
	if err == nil && handled {
		return
	}
	if err == nil {
		err = run()
	}
	if err != nil {
		observe.Error(context.Background(), "AI珞（爱珞）服务异常退出", err)
		os.Exit(1)
	}
}

func runMaintenanceCommand(arguments []string, output io.Writer) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	// runtime-host 是长驻服务器形态：独立信号上下文，不套用维护命令的 10 分钟上限。
	if arguments[0] == "runtime-host" {
		return runRuntimeHostCommand(arguments[1:], output)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	switch arguments[0] {
	case "backup":
		flags := flag.NewFlagSet("backup", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		database := flags.String("database", "", "SQLite 数据库绝对路径")
		destination := flags.String("destination", "", "备份目标绝对路径")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *database == "" || *destination == "" {
			return true, fmt.Errorf("configuration error: backup requires --database and --destination absolute paths")
		}
		if err := sqlite.BackupDatabase(ctx, *database, *destination); err != nil {
			return true, err
		}
		_, err := fmt.Fprintln(output, "SQLite 备份已创建并通过完整性校验")
		return true, err
	case "validate-backup":
		flags := flag.NewFlagSet("validate-backup", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		backup := flags.String("backup", "", "备份文件绝对路径")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *backup == "" {
			return true, fmt.Errorf("configuration error: validate-backup requires --backup absolute path")
		}
		if err := sqlite.ValidateBackup(ctx, *backup); err != nil {
			return true, err
		}
		_, err := fmt.Fprintln(output, "SQLite 备份完整性校验通过")
		return true, err
	case "restore":
		flags := flag.NewFlagSet("restore", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		backup := flags.String("backup", "", "备份文件绝对路径")
		destination := flags.String("destination", "", "恢复目标绝对路径")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *backup == "" || *destination == "" {
			return true, fmt.Errorf("configuration error: restore requires --backup and --destination absolute paths")
		}
		if err := sqlite.RestoreBackup(ctx, *backup, *destination); err != nil {
			return true, err
		}
		_, err := fmt.Fprintln(output, "SQLite 数据库已恢复并通过完整性校验")
		return true, err
	case "identity-bind":
		flags := flag.NewFlagSet("identity-bind", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		database := flags.String("database", "", "SQLite 数据库绝对路径")
		userID := flags.String("user", "", "Deployment 级内部用户 user_id")
		appID := flags.String("app", campus.AppID, "App 标识")
		platform := flags.String("platform", "", "外部平台标识")
		space := flags.String("space", "", "外部平台空间标识")
		platformUser := flags.String("platform-user", "", "外部平台用户标识")
		roles := flags.String("roles", "", "逗号分隔的角色标识列表（可选）")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *database == "" || *userID == "" {
			return true, fmt.Errorf("configuration error: identity-bind requires --database and --user")
		}
		if !filepath.IsAbs(*database) {
			return true, fmt.Errorf("configuration error: identity-bind requires an absolute --database path")
		}
		platformConfigured := *platform != "" || *space != "" || *platformUser != ""
		if platformConfigured && (*platform == "" || *space == "" || *platformUser == "") {
			return true, fmt.Errorf("configuration error: --platform, --space and --platform-user must be provided together")
		}
		var roleIDs []string
		for _, part := range strings.Split(*roles, ",") {
			if part = strings.TrimSpace(part); part != "" {
				roleIDs = append(roleIDs, part)
			}
		}
		store, err := sqlite.Open(*database)
		if err != nil {
			return true, err
		}
		provisionErr := provisionIdentity(ctx, store, identityProvision{
			AppID: *appID, UserID: *userID,
			Platform: *platform, PlatformSpaceID: *space, PlatformUserID: *platformUser,
			RoleIDs: roleIDs,
		})
		closeErr := store.Close()
		if err := errors.Join(provisionErr, closeErr); err != nil {
			return true, err
		}
		_, err = fmt.Fprintf(output, "身份开通完成：user_id=%s app=%s 绑定平台=%s 角色=%d\n", *userID, *appID, *platform, len(roleIDs))
		return true, err
	default:
		return true, fmt.Errorf("configuration error: unknown command")
	}
}

// identityProvision 是 identity-bind 命令的输入。
type identityProvision struct {
	AppID           string
	UserID          string
	Platform        string
	PlatformSpaceID string
	PlatformUserID  string
	RoleIDs         []string
}

// provisionIdentity 幂等开通内部用户并写入 App 成员关系；可选绑定一个外部平台身份。
// 用户已存在、同一外部身份重复绑定同一用户、成员角色未变化时都视为成功重放。
func provisionIdentity(ctx context.Context, store *sqlite.Store, request identityProvision) error {
	service := identity.NewService(store)
	if _, err := service.CreateUser(ctx, request.UserID); err != nil && !errors.Is(err, identity.ErrConflict) {
		return fmt.Errorf("身份开通失败：创建内部用户失败: %w", err)
	}
	if request.Platform != "" {
		if err := service.BindExternalIdentity(ctx, identity.ExternalIdentity{
			AppID: request.AppID, Platform: request.Platform,
			PlatformSpaceID: request.PlatformSpaceID, PlatformUserID: request.PlatformUserID,
			UserID: request.UserID,
		}); err != nil {
			return fmt.Errorf("身份开通失败：绑定外部平台身份失败: %w", err)
		}
	}
	if err := service.SetMembership(ctx, identity.AppMembership{
		AppID: request.AppID, UserID: request.UserID, RoleIDs: request.RoleIDs,
	}); err != nil {
		return fmt.Errorf("身份开通失败：写入 App 成员关系失败: %w", err)
	}
	return nil
}

// runRuntimeHostCommand 运行外部 Runtime Host 服务器：加载安装目录，为 installed
// hosted 包提供 RuntimeHost 协议服务，直到收到停止信号。这是"外部 Runtime Host 进程"
// 的产品形态——内核配置 AILUO_RUNTIME_HOST_ADDRESS 后经 GRPCHost 连接本服务执行
// hosted 工件；宿主进程不可用时内核 fail-closed，不降级回进程内执行。
func runRuntimeHostCommand(arguments []string, output io.Writer) (bool, error) {
	flags := flag.NewFlagSet("runtime-host", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	installRoot := flags.String("install-root", "", "安装目录绝对路径")
	address := flags.String("address", "", "监听地址（loopback 或绝对 Unix socket）")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *installRoot == "" || *address == "" {
		return true, fmt.Errorf("configuration error: runtime-host requires --install-root and --address")
	}
	if !filepath.IsAbs(*installRoot) || filepath.Clean(*installRoot) != *installRoot {
		return true, fmt.Errorf("configuration error: --install-root must be a clean absolute path")
	}
	if !loader.IsLocalRuntimeAddress(*address) {
		return true, fmt.Errorf("configuration error: --address must be loopback or an absolute unix socket")
	}
	return true, serveRuntimeHost(*installRoot, *address, output)
}

// serveRuntimeHost 装载 hosted 后端并监听 RuntimeHost 协议，直到信号停止。
func serveRuntimeHost(installRoot, address string, output io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = observe.With(ctx, observe.Component("runtime_host"))
	observe.Info(ctx, "正在启动 Runtime Host 服务",
		observe.StringAttr("install_root", installRoot),
		observe.StringAttr("address", address),
	)
	catalog, err := loader.NewCatalog(installRoot)
	if err != nil {
		return err
	}
	records, err := catalog.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover installed runtimes: %w", err)
	}
	hostedCount := 0
	for _, record := range records {
		if record.Runtime.Mode == loader.ModeHosted {
			hostedCount++
		}
	}
	if hostedCount == 0 {
		return fmt.Errorf("runtime host: install root contains no hosted runtimes")
	}
	backend, err := loader.NewHostedRuntimeBackend(loader.HostedBackendConfig{ReadArtifact: catalog.ReadArtifact})
	if err != nil {
		return fmt.Errorf("create hosted backend: %w", err)
	}
	protocolServer, err := loader.NewRuntimeHostProtocolServer(loader.RuntimeHostServerConfig{
		Mode: loader.ModeHosted, Backend: backend, MaxRuntimes: hostedCount, MaxConcurrent: 64,
	})
	if err != nil {
		return fmt.Errorf("create runtime host protocol server: %w", err)
	}
	grpcServer := grpc.NewServer(loader.RuntimeHostGRPCServerOptions()...)
	runtimev1.RegisterRuntimeHostServer(grpcServer, protocolServer)
	listener, err := listenRuntimeHost(address)
	if err != nil {
		return err
	}
	defer listener.Close()
	serveDone := make(chan error, 1)
	go func() { serveDone <- grpcServer.Serve(listener) }()
	_, _ = fmt.Fprintln(output, "Runtime Host 已就绪，等待内核连接")
	select {
	case <-ctx.Done():
		observe.Info(ctx, "收到停止信号，正在关闭 Runtime Host")
		grpcServer.GracefulStop()
		return nil
	case err := <-serveDone:
		if err == nil {
			return nil
		}
		return err
	}
}

// listenRuntimeHost 按地址形态监听：unix: 前缀为 Unix socket，其余为 loopback TCP。
func listenRuntimeHost(address string) (net.Listener, error) {
	if strings.HasPrefix(address, "unix:") {
		socketPath := strings.TrimPrefix(address, "unix:")
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o750); err != nil {
			return nil, fmt.Errorf("create runtime host socket directory: %w", err)
		}
		if info, err := os.Lstat(socketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(socketPath)
		}
		return net.Listen("unix", socketPath)
	}
	return net.Listen("tcp", address)
}

func run() (resultErr error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err := observe.Configure(observe.Config{
		Service:        "ailuo-kernel",
		Environment:    config.environment,
		Level:          config.logLevel,
		Format:         config.logFormat,
		AddSource:      config.logSource,
		MaxValueLength: config.logMaxValueLength,
		Writer:         os.Stdout,
	}); err != nil {
		return err
	}
	ctx = observe.With(ctx, observe.Component("kernel"))
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
	defer func() {
		resultErr = errors.Join(resultErr, store.Close())
	}()
	observe.Info(ctx, "统一数据库已经就绪")
	if config.loadDemoData {
		if err := bootstrap.LoadDemoBusData(ctx, store, time.Now()); err != nil {
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
		SystemPrompt: "你是AI珞（爱珞）校园综合服务智能体。准确理解用户问题，按需调用可用 Capability，并使用简洁中文回答。",
		Timezone:     "Asia/Shanghai", MaxSteps: 8, MaxToolCalls: 8,
		MaxInputTokens: 32768, MaxOutputTokens: 8192, MaxTotalTokens: 40960,
		MaxOutputBytes: 65536, ProviderTimeout: 30 * time.Second,
		EnabledCapabilities: []string{
			campus.BusStopSearchCapabilityID,
			campus.BusRouteListCapabilityID,
			campus.BusJourneySearchCapabilityID,
			subagent.CapabilityID,
		},
	})
	if err != nil {
		return fmt.Errorf("ensure campus App config: %w", err)
	}
	observe.Info(ctx, "校园 App 持久配置已经就绪",
		observe.StringAttr("app_id", app.AppID),
		observe.StringAttr("config_revision", app.Revision),
		observe.Int64Attr("config_generation", int64(app.Generation)),
		observe.BoolAttr("created", created),
	)
	policy, err := appconfig.NewPersistentPolicy(store)
	if err != nil {
		return err
	}
	// 确认与副作用治理：持久确认服务注入 Dispatcher，凡声明 write/external 副作用
	// 的 Capability 在未获批准前 fail-closed（缺确认标识或验证失败一律拒绝执行）。
	confirmations := confirmation.NewService(store)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.WithMaxCallDepth(16), runtime.WithIdempotencyStore(store), runtime.WithConfirmationVerifier(confirmations))
	// 校园服务以 hosted 包形态装配：内置 wasm 工件在进程内沙箱执行，
	// 权威存储经宿主函数投影（App 隔离在宿主侧强制），Dispatcher 治理保持不变。
	campusHost, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:  campus.ReadArtifact,
		HostFunctions: campus.HostedFunctions(store),
	})
	if err != nil {
		return fmt.Errorf("create campus hosted boundary: %w", err)
	}
	campusManager, err := loader.New(map[string]loader.Host{loader.ModeHosted: campusHost})
	if err != nil {
		return fmt.Errorf("create campus runtime loader: %w", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, campusManager.Shutdown(shutdownContext))
	}()
	if err := loader.RegisterInstalled(campusManager, reg, []loader.InstalledRecord{{
		Runtime:      campus.Manifest(),
		Tools:        campus.ToolSpecs(),
		Service:      campus.ServiceSpec(),
		Capabilities: campus.CapabilitySpecs(),
	}}); err != nil {
		return fmt.Errorf("register campus hosted package: %w", err)
	}
	// campus 是 pin 的内置包：启动预热，编译失败则内核拒绝就绪（fail-closed）。
	if err := campusManager.Warmup(ctx, []string{campus.ServiceID}, 1); err != nil {
		return fmt.Errorf("warm campus hosted package: %w", err)
	}
	runtimeManager, err := configureInstalledRuntimes(ctx, config, reg)
	if err != nil {
		return err
	}
	defer func() {
		if runtimeManager == nil {
			return
		}
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, runtimeManager.Shutdown(shutdownContext))
	}()
	var managedAgent *agenthost.Host
	if config.manageAgent {
		observe.Info(ctx, "正在启动 Python AI Agent 进程",
			observe.BoolAttr("managed_process", true),
		)
		host, err := agenthost.Start(ctx, config.pythonPath, config.agentAddress, os.Stdout, os.Stderr)
		if err != nil {
			return err
		}
		managedAgent = host
		defer func() {
			stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, host.Stop(stopContext))
		}()
		go func() {
			<-host.Done()
			if processErr := host.Err(); processErr != nil && ctx.Err() == nil {
				observe.Error(ctx, "Python AI Agent 进程异常退出", processErr)
				stop()
			}
		}()
	}
	dialContext, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	defer cancelDial()
	connection, agentClient, err := agenthost.Dial(dialContext, config.agentAddress)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, connection.Close())
	}()
	observe.Info(ctx, "已经连接 Python AI Agent")

	orchestrator := kernelecho.NewOrchestrator(agentClient, reg, dispatcher, policy, store, kernelecho.Config{
		AppID:              campus.AppID,
		Model:              app.Model,
		SystemPrompt:       app.SystemPrompt,
		Timezone:           app.Timezone,
		MaxSteps:           app.MaxSteps,
		MaxToolCalls:       app.MaxToolCalls,
		MaxInputTokens:     app.MaxInputTokens,
		MaxOutputTokens:    app.MaxOutputTokens,
		MaxTotalTokens:     app.MaxTotalTokens,
		MaxOutputBytes:     app.MaxOutputBytes,
		MaxCostMicrousd:    app.MaxCostMicrousd,
		ProviderTimeout:    app.ProviderTimeout,
		ModelConfigVersion: app.Revision,
		AppConfigSource:    store,
		RunTimeout:         90 * time.Second,
		MaxRunAttempts:     3,
		QueueCapacity:      128,
	})
	if err := subagent.Register(reg, orchestrator); err != nil {
		return fmt.Errorf("register governed Subagent service: %w", err)
	}
	observe.Info(ctx, "校园服务与受治理子 Run Capability 注册完成",
		observe.IntAttr("service_count", len(reg.Services())),
		observe.IntAttr("tool_count", len(reg.Tools())),
		observe.IntAttr("capability_count", len(reg.Capabilities())),
	)
	// 后台任务调度器：持久任务状态机以 SQLite 为唯一事实源；首个消费者是
	// 确认过期清扫（governance.confirmation.expiry），清扫链由任务自续，启动播种一轮。
	taskTypes := task.NewTypeRegistry()
	taskScheduler := task.NewScheduler(store, taskTypes, task.Config{})
	if err := registerGovernanceTaskTypes(taskTypes, confirmations, taskScheduler, confirmationSweepInterval); err != nil {
		return fmt.Errorf("register governance task types: %w", err)
	}
	if err := taskScheduler.Start(ctx); err != nil {
		return fmt.Errorf("start background task scheduler: %w", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, taskScheduler.Shutdown(shutdownContext))
	}()
	if err := seedConfirmationSweep(ctx, taskScheduler, campus.AppID, confirmationSweepInterval); err != nil {
		return fmt.Errorf("seed confirmation sweep: %w", err)
	}
	observe.Info(ctx, "后台任务调度器与确认过期清扫已就绪",
		observe.StringAttr("app_id", campus.AppID),
		observe.StringAttr("sweep_type", confirmationSweepType),
		observe.Int64Attr("sweep_interval_ms", confirmationSweepInterval.Milliseconds()),
	)
	readiness := health.Combined{store, agenthost.NewAppHealthChecker(agentClient, store, campus.AppID)}
	// 平台接入统一入口：标准消息 → 身份解析 → 会话/消息入库 → Echo。
	// 当前 Web 演示无身份（匿名发送者），身份服务在携带平台身份的消息到达时才会被调用。
	identities := identity.NewService(store)
	platformHub := access.NewHub(campus.AppID, store, identities)
	webAccess := web.NewServer(ctx, orchestrator, store, readiness, reg, policy, campus.AppID, platformHub)
	// 平台事件入口独立挂载：/api/v1/ingress/{platform} 由平台适配器规范化事件驱动，
	// 其余路径全部交给 Web Access（健康检查、Echo/SSE、演示页面）。
	ingressServer := ingress.NewServer(campus.AppID, platformHub, orchestrator)
	if _, err := webAccess.Recover(ctx); err != nil {
		return fmt.Errorf("recover durable runs: %w", err)
	}
	outer := http.NewServeMux()
	outer.Handle("/api/v1/ingress/", ingressServer.Handler())
	outer.Handle("/", webAccess.Handler())
	server := &http.Server{
		Addr:              config.httpAddress,
		Handler:           outer,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErrors := make(chan error, 1)
	go func() {
		observe.Info(ctx, "Web Access 开始监听",
			observe.StringAttr("address", config.httpAddress),
		)
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		observe.Info(ctx, "收到停止信号，正在关闭服务")
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		webAccess.StopAccepting()
		httpErr := server.Shutdown(shutdownContext)
		runErr := webAccess.Shutdown(shutdownContext)
		var runtimeErr error
		if runtimeManager != nil {
			runtimeErr = runtimeManager.Shutdown(shutdownContext)
			runtimeManager = nil
		}
		var agentErr error
		if managedAgent != nil {
			agentErr = managedAgent.Stop(shutdownContext)
		}
		if err := errors.Join(httpErr, runErr, runtimeErr, agentErr); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
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

type config struct {
	httpAddress        string
	agentAddress       string
	pythonPath         string
	databasePath       string
	model              string
	manageAgent        bool
	loadDemoData       bool
	environment        string
	logLevel           slog.Level
	logFormat          string
	logSource          bool
	logMaxValueLength  int
	runtimeInstallRoot string
	runtimeHostAddress string
}

func loadConfig() (config, error) {
	manageAgent, err := envBool("AILUO_MANAGE_AGENT", true)
	if err != nil {
		return config{}, err
	}
	loadDemoData, err := envBool("AILUO_LOAD_DEMO_DATA", false)
	if err != nil {
		return config{}, err
	}
	logSource, err := envBool("AILUO_LOG_SOURCE", false)
	if err != nil {
		return config{}, err
	}
	if logSource {
		return config{}, fmt.Errorf("configuration error: AILUO_LOG_SOURCE must be false because filesystem paths are not allowed in logs")
	}
	logLevel, err := observe.ParseLevel(envOr("AILUO_LOG_LEVEL", "info"))
	if err != nil {
		return config{}, err
	}
	logMaxValueLength, err := envInt("AILUO_LOG_MAX_VALUE_LENGTH", 4096)
	if err != nil {
		return config{}, err
	}
	result := config{
		httpAddress:        envOr("AILUO_HTTP_ADDRESS", "127.0.0.1:8080"),
		agentAddress:       envOr("AILUO_AGENT_ADDRESS", "127.0.0.1:50051"),
		pythonPath:         envOr("AILUO_PYTHON", agenthost.DefaultPythonPath(".")),
		databasePath:       envOr("AILUO_DATABASE_PATH", "var/ailuo.db"),
		model:              os.Getenv("AILUO_MODEL"),
		manageAgent:        manageAgent,
		loadDemoData:       loadDemoData,
		environment:        envOr("AILUO_ENVIRONMENT", "development"),
		logLevel:           logLevel,
		logFormat:          envOr("AILUO_LOG_FORMAT", "console"),
		logSource:          logSource,
		logMaxValueLength:  logMaxValueLength,
		runtimeInstallRoot: os.Getenv("AILUO_RUNTIME_INSTALL_ROOT"),
		runtimeHostAddress: os.Getenv("AILUO_RUNTIME_HOST_ADDRESS"),
	}
	if result.model == "" {
		return config{}, fmt.Errorf("configuration error: AILUO_MODEL is required")
	}
	if result.loadDemoData && (strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")) {
		return config{}, fmt.Errorf("configuration error: AILUO_LOAD_DEMO_DATA must be false in production")
	}
	if result.runtimeInstallRoot != "" &&
		(!filepath.IsAbs(result.runtimeInstallRoot) || filepath.Clean(result.runtimeInstallRoot) != result.runtimeInstallRoot) {
		return config{}, fmt.Errorf("configuration error: AILUO_RUNTIME_INSTALL_ROOT must be a clean absolute path")
	}
	if result.manageAgent {
		secretFile := os.Getenv("AILUO_MODEL_API_KEY_FILE")
		rawSecretConfigured := os.Getenv("AILUO_MODEL_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != ""
		production := strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")
		if production && rawSecretConfigured {
			return config{}, fmt.Errorf("configuration error: production model credentials must use AILUO_MODEL_API_KEY_FILE")
		}
		if secretFile == "" && !rawSecretConfigured {
			return config{}, fmt.Errorf("configuration error: AILUO_MODEL_API_KEY_FILE, AILUO_MODEL_API_KEY or OPENAI_API_KEY is required when Go manages the AI Agent")
		}
		if secretFile != "" {
			if err := validateSecretFile(secretFile); err != nil {
				return config{}, err
			}
		}
	}
	return result, nil
}

func configureInstalledRuntimes(ctx context.Context, cfg config, target *registry.Registry) (*loader.Manager, error) {
	if cfg.runtimeInstallRoot == "" {
		return nil, nil
	}
	catalog, err := loader.NewCatalog(cfg.runtimeInstallRoot)
	if err != nil {
		return nil, fmt.Errorf("create installed runtime catalog: %w", err)
	}
	records, err := catalog.Discover(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover installed runtimes: %w", err)
	}
	if len(records) == 0 {
		observe.Info(ctx, "运行时安装目录校验完成",
			observe.IntAttr("runtime_count", 0),
		)
		return nil, nil
	}
	hosts := make(map[string]loader.Host, 2)
	hostedCount := 0
	isolatedCount := 0
	pinnedIDs := make([]string, 0, len(records))
	for _, record := range records {
		switch record.Runtime.Mode {
		case loader.ModeHosted:
			hostedCount++
		case loader.ModeIsolated:
			isolatedCount++
		default:
			return nil, loader.ErrUnsupportedMode
		}
		if record.Runtime.Pin {
			pinnedIDs = append(pinnedIDs, record.Runtime.ID)
		}
	}
	if hostedCount > 0 {
		if cfg.runtimeHostAddress == "" {
			return nil, fmt.Errorf("configuration error: AILUO_RUNTIME_HOST_ADDRESS is required for installed hosted runtimes")
		}
		host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
			Mode: loader.ModeHosted, Address: cfg.runtimeHostAddress,
			VerifyInstalled: catalog.VerifyRuntime,
			DialTimeout:     10 * time.Second,
			MaxRuntimes:     hostedCount,
			MaxConcurrent:   64,
		})
		if err != nil {
			return nil, fmt.Errorf("configure hosted runtime boundary: %w", err)
		}
		hosts[loader.ModeHosted] = host
	}
	if isolatedCount > 0 {
		host, err := loader.NewIsolatedProcessHost(loader.IsolatedProcessHostConfig{
			ResolveInstalled: catalog.ResolveProcess,
			VerifyInstalled:  catalog.VerifyProcess,
			DialTimeout:      10 * time.Second,
			StopGrace:        5 * time.Second,
			TerminateGrace:   2 * time.Second,
		})
		if err != nil {
			return nil, fmt.Errorf("configure isolated runtime boundary: %w", err)
		}
		hosts[loader.ModeIsolated] = host
	}
	manager, err := loader.New(hosts)
	if err != nil {
		return nil, fmt.Errorf("create runtime loader: %w", err)
	}
	if err := loader.RegisterInstalled(manager, target, records); err != nil {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return nil, errors.Join(fmt.Errorf("register installed runtimes: %w", err), manager.Shutdown(shutdownContext))
	}
	if len(pinnedIDs) > 0 {
		concurrency := min(len(pinnedIDs), 4)
		if err := manager.Warmup(ctx, pinnedIDs, concurrency); err != nil {
			shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return nil, errors.Join(fmt.Errorf("warm installed runtimes: %w", err), manager.Shutdown(shutdownContext))
		}
	}
	observe.Info(ctx, "已安装运行时恢复完成",
		observe.IntAttr("runtime_count", len(records)),
		observe.IntAttr("hosted_count", hostedCount),
		observe.IntAttr("isolated_count", isolatedCount),
		observe.IntAttr("pinned_count", len(pinnedIDs)),
	)
	return manager, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("configuration error: %s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("配置错误：%s 必须是正整数", name)
	}
	return parsed, nil
}
