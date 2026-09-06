package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/app"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"google.golang.org/grpc"
)

func main() {
	handled, err := runMaintenanceCommand(os.Args[1:], os.Stdout)
	if handled {
		if err != nil {
			// CLI/维护命令直接输出可读错误到 stderr（公共 API 的泄密约束不适用于本机 CLI）。
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
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
		appID := flags.String("app", "", "App 标识（必填）")
		platform := flags.String("platform", "", "外部平台标识")
		space := flags.String("space", "", "外部平台空间标识")
		platformUser := flags.String("platform-user", "", "外部平台用户标识")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *database == "" || *userID == "" || *appID == "" {
			return true, fmt.Errorf("configuration error: identity-bind requires --database, --user and --app")
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
		appID := flags.String("app", "", "App 标识（必填）")
		platform := flags.String("platform", "", "外部平台标识")
		space := flags.String("space", "", "外部平台空间标识")
		platformUser := flags.String("platform-user", "", "外部平台用户标识")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *database == "" || *appID == "" || *platform == "" || *space == "" || *platformUser == "" {
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
	projectRoot := flags.String("project-root", "", "项目根目录绝对路径（含 ailuo.toml/ailuo.lock）")
	address := flags.String("address", "", "监听地址（loopback 或绝对 Unix socket）")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *installRoot == "" || *projectRoot == "" || *address == "" {
		return true, fmt.Errorf("configuration error: runtime-host requires --install-root, --project-root and --address")
	}
	for name, value := range map[string]string{"--install-root": *installRoot, "--project-root": *projectRoot} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return true, fmt.Errorf("configuration error: %s must be a clean absolute path", name)
		}
	}
	if !packagecontract.IsLocalRuntimeAddress(*address) {
		return true, fmt.Errorf("configuration error: --address must be loopback or an absolute unix socket")
	}
	return true, serveRuntimeHost(*installRoot, *projectRoot, *address, output)
}

// serveRuntimeHost 装载 hosted 后端并监听 RuntimeHost 协议，直到信号停止。
func serveRuntimeHost(installRoot, projectRoot, address string, output io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = observe.With(ctx, observe.Component("runtime_host"))
	observe.Info(ctx, "正在启动 Runtime Host 服务",
		observe.StringAttr("install_root", installRoot),
		observe.StringAttr("address", address),
	)
	catalog, err := packagesource.NewCatalog(installRoot)
	if err != nil {
		return err
	}
	projectLock, err := packagesource.ReadProjectLock(ctx, projectRoot)
	if err != nil {
		return fmt.Errorf("read project lock: %w", err)
	}
	records, err := catalog.DiscoverLocked(ctx, projectLock)
	if err != nil {
		return fmt.Errorf("discover installed runtimes: %w", err)
	}
	hostedCount := 0
	allowedRuntimes := make([]loader.BackendIdentity, 0, len(records))
	for _, record := range records {
		if record.Runtime.Mode == loader.ModeHosted {
			hostedCount++
			allowedRuntimes = append(allowedRuntimes, loader.BackendIdentity{
				ID: record.Runtime.ID, Version: record.Runtime.Version,
			})
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
		Mode: loader.ModeHosted, Backend: backend, AllowedRuntimes: allowedRuntimes,
		MaxRuntimes: hostedCount, MaxConcurrent: 64,
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
	return app.Run()
}
