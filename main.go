package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/gen/runtimev1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/app"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/sdkgen"
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
	case "install", "upgrade", "uninstall", "list", "pack", "publish", "sdk-go", "sdk-py", "sdk-ts":
		return runPackageCommand(ctx, arguments, output)
	default:
		return true, fmt.Errorf("configuration error: unknown command")
	}
}

// runPackageCommand 执行包管理 CLI：install 支持本地目录/tarball/GitHub
// Release 源（owner/repo[@约束]），upgrade/uninstall/list/pack/publish 见各分支。
func runPackageCommand(parent context.Context, arguments []string, output io.Writer) (bool, error) {
	command := arguments[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "安装根目录（默认 AILUO_RUNTIME_INSTALL_ROOT，再默认用户配置目录 ailuo/runtime）")
	repo := flags.String("repo", "", "GitHub 仓库（owner/repo），publish 使用")
	version := flags.String("version", "", "零声明包自动生成的 semver 版本")
	if err := flags.Parse(arguments[1:]); err != nil {
		return true, fmt.Errorf("configuration error: %s", err)
	}
	if *root == "" {
		*root = os.Getenv("AILUO_RUNTIME_INSTALL_ROOT")
	}
	if *root == "" {
		*root = packmgr.DefaultInstallRoot()
	}
	if *root == "" && command != "pack" && command != "publish" && command != "sdk-go" && command != "sdk-py" && command != "sdk-ts" {
		return true, fmt.Errorf("configuration error: %s requires --root 或 AILUO_RUNTIME_INSTALL_ROOT", command)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	switch command {
	case "install":
		if flags.NArg() != 1 {
			return true, fmt.Errorf("configuration error: install requires exactly one source package directory")
		}
		source := flags.Arg(0)
		owner, name, constraint, isRegistry := splitRegistryRef(source)
		var record packmgr.InstalledRecord
		var err error
		_, sourceErr := os.Stat(source)
		if errors.Is(sourceErr, fs.ErrNotExist) && isRegistry {
			record, err = packmgr.InstallFromRelease(ctx, *root, packmgr.NewGitHubClient(), owner, name, constraint)
		} else if sourceErr != nil {
			return true, sourceErr
		} else {
			record, err = packmgr.Install(ctx, *root, source)
		}
		if err != nil {
			return true, err
		}
		if _, err := fmt.Fprintf(output, "已安装 %s@%s（%s）\n", record.Manifest.ID, record.Manifest.Version, componentModes(record.Manifest)); err != nil {
			return true, err
		}
		return true, nil
	case "upgrade":
		if flags.NArg() != 2 {
			return true, fmt.Errorf("configuration error: upgrade requires package id and source package directory")
		}
		record, err := packmgr.Upgrade(ctx, *root, flags.Arg(0), flags.Arg(1))
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprintf(output, "已升级 %s@%s（%s）\n", record.Manifest.ID, record.Manifest.Version, componentModes(record.Manifest))
		return true, err
	case "uninstall":
		if flags.NArg() != 1 {
			return true, fmt.Errorf("configuration error: uninstall requires exactly one package id")
		}
		if err := packmgr.Uninstall(ctx, *root, flags.Arg(0)); err != nil {
			return true, err
		}
		if _, err := fmt.Fprintf(output, "已卸载 %s\n", flags.Arg(0)); err != nil {
			return true, err
		}
		return true, nil
	case "pack":
		if flags.NArg() < 1 || flags.NArg() > 2 {
			return true, fmt.Errorf("configuration error: pack requires source package directory [and optional output directory]")
		}
		// 输出目录只由位置参数决定：--root 是安装根，拿它当打包输出目录会把
		// tarball 丢进已安装包的目录树里。
		outputDir := "."
		if flags.NArg() == 2 {
			outputDir = flags.Arg(1)
		}
		manifest, manifestBytes, err := resolveSource(ctx, flags.Arg(0), *version)
		if err != nil {
			return true, err
		}
		tarballPath, err := packmgr.PackFromSource(ctx, flags.Arg(0), outputDir, manifest, manifestBytes)
		if err != nil {
			return true, err
		}
		if _, err := fmt.Fprintf(output, "已打包 %s\n", tarballPath); err != nil {
			return true, err
		}
		return true, nil
	case "publish":
		if flags.NArg() != 1 {
			return true, fmt.Errorf("configuration error: publish requires exactly one source package directory or .tgz")
		}
		owner, name, _, ok := splitRegistryRef(*repo)
		if !ok || owner == "" || name == "" {
			return true, fmt.Errorf("configuration error: publish requires --repo owner/repo")
		}
		client := packmgr.NewGitHubClient()
		var htmlURL string
		var err error
		if strings.HasSuffix(strings.ToLower(flags.Arg(0)), ".tgz") {
			htmlURL, err = client.PublishTarball(ctx, owner, name, flags.Arg(0))
		} else {
			manifest, manifestBytes, resolveErr := resolveSource(ctx, flags.Arg(0), *version)
			if resolveErr != nil {
				return true, resolveErr
			}
			htmlURL, err = client.PublishFromSource(ctx, owner, name, flags.Arg(0), manifest, manifestBytes)
		}
		if err != nil {
			return true, err
		}
		if _, err := fmt.Fprintf(output, "已发布 %s\n", htmlURL); err != nil {
			return true, err
		}
		return true, nil
	case "sdk-go", "sdk-py", "sdk-ts":
		if flags.NArg() < 1 || flags.NArg() > 2 {
			return true, fmt.Errorf("configuration error: %s requires source package directory [and optional output directory]", command)
		}
		language := sdkgen.LanguageGo
		if command == "sdk-py" {
			language = sdkgen.LanguagePython
		}
		if command == "sdk-ts" {
			language = sdkgen.LanguageTypeScript
		}
		packageID, extensions, err := resolveSDKSource(ctx, flags.Arg(0))
		if err != nil {
			return true, err
		}
		files, err := sdkgen.Generate(extensions, sdkgen.Options{Language: language, PackageID: packageID})
		if err != nil {
			return true, err
		}
		outputDir := "."
		if flags.NArg() == 2 {
			outputDir = flags.Arg(1)
		}
		for _, f := range files {
			path := filepath.Join(outputDir, f.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return true, err
			}
			if err := os.WriteFile(path, f.Code, 0o644); err != nil {
				return true, err
			}
		}
		if _, err := fmt.Fprintf(output, "已生成 SDK：%d 个文件到 %s\n", len(files), outputDir); err != nil {
			return true, err
		}
		return true, nil
	default: // list
		if flags.NArg() != 0 {
			return true, fmt.Errorf("configuration error: list takes no positional arguments")
		}
		records, err := packmgr.ListInstalled(ctx, *root)
		if err != nil {
			return true, err
		}
		if len(records) == 0 {
			if _, err := fmt.Fprintln(output, "安装根目录为空"); err != nil {
				return true, err
			}
			return true, nil
		}
		for _, record := range records {
			pin := ""
			if record.Manifest.Pin {
				pin = " [pin]"
			}
			if _, err := fmt.Fprintf(output, "%s@%s\t%s%s\n", record.Manifest.ID, record.Manifest.Version, componentModes(record.Manifest), pin); err != nil {
				return true, err
			}
		}
		return true, nil
	}
}

// componentModes 汇总包内组件的运行形态：单组件直接给出 mode，多组件按形态计数。
// 包不是"一种模式"（每个组件各有 mode），打印 Components[0].Mode 会把混合包说成
// 单一形态。
func componentModes(manifest packmgr.Manifest) string {
	if len(manifest.Components) == 1 {
		return manifest.Components[0].Mode
	}
	counts := make(map[string]int, 2)
	for _, component := range manifest.Components {
		counts[component.Mode]++
	}
	parts := make([]string, 0, len(counts))
	for _, mode := range []string{packmgr.ModeHosted, packmgr.ModeIsolated} {
		if counts[mode] > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", mode, counts[mode]))
		}
	}
	return strings.Join(parts, "+")
}

