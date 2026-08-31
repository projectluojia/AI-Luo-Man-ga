package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/configui"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	controlconfig "github.com/projectluojia/AI-Luo-Man-ga/internal/controlplane/config"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/prompt"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

type config struct {
	httpAddress         string
	configUIAddress     string
	localConfigRoot     string
	databasePath        string
	model               string
	executorTimeout     time.Duration
	manageExecutor      bool
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
	runtimeProcess      controlconfig.RuntimeProcessSettings
	governance          controlconfig.GovernanceSettings
}

func loadConfig() (config, error) {
	manageExecutor, err := envBool("AILUO_MANAGE_EXECUTOR", false)
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
	runtimeInstallRoot := os.Getenv("AILUO_RUNTIME_INSTALL_ROOT")
	if runtimeInstallRoot == "" {
		if def := packmgr.DefaultInstallRoot(); def != "" {
			if _, err := os.Stat(def); err == nil {
				runtimeInstallRoot = def
			}
		}
	}
	result := config{
		httpAddress:        envOr("AILUO_HTTP_ADDRESS", "127.0.0.1:8080"),
		configUIAddress:    envOr("AILUO_CONFIG_UI_ADDRESS", configui.DefaultAddress),
		localConfigRoot:    envOr("AILUO_CONFIG_DIR", "var"),
		databasePath:       envOr("AILUO_DATABASE_PATH", "var/ailuo.db"),
		manageExecutor:     manageExecutor,
		loadDemoData:       loadDemoData,
		environment:        envOr("AILUO_ENVIRONMENT", "development"),
		logLevel:           logLevel,
		logFormat:          envOr("AILUO_LOG_FORMAT", "console"),
		logSource:          logSource,
		logMaxValueLength:  logMaxValueLength,
		runtimeInstallRoot: runtimeInstallRoot,
		runtimeHostAddress: os.Getenv("AILUO_RUNTIME_HOST_ADDRESS"),
	}
	if !packagecontract.IsLocalRuntimeAddress(result.configUIAddress) {
		return config{}, fmt.Errorf("configuration error: AILUO_CONFIG_UI_ADDRESS must be loopback")
	}
	if result.loadDemoData && (strings.EqualFold(result.environment, "production") || strings.EqualFold(result.environment, "prod")) {
		return config{}, fmt.Errorf("configuration error: AILUO_LOAD_DEMO_DATA must be false in production")
	}
	if result.runtimeInstallRoot != "" &&
		(!filepath.IsAbs(result.runtimeInstallRoot) || filepath.Clean(result.runtimeInstallRoot) != result.runtimeInstallRoot) {
		return config{}, fmt.Errorf("configuration error: AILUO_RUNTIME_INSTALL_ROOT must be a clean absolute path")
	}
	return result, nil
}

