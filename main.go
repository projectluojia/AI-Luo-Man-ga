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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
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
	"github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/agent"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/prompt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"

	"github.com/joho/godotenv"
	"google.golang.org/grpc"
)

func main() {
	loadDotEnv()
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

// loadDotEnv 从工作目录读取 .env 并载入进程环境：已存在的环境变量优先，
// 缺失项才由 .env 补足；.env 不存在时静默跳过（显式环境变量仍然可用）。
func loadDotEnv() {
	if err := godotenv.Load(); err != nil {
		if os.IsNotExist(err) {
			return
		}
		observe.Error(context.Background(), "读取 .env 失败", err)
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
		store, err := sqlite.Open(*database)
		if err != nil {
			return true, err
		}
		provisionErr := provisionIdentity(ctx, store, identityProvision{
			AppID: *appID, UserID: *userID,
			Platform: *platform, PlatformSpaceID: *space, PlatformUserID: *platformUser,
		})
		closeErr := store.Close()
		if err := errors.Join(provisionErr, closeErr); err != nil {
			return true, err
		}
		_, err = fmt.Fprintf(output, "身份开通完成：user_id=%s app=%s 绑定平台=%s\n", *userID, *appID, *platform)
		return true, err
	case "identity-unbind":
		flags := flag.NewFlagSet("identity-unbind", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		database := flags.String("database", "", "SQLite 数据库绝对路径")
		appID := flags.String("app", campus.AppID, "App 标识")
		platform := flags.String("platform", "", "外部平台标识")
		space := flags.String("space", "", "外部平台空间标识")
		platformUser := flags.String("platform-user", "", "外部平台用户标识")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *database == "" || *platform == "" || *space == "" || *platformUser == "" {
			return true, fmt.Errorf("configuration error: identity-unbind requires --database, --platform, --space and --platform-user")
		}
		if !filepath.IsAbs(*database) {
			return true, fmt.Errorf("configuration error: identity-unbind requires an absolute --database path")
		}
		store, err := sqlite.Open(*database)
		if err != nil {
			return true, err
		}
		unbindErr := identity.NewService(store).UnbindExternalIdentity(ctx, *appID, *platform, *space, *platformUser)
		closeErr := store.Close()
		if err := errors.Join(unbindErr, closeErr); err != nil {
			return true, err
		}
		_, err = fmt.Fprintf(output, "身份解绑完成：app=%s 平台=%s space=%s platform_user=%s\n", *appID, *platform, *space, *platformUser)
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
}

// provisionIdentity 幂等开通内部用户并写入空角色成员关系；可选绑定一个外部平台身份。
// 用户已存在、同一外部身份重复绑定同一用户时都视为成功重放。
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
		AppID: request.AppID, UserID: request.UserID,
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
	backend, err := loader.NewHostedRuntimeBackend(loader.WasmHostConfig{ReadArtifact: catalog.ReadArtifact})
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

func run() error {
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
	if err := seedLocalConfigFromEnvironment(manager, baseConfig); err != nil {
		return err
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
			return nil
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
				observe.Error(ctx, "旧配置内核关闭时发生错误", err)
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
	// 会话正文与附件 Blob 存储：与数据库同目录的 blobs 子目录。
	// 上下文装配经 session.Service 统一读取内联与 Blob 模式的消息正文。
	blobStore, err := blob.Open(filepath.Join(filepath.Dir(config.databasePath), "blobs"), session.MaxMessageContentBytes)
	if err != nil {
		return fmt.Errorf("open blob storage: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, blobStore.Close())
	}()
	sessionService, err := session.NewService(store, blobStore)
	if err != nil {
		return fmt.Errorf("create session service: %w", err)
	}
	if config.loadDemoData {
		if err := demo.LoadBusData(ctx, store, time.Now()); err != nil {
			return fmt.Errorf("load demo bus data: %w", err)
		}
		observe.Warn(ctx, "已载入非权威校巴演示数据",
			observe.BoolAttr("authoritative", false),
			observe.StringAttr("source", "demo-fixture-not-zhihui-luojia"),
		)
	}

	baseSystemPrompt := config.baseSystemPrompt
	if strings.TrimSpace(baseSystemPrompt) == "" {
		baseSystemPrompt = promptcatalog.DefaultBaseSystemPrompt
	}
	channelPrompts := config.channelPrompts
	if len(channelPrompts) == 0 {
		channelPrompts = promptcatalog.DefaultChannelPrompts()
	}
	reg := registry.New()
	app, created, err := store.Ensure(ctx, appconfig.Config{
		AppID: campus.AppID, Enabled: true, Model: config.model,
		SystemPrompt:    baseSystemPrompt,
		ChannelPrompts:  channelPrompts,
		Timezone:        config.agentRun.Timezone,
		MaxSteps:        config.agentRun.MaxSteps,
		MaxToolCalls:    config.agentRun.MaxToolCalls,
		MaxInputTokens:  config.agentRun.MaxInputTokens,
		MaxOutputTokens: config.agentRun.MaxOutputTokens,
		MaxTotalTokens:  config.agentRun.MaxTotalTokens,
		MaxOutputBytes:  config.agentRun.MaxOutputBytes,
		ProviderTimeout: config.modelRequestTimeout,
		EnabledCapabilities: []string{
			campus.BusStopSearchCapabilityID,
			campus.BusRouteListCapabilityID,
			campus.BusJourneySearchCapabilityID,
			agent.CapabilityID,
			agent.StatusCapabilityID,
			promptservice.PreferenceGetID,
			promptservice.PreferenceSetID,
			promptservice.PreferenceResetID,
		},
	})
	if err != nil {
		return fmt.Errorf("ensure campus App config: %w", err)
	}
	if !created {
		// 既有部署升级：把 V2 人格/渠道提示与 prompt 偏好 Capability 补齐到当前配置。
		// CompareAndSwap 在内容未变化时直接返回原修订，不会反复增加 generation。
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
		replacement.SystemPrompt = baseSystemPrompt
		replacement.ChannelPrompts = channelPrompts
		replacement.EnabledCapabilities = ensurePromptCapabilities(app.EnabledCapabilities)
		app, err = store.CompareAndSwap(ctx, app.Generation, replacement)
		if err != nil {
			return fmt.Errorf("migrate campus App prompt configuration: %w", err)
		}
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
	promptCatalog := config.promptCatalog
	if promptCatalog.IsZero() {
		promptCatalog = promptcatalog.Default()
	}
	promptService := promptservice.NewService(promptCatalog, store)
	if err := promptservice.Register(reg, promptService); err != nil {
		return fmt.Errorf("register prompt Service: %w", err)
	}
	// 确认与副作用治理：持久确认服务注入 Dispatcher，凡声明 write/external 副作用
	// 的 Capability 在未获批准前 fail-closed（缺确认标识或验证失败一律拒绝执行）。
	confirmations := confirmation.NewService(store, confirmation.Config{})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		MaxCallDepth:         config.orchestration.MaxCallDepth,
		IdempotencyStore:     store,
		ConfirmationVerifier: confirmations,
	})
	// 统一 Loader：所有运行模式共享一个 Manager，清单在注册时按 Verify 精确绑定
	// 到唯一宿主。内置 campus（hosted 沙箱）、内置 agent（isolated 进程）与
	// installed 包（hosted/isolated）同池管理，不再按包分叉多个 Loader。
	campusHost, err := campus.Host(store)
	if err != nil {
		return fmt.Errorf("create campus hosted boundary: %w", err)
	}
	workDir, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("resolve agent working directory: %w", err)
	}
	agentHost, err := agent.NewHost(agent.Config{
		Resolve: func(context.Context) (agent.Spec, error) {
			return agent.Spec{
				PythonPath: config.pythonPath,
				WorkDir:    workDir,
				Address:    config.agentAddress,
				Env:        agentEnvironment(config),
				Limits:     loader.ProcessLimits{},
			}, nil
		},
		Spawn:          config.manageAgent,
		Model:          app.Model,
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		DialTimeout:    secondsDuration(config.agentProcess.DialTimeoutSeconds),
		StopGrace:      secondsDuration(config.agentProcess.StopGraceSeconds),
		TerminateGrace: secondsDuration(config.agentProcess.TerminateGraceSeconds),
	})
	if err != nil {
		return fmt.Errorf("create built-in agent runtime: %w", err)
	}
	installedHosts, installedRecords, err := configureInstalledRuntimes(ctx, config)
	if err != nil {
		return err
	}
	hosts := make([]loader.Host, 0, 2+len(installedHosts))
	hosts = append(hosts, campusHost, agentHost)
	hosts = append(hosts, installedHosts...)
	runtimeLoader, err := loader.New(hosts...)
	if err != nil {
		return fmt.Errorf("create runtime loader: %w", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, runtimeLoader.Shutdown(shutdownContext))
	}()
	// 内置包与 installed 包统一注册：campus 与内置 agent 各以安装清单形式经同一
	// RegisterInstalled 路径注册（agent 记录只携带运行时清单，其 Service agent.run
	// 依赖 Orchestrator，在内核装配完成后单独注册），随后注册 installed 目录。
	if err := loader.RegisterInstalled(ctx, runtimeLoader, reg, []loader.InstalledRecord{campus.Record(), agent.Record(agentHost)}); err != nil {
		return fmt.Errorf("register built-in packages: %w", err)
	}
	if len(installedRecords) > 0 {
		if err := loader.RegisterInstalled(ctx, runtimeLoader, reg, installedRecords); err != nil {
			return fmt.Errorf("register installed runtimes: %w", err)
		}
	}
	// 预热全部声明 pin 的运行时（内置 campus/agent 与 installed pin）：编译/启动
	// 失败则内核拒绝就绪（fail-closed）。预热清单由各清单声明推导，不再硬编码。
	pinnedRuntimes := runtimeLoader.Pinned()
	if err := runtimeLoader.Warmup(ctx, pinnedRuntimes, min(len(pinnedRuntimes), 4)); err != nil {
		return fmt.Errorf("warm pinned runtimes: %w", err)
	}
	// 内核按执行者契约使用 AI 执行者：Manager 按清单角色解析本 Deployment
	// 唯一执行者运行时并返回租约。任何实现 executor 契约的运行时（LLM 智能体、
	// 规划器、其他语言实现）都可充当该角色，内核不依赖具体实现或语言。
	executorLease, err := runtimeLoader.Executor(ctx)
	if err != nil {
		return fmt.Errorf("resolve AI executor runtime: %w", err)
	}
	// 租约在函数返回时先于 runtimeLoader.Shutdown 释放（defer 逆序）。
	defer executorLease.Release()
	executorRuntime := executorLease.Runtime()
	clientProvider, ok := executorRuntime.(executor.ClientProvider)
	if !ok {
		return fmt.Errorf("executor runtime does not expose an executor client")
	}
	executorClient := clientProvider.Client()
	if config.manageAgent {
		// 受监督进程异常退出 → 内核 fail-closed 停止（连接模式不拥有进程）。
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
		observe.StringAttr("runtime_id", executorLease.ID()),
		observe.StringAttr("address", config.agentAddress),
		observe.BoolAttr("managed_process", config.manageAgent),
	)

	orchestrator := kernelecho.NewOrchestrator(executorClient, reg, dispatcher, policy, store, kernelecho.Config{
		AppID:           campus.AppID,
		AppConfigSource: store,
		Context:         sessionService,
		ContextBudget: contextasm.Budget{
			MaxMessages:    config.contextAssembly.MaxMessages,
			MaxCharsPerMsg: config.contextAssembly.MaxCharsPerMsg,
			MaxTotalChars:  config.contextAssembly.MaxTotalChars,
			MaxPromptBytes: config.contextAssembly.MaxPromptBytes,
		},
		Prompts:        promptServiceRenderer{promptService},
		RunTimeout:     secondsDuration(config.orchestration.RunTimeoutSeconds),
		MaxRunAttempts: config.orchestration.MaxRunAttempts,
		QueueCapacity:  config.orchestration.QueueCapacity,
		MaxChildRuns:   int(config.agentRun.MaxChildRuns),
	})
	if err := agent.Register(reg, orchestrator); err != nil {
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
	confirmationSweepInterval := secondsDuration(config.governance.ConfirmationSweepSeconds)
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
	readiness := health.Combined{store, health.ExecutorAppChecker{Client: executorClient, Source: store, AppID: campus.AppID}}
	// 平台接入统一入口：标准消息 → 身份解析 → 会话/消息入库 → Echo。
	// Web 登录边界尚未接入时聊天创建入口返回 401，不降级为匿名用户。
	identities := identity.NewService(store)
	platformHub, err := access.NewHub(campus.AppID, store, identities)
	if err != nil {
		return fmt.Errorf("configure platform access hub: %w", err)
	}
	qqProvisioner, err := qq.NewProvisioner(identities)
	if err != nil {
		return fmt.Errorf("configure QQ identity provisioner: %w", err)
	}
	qqEvents := access.NewEventHub()
	qqManager, err := qq.NewManager(store, func(settings qqsettings.Settings, connectionChange func(bool)) (qq.Runner, error) {
		return qq.New(qq.Config{
			AppID: settings.AppID, WSURL: settings.WSURL, Token: config.qqToken, BotQQID: settings.BotQQID,
			AllowedGroupIDs: settings.AllowedGroupIDs, AllowedPrivateUserIDs: settings.AllowedPrivateUserIDs,
			QuickReplies: config.qqQuickReplies, PokeReplies: config.qqPokeReplies,
			Provisioner: qqProvisioner, OnConnectionChange: connectionChange,
			DialTimeout:    secondsDuration(config.qqConnection.DialTimeoutSeconds),
			ReconnectDelay: secondsDuration(config.qqConnection.ReconnectDelaySeconds),
			RunTimeout:     secondsDuration(config.qqConnection.RunTimeoutSeconds),
		}, platformHub, qqEvents, orchestrator, store)
	}, secondsDuration(config.qqConnection.ManagerStopTimeoutSeconds))
	if err != nil {
		return fmt.Errorf("configure QQ access manager: %w", err)
	}
	webAccess := web.NewServer(ctx, orchestrator, store, readiness, reg, policy, campus.AppID, platformHub,
		web.WithEventHub(qqEvents),
		web.WithScheduler(config.scheduler.Workers, time.Duration(config.scheduler.PollMs)*time.Millisecond, config.scheduler.BatchSize))
	// 平台事件入口独立挂载：/api/v1/ingress/{platform} 由平台适配器规范化事件驱动，
	// 其余路径全部交给 Web Access（健康检查、Echo/SSE、演示页面）。
	ingressServer := ingress.NewServer(campus.AppID, platformHub, orchestrator)
	if _, err := webAccess.Recover(ctx); err != nil {
		return fmt.Errorf("recover durable runs: %w", err)
	}
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
	qqCurrent, _, err := qqManager.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("read QQ access configuration: %w", err)
	}
	if !qqsettings.EqualContent(qqCurrent, qqDesired) {
		if _, _, err := qqManager.Update(ctx, qqCurrent.Generation, qqDesired); err != nil {
			return fmt.Errorf("apply QQ access configuration: %w", err)
		}
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, qqManager.Shutdown(shutdownContext))
	}()
	qqCurrent, qqStatus, _ := qqManager.Snapshot(ctx)
	observe.Info(ctx, "QQ Access 管理器已就绪",
		observe.BoolAttr("enabled", qqCurrent.Enabled),
		observe.BoolAttr("running", qqStatus.Running),
		observe.IntAttr("allowed_group_count", len(qqCurrent.AllowedGroupIDs)),
		observe.IntAttr("allowed_private_user_count", len(qqCurrent.AllowedPrivateUserIDs)),
	)
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
	localConfig.SetRuntime("ready", "内核与接入服务正在运行", localConfig.Snapshot().Settings.Revision)
	select {
	case <-ctx.Done():
		observe.Info(ctx, "收到停止信号，正在关闭服务")
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		webAccess.StopAccepting()
		httpErr := server.Shutdown(shutdownContext)
		runErr := webAccess.Shutdown(shutdownContext)
		if err := errors.Join(httpErr, runErr); err != nil {
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
	httpAddress         string
	configUIAddress     string
	localConfigRoot     string
	agentAddress        string
	pythonPath          string
	databasePath        string
	model               string
	modelBaseURL        string
	modelAPIKey         string
	modelAPIKeyFile     string
	modelRequestTimeout time.Duration
	modelReadyTimeout   time.Duration
	modelMaxRetries     int
	modelRetryBase      time.Duration
	modelRetryMax       time.Duration
	modelRequestsMinute int
	modelMaxConcurrency int
	manageAgent         bool
	loadDemoData        bool
	environment         string
	logLevel            slog.Level
	logFormat           string
	logSource           bool
	logMaxValueLength   int
	runtimeInstallRoot  string
	runtimeHostAddress  string
	qqWSURL             string
	qqEnabled           bool
	qqToken             string
	qqBotID             string
	qqAllowedGroupIDs   []string
	qqAllowedPrivateIDs []string
	qqQuickReplies      []qq.QuickReply
	qqPokeReplies       []string
	promptCatalog       promptcatalog.Catalog
	baseSystemPrompt    string
	channelPrompts      map[string]string
	agentRun            controlconfig.AgentRunSettings
	orchestration       controlconfig.OrchestrationSettings
	contextAssembly     controlconfig.ContextAssemblySettings
	scheduler           controlconfig.SchedulerSettings
	qqConnection        controlconfig.QQConnectionSettings
	agentProcess        controlconfig.AgentProcessSettings
	governance          controlconfig.GovernanceSettings
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
	requestTimeout, err := envFloat("AILUO_MODEL_TIMEOUT_SECONDS", 30)
	if err != nil {
		return config{}, err
	}
	readinessTimeout, err := envFloat("AILUO_MODEL_READINESS_TIMEOUT_SECONDS", 3)
	if err != nil {
		return config{}, err
	}
	retryBase, err := envFloat("AILUO_MODEL_RETRY_BASE_SECONDS", 0.25)
	if err != nil {
		return config{}, err
	}
	retryMax, err := envFloat("AILUO_MODEL_RETRY_MAX_SECONDS", 2)
	if err != nil {
		return config{}, err
	}
	maxRetries, err := envIntRange("AILUO_MODEL_MAX_RETRIES", 2, 0, 5)
	if err != nil {
		return config{}, err
	}
	requestsMinute, err := envIntRange("AILUO_MODEL_REQUESTS_PER_MINUTE", 60, 1, 10_000)
	if err != nil {
		return config{}, err
	}
	maxConcurrency, err := envIntRange("AILUO_MODEL_MAX_CONCURRENCY", 4, 1, 64)
	if err != nil {
		return config{}, err
	}
	result := config{
		httpAddress:         envOr("AILUO_HTTP_ADDRESS", "127.0.0.1:8080"),
		configUIAddress:     envOr("AILUO_CONFIG_UI_ADDRESS", configui.DefaultAddress),
		localConfigRoot:     envOr("AILUO_CONFIG_DIR", "var"),
		agentAddress:        envOr("AILUO_AGENT_ADDRESS", "127.0.0.1:50051"),
		pythonPath:          envOr("AILUO_PYTHON", agent.DefaultPythonPath(".")),
		databasePath:        envOr("AILUO_DATABASE_PATH", "var/ailuo.db"),
		model:               os.Getenv("AILUO_MODEL"),
		modelBaseURL:        os.Getenv("AILUO_MODEL_BASE_URL"),
		modelAPIKey:         envOr("AILUO_MODEL_API_KEY", os.Getenv("OPENAI_API_KEY")),
		modelAPIKeyFile:     os.Getenv("AILUO_MODEL_API_KEY_FILE"),
		modelRequestTimeout: time.Duration(requestTimeout * float64(time.Second)),
		modelReadyTimeout:   time.Duration(readinessTimeout * float64(time.Second)),
		modelMaxRetries:     maxRetries,
		modelRetryBase:      time.Duration(retryBase * float64(time.Second)),
		modelRetryMax:       time.Duration(retryMax * float64(time.Second)),
		modelRequestsMinute: requestsMinute,
		modelMaxConcurrency: maxConcurrency,
		manageAgent:         manageAgent,
		loadDemoData:        loadDemoData,
		environment:         envOr("AILUO_ENVIRONMENT", "development"),
		logLevel:            logLevel,
		logFormat:           envOr("AILUO_LOG_FORMAT", "console"),
		logSource:           logSource,
		logMaxValueLength:   logMaxValueLength,
		runtimeInstallRoot:  os.Getenv("AILUO_RUNTIME_INSTALL_ROOT"),
		runtimeHostAddress:  os.Getenv("AILUO_RUNTIME_HOST_ADDRESS"),
		qqWSURL:             os.Getenv("AILUO_QQ_WS_URL"),
		qqEnabled:           os.Getenv("AILUO_QQ_WS_URL") != "",
		qqToken:             os.Getenv("AILUO_QQ_WS_TOKEN"),
		qqBotID:             os.Getenv("AILUO_QQ_BOT_ID"),
		qqAllowedGroupIDs:   envCSV("AILUO_QQ_ALLOWED_GROUP_IDS"),
		qqAllowedPrivateIDs: envCSV("AILUO_QQ_ALLOWED_PRIVATE_USER_IDS"),
	}
	// Agent 进程规格要求绝对 Python 路径（Spawn 模式校验）；默认值与用户配置
	// 都可能为相对路径，统一在装配前解析为绝对路径。
	absolutePython, err := filepath.Abs(result.pythonPath)
	if err != nil {
		return config{}, fmt.Errorf("configuration error: resolve python path: %w", err)
	}
	result.pythonPath = absolutePython
	if !loader.IsLocalRuntimeAddress(result.configUIAddress) {
		return config{}, fmt.Errorf("configuration error: AILUO_CONFIG_UI_ADDRESS must be loopback")
	}
	if result.loadDemoData && (strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")) {
		return config{}, fmt.Errorf("configuration error: AILUO_LOAD_DEMO_DATA must be false in production")
	}
	if result.runtimeInstallRoot != "" &&
		(!filepath.IsAbs(result.runtimeInstallRoot) || filepath.Clean(result.runtimeInstallRoot) != result.runtimeInstallRoot) {
		return config{}, fmt.Errorf("configuration error: AILUO_RUNTIME_INSTALL_ROOT must be a clean absolute path")
	}
	if result.qqWSURL != "" && !strings.HasPrefix(result.qqWSURL, "ws://") && !strings.HasPrefix(result.qqWSURL, "wss://") {
		return config{}, fmt.Errorf("configuration error: AILUO_QQ_WS_URL must be a ws:// or wss:// address")
	}
	if result.manageAgent {
		secretFile := result.modelAPIKeyFile
		rawSecretConfigured := result.modelAPIKey != ""
		production := strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")
		if production && rawSecretConfigured {
			return config{}, fmt.Errorf("configuration error: production model credentials must use AILUO_MODEL_API_KEY_FILE")
		}
		if secretFile != "" {
			if err := validateSecretFile(secretFile); err != nil {
				return config{}, err
			}
		}
	}
	return result, nil
}

func seedLocalConfigFromEnvironment(manager *controlconfig.Service, base config) error {
	if _, ready := manager.CurrentResolved(); ready || base.model == "" {
		return nil
	}
	modelKey := base.modelAPIKey
	if modelKey == "" && base.modelAPIKeyFile != "" {
		content, err := os.ReadFile(base.modelAPIKeyFile)
		if err != nil {
			return fmt.Errorf("read model secret for configuration migration: %w", err)
		}
		modelKey = strings.TrimSpace(string(content))
	}
	if modelKey == "" {
		return nil
	}
	_, err := manager.Save(controlconfig.SaveInput{
		Model:                        base.model,
		ModelBaseURL:                 base.modelBaseURL,
		ModelAPIKey:                  modelKey,
		ModelRequestTimeoutSeconds:   base.modelRequestTimeout.Seconds(),
		ModelReadinessTimeoutSeconds: base.modelReadyTimeout.Seconds(),
		ModelMaxRetries:              base.modelMaxRetries,
		ModelRetryBaseSeconds:        base.modelRetryBase.Seconds(),
		ModelRetryMaxSeconds:         base.modelRetryMax.Seconds(),
		ModelRequestsPerMinute:       base.modelRequestsMinute,
		ModelMaxConcurrency:          base.modelMaxConcurrency,
		QQEnabled:                    base.qqEnabled,
		QQWSURL:                      base.qqWSURL,
		QQWSToken:                    base.qqToken,
		QQBotID:                      base.qqBotID,
		QQAllowedGroupIDs:            base.qqAllowedGroupIDs,
		QQAllowedPrivateUserIDs:      base.qqAllowedPrivateIDs,
	})
	if err != nil {
		return fmt.Errorf("migrate environment configuration into control plane: %w", err)
	}
	return nil
}

func applyLocalConfig(base config, resolved controlconfig.Resolved) (config, error) {
	settings := resolved.Settings
	base.model = settings.Model
	base.modelBaseURL = settings.ModelBaseURL
	base.modelAPIKeyFile = resolved.ModelAPIKeyFile
	base.modelRequestTimeout = time.Duration(settings.ModelRequestTimeoutSeconds * float64(time.Second))
	base.modelReadyTimeout = time.Duration(settings.ModelReadinessTimeoutSeconds * float64(time.Second))
	base.modelMaxRetries = settings.ModelMaxRetries
	base.modelRetryBase = time.Duration(settings.ModelRetryBaseSeconds * float64(time.Second))
	base.modelRetryMax = time.Duration(settings.ModelRetryMaxSeconds * float64(time.Second))
	base.modelRequestsMinute = settings.ModelRequestsPerMinute
	base.modelMaxConcurrency = settings.ModelMaxConcurrency
	base.qqWSURL = settings.QQWSURL
	base.qqEnabled = settings.QQEnabled
	base.qqBotID = settings.QQBotID
	base.qqAllowedGroupIDs = append([]string(nil), settings.QQAllowedGroupIDs...)
	base.qqAllowedPrivateIDs = append([]string(nil), settings.QQAllowedPrivateUserIDs...)
	base.qqQuickReplies = make([]qq.QuickReply, 0, len(settings.QQQuickReplies))
	for _, rule := range settings.QQQuickReplies {
		base.qqQuickReplies = append(base.qqQuickReplies, qq.QuickReply{Trigger: rule.Trigger, Reply: rule.Reply})
	}
	base.qqPokeReplies = append([]string(nil), settings.QQPokeReplies...)
	base.promptCatalog = settings.PromptCatalog.Clone()
	base.baseSystemPrompt = settings.BaseSystemPrompt
	base.channelPrompts = cloneStringMap(settings.ChannelPrompts)
	base.agentRun = settings.AgentRun
	base.orchestration = settings.Orchestration
	base.contextAssembly = settings.ContextAssembly
	base.scheduler = settings.Scheduler
	base.qqConnection = settings.QQConnection
	base.agentProcess = settings.AgentProcess
	base.governance = settings.Governance
	base.qqToken = ""
	if resolved.QQWSTokenFile != "" {
		content, err := os.ReadFile(resolved.QQWSTokenFile)
		if err != nil || len(content) > controlconfig.MaxQQTokenBytes {
			return config{}, errors.New("QQ secret file is unavailable")
		}
		base.qqToken = strings.TrimSpace(string(content))
	}
	return base, nil
}

func agentEnvironment(config config) []string {
	return []string{
		"AILUO_ENVIRONMENT=" + config.environment,
		"AILUO_MODEL_API_KEY_FILE=" + config.modelAPIKeyFile,
		"AILUO_MODEL_BASE_URL=" + config.modelBaseURL,
		"AILUO_MODEL_TIMEOUT_SECONDS=" + strconv.FormatFloat(config.modelRequestTimeout.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_READINESS_TIMEOUT_SECONDS=" + strconv.FormatFloat(config.modelReadyTimeout.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_MAX_RETRIES=" + strconv.Itoa(config.modelMaxRetries),
		"AILUO_MODEL_RETRY_BASE_SECONDS=" + strconv.FormatFloat(config.modelRetryBase.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_RETRY_MAX_SECONDS=" + strconv.FormatFloat(config.modelRetryMax.Seconds(), 'f', -1, 64),
		"AILUO_MODEL_REQUESTS_PER_MINUTE=" + strconv.Itoa(config.modelRequestsMinute),
		"AILUO_MODEL_MAX_CONCURRENCY=" + strconv.Itoa(config.modelMaxConcurrency),
	}
}

// configureInstalledRuntimes 发现安装目录中的 installed 包，返回加入统一 Loader
// 的宿主与待注册记录。安装目录未配置时返回空切片（统一 Loader 只装配内置包）；
// 配置了安装目录但没有 hosted 宿主地址时 fail-closed。pin 运行时由各清单声明，
// 预热清单由 runtimeLoader.Pinned() 统一推导，本函数不再返回。
func configureInstalledRuntimes(ctx context.Context, cfg config) ([]loader.Host, []loader.InstalledRecord, error) {
	if cfg.runtimeInstallRoot == "" {
		return nil, nil, nil
	}
	catalog, err := loader.NewCatalog(cfg.runtimeInstallRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("create installed runtime catalog: %w", err)
	}
	records, err := catalog.Discover(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("discover installed runtimes: %w", err)
	}
	if len(records) == 0 {
		observe.Info(ctx, "运行时安装目录校验完成",
			observe.IntAttr("runtime_count", 0),
		)
		return nil, nil, nil
	}
	hosts := make([]loader.Host, 0, 2)
	hostedCount := 0
	isolatedCount := 0
	pinnedCount := 0
	for _, record := range records {
		switch record.Runtime.Mode {
		case loader.ModeHosted:
			hostedCount++
		case loader.ModeIsolated:
			isolatedCount++
		default:
			return nil, nil, loader.ErrUnsupportedMode
		}
		if record.Runtime.Pin {
			pinnedCount++
		}
	}
	if hostedCount > 0 {
		if cfg.runtimeHostAddress == "" {
			return nil, nil, fmt.Errorf("configuration error: AILUO_RUNTIME_HOST_ADDRESS is required for installed hosted runtimes")
		}
		host, err := loader.NewGRPCHost(loader.GRPCHostConfig{
			Mode: loader.ModeHosted, Address: cfg.runtimeHostAddress,
			VerifyInstalled: catalog.VerifyRuntime,
			DialTimeout:     10 * time.Second,
			MaxRuntimes:     hostedCount,
			MaxConcurrent:   64,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("configure hosted runtime boundary: %w", err)
		}
		hosts = append(hosts, host)
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
			return nil, nil, fmt.Errorf("configure isolated runtime boundary: %w", err)
		}
		hosts = append(hosts, host)
	}
	observe.Info(ctx, "已安装运行时发现完成",
		observe.IntAttr("runtime_count", len(records)),
		observe.IntAttr("hosted_count", hostedCount),
		observe.IntAttr("isolated_count", isolatedCount),
		observe.IntAttr("pinned_count", pinnedCount),
	)
	return hosts, records, nil
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

func envIntRange(name string, fallback, minimum, maximum int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("配置错误：%s 超出允许范围", name)
	}
	return parsed, nil
}

func envFloat(name string, fallback float64) (float64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("配置错误：%s 必须是正数", name)
	}
	return parsed, nil
}

func envCSV(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	values := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

type promptServiceRenderer struct {
	service *promptservice.Service
}

func (r promptServiceRenderer) RenderSystemPrompt(ctx context.Context, request kernelecho.PromptRenderRequest) (string, error) {
	return r.service.RenderSystemPrompt(ctx, promptservice.RenderRequest{
		AppID:            request.AppID,
		UserID:           request.UserID,
		BaseSystemPrompt: request.BaseSystemPrompt,
		Channel:          request.Channel,
		ChannelPrompts:   request.ChannelPrompts,
	})
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func ensurePromptCapabilities(existing []string) []string {
	result := append([]string(nil), existing...)
	for _, capabilityID := range []string{
		agent.StatusCapabilityID,
		promptservice.PreferenceGetID,
		promptservice.PreferenceSetID,
		promptservice.PreferenceResetID,
	} {
		if !slices.Contains(result, capabilityID) {
			result = append(result, capabilityID)
		}
	}
	return result
}

func secondsDuration(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}
