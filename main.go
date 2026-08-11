package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/agenthost"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/bootstrap"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/subagent"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
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
			return true, errorsNew("backup requires --database and --destination absolute paths")
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
			return true, errorsNew("validate-backup requires --backup absolute path")
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
			return true, errorsNew("restore requires --backup and --destination absolute paths")
		}
		if err := sqlite.RestoreBackup(ctx, *backup, *destination); err != nil {
			return true, err
		}
		_, err := fmt.Fprintln(output, "SQLite 数据库已恢复并通过完整性校验")
		return true, err
	default:
		return true, errorsNew("unknown command")
	}
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
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.WithMaxCallDepth(16), runtime.WithIdempotencyStore(store))
	if err := campus.Register(reg, dispatcher, store); err != nil {
		return fmt.Errorf("register campus service: %w", err)
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
	readiness := health.Combined{store, agenthost.NewAppHealthChecker(agentClient, store, campus.AppID)}
	access := web.NewServer(ctx, orchestrator, store, readiness, reg, policy, campus.AppID)
	if _, err := access.Recover(ctx); err != nil {
		return fmt.Errorf("recover durable runs: %w", err)
	}
	server := &http.Server{
		Addr:              config.httpAddress,
		Handler:           access.Handler(),
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
		access.StopAccepting()
		httpErr := server.Shutdown(shutdownContext)
		runErr := access.Shutdown(shutdownContext)
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
		return config{}, errorsNew("AILUO_LOG_SOURCE must be false because filesystem paths are not allowed in logs")
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
		return config{}, errorsNew("AILUO_MODEL is required")
	}
	if result.loadDemoData && (strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")) {
		return config{}, errorsNew("AILUO_LOAD_DEMO_DATA must be false in production")
	}
	if result.runtimeInstallRoot != "" &&
		(!filepath.IsAbs(result.runtimeInstallRoot) || filepath.Clean(result.runtimeInstallRoot) != result.runtimeInstallRoot) {
		return config{}, errorsNew("AILUO_RUNTIME_INSTALL_ROOT must be a clean absolute path")
	}
	if result.manageAgent {
		secretFile := os.Getenv("AILUO_MODEL_API_KEY_FILE")
		rawSecretConfigured := os.Getenv("AILUO_MODEL_API_KEY") != "" || os.Getenv("OPENAI_API_KEY") != ""
		production := strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")
		if production && rawSecretConfigured {
			return config{}, errorsNew("production model credentials must use AILUO_MODEL_API_KEY_FILE")
		}
		if secretFile == "" && !rawSecretConfigured {
			return config{}, errorsNew("AILUO_MODEL_API_KEY_FILE, AILUO_MODEL_API_KEY or OPENAI_API_KEY is required when Go manages the AI Agent")
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
			return nil, errorsNew("AILUO_RUNTIME_HOST_ADDRESS is required for installed hosted runtimes")
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

func errorsNew(message string) error {
	return fmt.Errorf("configuration error: %s", message)
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