// registryRefPattern 匹配 GitHub Release 源：owner/repo[@约束]。
var registryRefPattern = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)(?:@(.+))?$`)

// splitRegistryRef 解析 owner/repo[@constraint] 注册表引用；本地存在的路径
// 由调用方先排除（本地路径优先）。
func splitRegistryRef(source string) (owner, repo, constraint string, ok bool) {
	match := registryRefPattern.FindStringSubmatch(source)
	if match == nil {
		return "", "", "", false
	}
	return match[1], match[2], match[3], true
}

// resolveSource 解析源包目录：优先 ailuo.toml（显式声明，含宿主函数/存储），
// 无则从源码自动提取清单并构建（作者零声明，纯计算包）。清单声明 [build] 时
// 先执行构建再返回（pack/publish 共用，构建失败即报错，不打包残缺工件）。
func resolveSource(ctx context.Context, sourceDir, version string) (manifest packmgr.Manifest, manifestBytes []byte, err error) {
	path := packagefmt.SourcePath(sourceDir)
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		if version == "" {
			return packmgr.Manifest{}, nil, fmt.Errorf("配置错误：零声明包必须通过 --version 提供版本")
		}
		// 无 ailuo.toml：从源码自动提取并构建（作者零声明）。
		capabilities, buildTool, extractErr := packagefmt.AutoExtract(ctx, sourceDir)
		if extractErr != nil {
			return packmgr.Manifest{}, nil, extractErr
		}
		absolute, absErr := filepath.Abs(sourceDir)
		if absErr != nil {
			return packmgr.Manifest{}, nil, absErr
		}
		manifest, manifestBytes, err = packagefmt.ManifestFromCapabilities(filepath.Base(absolute), version, capabilities)
		if err != nil {
			return packmgr.Manifest{}, nil, err
		}
		if err := packagefmt.Build(ctx, sourceDir, manifest, packagefmt.BuildSpec{Tool: buildTool}); err != nil {
			return packmgr.Manifest{}, nil, err
		}
		if err := loader.VerifyHostedProtocol(ctx, sourceDir, manifest); err != nil {
			return packmgr.Manifest{}, nil, err
		}
		return manifest, manifestBytes, nil
	}
	if statErr != nil {
		return packmgr.Manifest{}, nil, fmt.Errorf("读取源清单失败: %w", statErr)
	}
	manifest, manifestBytes, build, err := packagefmt.Parse(path)
	if err != nil {
		return packmgr.Manifest{}, nil, err
	}
	if build != nil {
		if err := packagefmt.Build(ctx, sourceDir, manifest, *build); err != nil {
			return packmgr.Manifest{}, nil, err
		}
	}
	return manifest, manifestBytes, nil
}

// resolveSDKSource 读取显式源清单，或只提取零声明源码的 extensions；SDK 生成不构建
// guest 工件，也不需要虚构一个包版本。
func resolveSDKSource(ctx context.Context, sourceDir string) (string, json.RawMessage, error) {
	path := packagefmt.SourcePath(sourceDir)
	_, statErr := os.Stat(path)
	if statErr == nil {
		manifest, _, _, err := packagefmt.Parse(path)
		return manifest.ID, manifest.Extensions, err
	}
	if !errors.Is(statErr, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("读取源清单失败: %w", statErr)
	}
	absolute, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", nil, err
	}
	capabilities, _, err := packagefmt.AutoExtract(ctx, sourceDir)
	if err != nil {
		return "", nil, err
	}
	extensions, err := packagefmt.ExtensionsFromCapabilities(filepath.Base(absolute), capabilities)
	if err != nil {
		return "", nil, err
	}
	return filepath.Base(absolute), extensions, nil
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
	if !packmgr.IsLocalRuntimeAddress(*address) {
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
	return app.Run()
}