func applyLocalConfig(base config, resolved controlconfig.Resolved) (config, error) {
	settings := resolved.Settings
	base.model = settings.Model
	base.executorTimeout = time.Duration(settings.ExecutorTimeoutSeconds * float64(time.Second))
	base.qqWSURL = settings.QQWSURL
	base.qqEnabled = settings.QQEnabled
	base.qqBotID = settings.QQBotID
	base.qqAllowedGroupIDs = slices.Clone(settings.QQAllowedGroupIDs)
	base.qqAllowedPrivateIDs = slices.Clone(settings.QQAllowedPrivateUserIDs)
	base.qqQuickReplies = make([]qq.QuickReply, 0, len(settings.QQQuickReplies))
	for _, rule := range settings.QQQuickReplies {
		base.qqQuickReplies = append(base.qqQuickReplies, qq.QuickReply{Trigger: rule.Trigger, Reply: rule.Reply})
	}
	base.qqPokeReplies = slices.Clone(settings.QQPokeReplies)
	base.promptCatalog = settings.PromptCatalog.Clone()
	base.baseSystemPrompt = settings.BaseSystemPrompt
	base.channelPrompts = maps.Clone(settings.ChannelPrompts)
	base.agentRun = settings.AgentRun
	base.orchestration = settings.Orchestration
	base.contextAssembly = settings.ContextAssembly
	base.scheduler = settings.Scheduler
	base.qqConnection = settings.QQConnection
	base.runtimeProcess = settings.RuntimeProcess
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

// configureInstalledRuntimes 发现安装目录中的 Runtime 包，按声明的运行模式
// 选择宿主。Loader 只接收已校验的安装记录和通用 Host。宿主函数按清单提供：
// ailuo.store 通用存储函数绑定到各包声明的 namespace，App 隔离在宿主侧强制。
func configureInstalledRuntimes(ctx context.Context, cfg config, packageStore packstore.Store) (hosts []loader.Host, records []loader.InstalledRecord, err error) {
	if cfg.runtimeInstallRoot == "" {
		return nil, nil, nil
	}
	catalog, err := packagesource.NewCatalog(cfg.runtimeInstallRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("create installed runtime catalog: %w", err)
	}
	records, err = catalog.Discover(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("discover installed runtimes: %w", err)
	}
	if len(records) == 0 {
		observe.Info(ctx, "运行时安装目录校验完成", observe.IntAttr("runtime_count", 0))
		return nil, nil, nil
	}
	hostedWithFunctions := 0
	hostedWithoutFunctions := 0
	isolatedCount := 0
	pinnedCount := 0
	for _, record := range records {
		switch record.Runtime.Mode {
		case loader.ModeHosted:
			if len(record.Runtime.HostFunctions) > 0 {
				hostedWithFunctions++
			} else {
				hostedWithoutFunctions++
			}
		case loader.ModeIsolated:
			isolatedCount++
		default:
			return nil, nil, loader.ErrUnsupportedMode
		}
		if record.Runtime.Pin {
			pinnedCount++
		}
	}
	hosts = make([]loader.Host, 0, 3)
	if hostedWithFunctions > 0 {
		host, hostErr := loader.NewWasmHost(loader.WasmHostConfig{
			ReadArtifact: catalog.ReadArtifact,
			HostFunctionsFor: func(manifest loader.Manifest) ([]loader.HostedFunction, error) {
				return packstore.ManifestFunctions(packageStore, manifest)
			},
			RequireHostFunctions: true,
		})
		if hostErr != nil {
			return nil, nil, fmt.Errorf("configure in-kernel hosted runtime boundary: %w", hostErr)
		}
		hosts = append(hosts, host)
	}
	if hostedWithoutFunctions > 0 {
		if cfg.runtimeHostAddress == "" {
			return nil, nil, fmt.Errorf("configuration error: AILUO_RUNTIME_HOST_ADDRESS is required for installed hosted runtimes without host functions")
		}
		host, hostErr := loader.NewGRPCHost(loader.GRPCHostConfig{
			Mode: loader.ModeHosted, Address: cfg.runtimeHostAddress, VerifyInstalled: catalog.VerifyRuntime,
			DialTimeout: 10 * time.Second, MaxRuntimes: hostedWithoutFunctions, MaxConcurrent: 64,
		})
		if hostErr != nil {
			return nil, nil, fmt.Errorf("configure hosted runtime boundary: %w", hostErr)
		}
		hosts = append(hosts, host)
	}
	if isolatedCount > 0 {
		host, hostErr := loader.NewProcessHost(loader.ProcessHostConfig{
			Resolve: catalog.ResolveProcess, Verify: catalog.VerifyProcess, Spawn: true,
			SpawnFor: func(manifest loader.Manifest) bool {
				return manifest.Role != loader.RoleExecutor || cfg.manageExecutor
			},
			DialTimeout:    secondsDuration(cfg.runtimeProcess.DialTimeoutSeconds),
			StopGrace:      secondsDuration(cfg.runtimeProcess.StopGraceSeconds),
			TerminateGrace: secondsDuration(cfg.runtimeProcess.TerminateGraceSeconds),
		})
		if hostErr != nil {
			return nil, nil, fmt.Errorf("configure isolated runtime boundary: %w", hostErr)
		}
		hosts = append(hosts, host)
	}
	observe.Info(ctx, "已安装运行时发现完成",
		observe.IntAttr("runtime_count", len(records)),
		observe.IntAttr("hosted_with_host_functions", hostedWithFunctions),
		observe.IntAttr("hosted_without_host_functions", hostedWithoutFunctions),
		observe.IntAttr("isolated_count", isolatedCount),
		observe.IntAttr("pinned_count", pinnedCount),
	)
	return hosts, records, nil
}

type promptServiceRenderer struct {
	service *promptservice.Service
}

func (r promptServiceRenderer) RenderSystemPrompt(ctx context.Context, request kernelecho.PromptRenderRequest) (string, error) {
	return r.service.RenderSystemPrompt(ctx, promptservice.RenderRequest{
		AppID: request.AppID, UserID: request.UserID, BaseSystemPrompt: request.BaseSystemPrompt,
		Channel: request.Channel, ChannelPrompts: request.ChannelPrompts,
	})
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
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("配置错误：%s 必须是正整数", name)
	}
	return parsed, nil
}

func secondsDuration(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}
