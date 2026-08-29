package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	goruntime "runtime"
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
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/demo"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/prompt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
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

// defaultRuntimeRoot 返回用户级默认安装根目录（用户配置目录下 ailuo/runtime）。
// 与 npm 的 node_modules、cargo 的 registry 同理：ailuo install 与内核启动共享
// 同一默认位置，无需显式 --root；UserConfigDir 不可用（无 HOME 等）时返回空。
func defaultRuntimeRoot() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "ailuo", "runtime")
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
		*root = defaultRuntimeRoot()
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

const (
	builtInExecutorRuntimeID      = "ailuo.agent"
	builtInExecutorRuntimeVersion = "1.0.0"
)

var builtInExecutorRuntimeDigest = func() string {
	sum := sha256.Sum256([]byte("ailuo.agent built-in isolated executor runtime\nprotocol " + executor.Version))
	return hex.EncodeToString(sum[:])
}()

func builtInExecutorManifest() loader.Manifest {
	return loader.Manifest{
		ID: builtInExecutorRuntimeID, Version: builtInExecutorRuntimeVersion,
		Mode: loader.ModeIsolated, Role: loader.RoleExecutor,
		LockedDigest: builtInExecutorRuntimeDigest, Pin: true,
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

	reg := registry.New()
	app, created, err := store.Ensure(ctx, appconfig.Config{
		AppID: campus.AppID, Enabled: true, Model: config.model,
		SystemPrompt:    config.baseSystemPrompt,
		ChannelPrompts:  config.channelPrompts,
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
			kernelecho.CreateChildRunCapabilityID,
			kernelecho.GetChildStatusCapabilityID,
			promptservice.PreferenceGetID,
			promptservice.PreferenceSetID,
			promptservice.PreferenceResetID,
		},
	})
	if err != nil {
		return fmt.Errorf("ensure campus App config: %w", err)
	}
	if !created {
		// 既有部署升级：把旧 child Capability 名称和 prompt 偏好补齐到当前配置。
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
		replacement.SystemPrompt = config.baseSystemPrompt
		replacement.ChannelPrompts = config.channelPrompts
		replacement.EnabledCapabilities = migrateEnabledCapabilities(app.EnabledCapabilities)
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
	promptService := promptservice.NewService(config.promptCatalog, store)
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
	// 到唯一宿主。内置 agent（isolated 进程）与 installed 包（hosted/isolated）
	// 同池管理；campus.bus 是安装目录包（声明宿主函数 → 进程内 WasmHost 装载）。
	workDir, err := filepath.Abs(".")
	if err != nil {
		return fmt.Errorf("resolve executor working directory: %w", err)
	}
	executorManifest := builtInExecutorManifest()
	executorHost, err := loader.NewExecutorHost(loader.ExecutorHostConfig{
		Manifest: executorManifest,
		Resolve: func(context.Context) (packmgr.ProcessSpec, error) {
			return packmgr.ProcessSpec{
				Path:    config.pythonPath,
				Args:    []string{"-m", "agent.runtime", "--listen", config.agentAddress},
				Env:     agentEnvironment(config),
				WorkDir: workDir,
				Address: config.agentAddress,
				Limits:  packmgr.ProcessLimits{},
			}, nil
		},
		Spawn: config.manageAgent, Model: app.Model, Stdout: os.Stdout, Stderr: os.Stderr,
		DialTimeout:    secondsDuration(config.agentProcess.DialTimeoutSeconds),
		StopGrace:      secondsDuration(config.agentProcess.StopGraceSeconds),
		TerminateGrace: secondsDuration(config.agentProcess.TerminateGraceSeconds),
	})
	if err != nil {
		return fmt.Errorf("create executor runtime host: %w", err)
	}
	// 宿主函数集：当前只有 campus 的存储投影（内核特权，供安装目录 hosted 包声明）。
	hostFunctions := campus.HostedFunctions(store)
	installedHosts, installedRecords, _, err := configureInstalledRuntimes(ctx, config, hostFunctions)
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
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, runtimeLoader.Shutdown(shutdownContext))
	}()
	// 执行者是 Core 的运行时角色，不注册为 Capability Service；业务包仍经安装目录
	// 的统一注册路径进入 Registry。
	if err := runtimeLoader.Register(ctx, executorManifest); err != nil {
		return fmt.Errorf("register executor runtime: %w", err)
	}
	if len(installedRecords) > 0 {
		if err := loader.RegisterInstalled(ctx, runtimeLoader, reg, installedRecords); err != nil {
			return fmt.Errorf("register installed runtimes: %w", err)
		}
	}
	// campus.bus 是核心业务包（App 配置启用的 Capability 均由其导出）：安装目录
	// 缺失 bus 组件即拒绝就绪（fail-closed），给出可行动错误。只比包 ID 不够——
	// 包内可能只装上了别的组件，那时 Capability 一个也没注册，却会判定为就绪。
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
	// 预热全部声明 pin 的运行时（内置 agent 与 installed pin）：编译/启动
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

	orchestrator := kernelecho.NewOrchestrator(executorClient, reg, dispatcher, policy, kernelecho.StorePorts{
		Idempotency: store, Creation: store, Execution: store, Recovery: store,
		Cancellation: store, Events: store, Audit: store,
	}, kernelecho.Config{
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
	if err := kernelecho.RegisterChildCapabilities(reg, orchestrator); err != nil {
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
	echoEvents := access.NewEventHub()
	runScheduler := kernelecho.NewScheduler(ctx, orchestrator, store, echoEvents, campus.AppID,
		kernelecho.WithScheduler(config.scheduler.Workers, time.Duration(config.scheduler.PollMs)*time.Millisecond, config.scheduler.BatchSize))
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, runScheduler.Shutdown(shutdownContext))
	}()
	if _, err := runScheduler.Recover(ctx); err != nil {
		return fmt.Errorf("recover durable runs: %w", err)
	}
	echoAdmission := kernelecho.NewAdmission(orchestrator, runScheduler)
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
	webAccess := web.NewServer(echoAdmission, store, readiness, reg, policy, campus.AppID, platformHub, runScheduler, echoEvents,
		web.WithDispatcher(dispatcher))
	// 平台事件入口独立挂载：/api/v1/ingress/{platform} 由平台适配器规范化事件驱动，
	// 其余路径全部交给 Web Access（健康检查、Echo/SSE、演示页面）。
	ingressServer := ingress.NewServer(campus.AppID, platformHub, echoAdmission)
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
		ingressServer.StopAccepting()
		admissionDone := make(chan error, 2)
		go func() { admissionDone <- webAccess.WaitAdmissions(shutdownContext) }()
		go func() { admissionDone <- ingressServer.WaitAdmissions(shutdownContext) }()
		admissionErr := errors.Join(<-admissionDone, <-admissionDone)
		httpErr := server.Shutdown(shutdownContext)
		if err := errors.Join(admissionErr, httpErr); err != nil {
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
	// 安装根目录：显式 AILUO_RUNTIME_INSTALL_ROOT 优先；未设置时回退用户级
	// 默认目录（ailuo install 与内核共享同一默认位置）。默认目录尚未创建
	// （未安装任何包）时视为未配置：内核跳过安装目录装载，由 campus 核心
	// 包缺失的 fail-closed 检查给出可行动安装指引。
	runtimeInstallRoot := os.Getenv("AILUO_RUNTIME_INSTALL_ROOT")
	if runtimeInstallRoot == "" {
		if def := defaultRuntimeRoot(); def != "" {
			if _, err := os.Stat(def); err == nil {
				runtimeInstallRoot = def
			}
		}
	}
	result := config{
		httpAddress:        envOr("AILUO_HTTP_ADDRESS", "127.0.0.1:8080"),
		configUIAddress:    envOr("AILUO_CONFIG_UI_ADDRESS", configui.DefaultAddress),
		localConfigRoot:    envOr("AILUO_CONFIG_DIR", "var"),
		agentAddress:       envOr("AILUO_AGENT_ADDRESS", "127.0.0.1:50051"),
		pythonPath:         envOr("AILUO_PYTHON", defaultPythonPath(".")),
		databasePath:       envOr("AILUO_DATABASE_PATH", "var/ailuo.db"),
		manageAgent:        manageAgent,
		loadDemoData:       loadDemoData,
		environment:        envOr("AILUO_ENVIRONMENT", "development"),
		logLevel:           logLevel,
		logFormat:          envOr("AILUO_LOG_FORMAT", "console"),
		logSource:          logSource,
		logMaxValueLength:  logMaxValueLength,
		runtimeInstallRoot: runtimeInstallRoot,
		runtimeHostAddress: os.Getenv("AILUO_RUNTIME_HOST_ADDRESS"),
	}
	// Agent 进程规格要求绝对 Python 路径（Spawn 模式校验）；默认值与用户配置
	// 都可能为相对路径，统一在装配前解析为绝对路径。
	absolutePython, err := filepath.Abs(result.pythonPath)
	if err != nil {
		return config{}, fmt.Errorf("configuration error: resolve python path: %w", err)
	}
	result.pythonPath = absolutePython
	if !packmgr.IsLocalRuntimeAddress(result.configUIAddress) {
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
	base.qqAllowedGroupIDs = slices.Clone(settings.QQAllowedGroupIDs)
	base.qqAllowedPrivateIDs = slices.Clone(settings.QQAllowedPrivateUserIDs)
	base.qqQuickReplies = make([]qq.QuickReply, 0, len(settings.QQQuickReplies))
	for _, rule := range settings.QQQuickReplies {
		base.qqQuickReplies = append(base.qqQuickReplies, qq.QuickReply{Trigger: rule.Trigger, Reply: rule.Reply})
	}
	base.qqPokeReplies = append([]string{}, settings.QQPokeReplies...)
	base.promptCatalog = settings.PromptCatalog.Clone()
	base.baseSystemPrompt = settings.BaseSystemPrompt
	base.channelPrompts = maps.Clone(settings.ChannelPrompts)
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

func defaultPythonPath(projectRoot string) string {
	if goruntime.GOOS == "windows" {
		return filepath.Join(projectRoot, "agent", ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(projectRoot, "agent", ".venv", "bin", "python")
}

// configureInstalledRuntimes 发现安装目录中的 installed 包，返回加入统一 Loader
// 的宿主、待注册记录与安装目录 catalog。安装目录未配置时返回空切片（统一
// Loader 只装配内置 agent）；配置了安装目录但没有 hosted 宿主地址时 fail-closed。
// hosted 包按是否声明 host_functions 分流：有声明 → 进程内 WasmHost（宿主函数
// 是内核特权，跨进程无法投影），无声明 → 外部 GRPCHost。pin 运行时由各清单
// 声明，预热清单由 runtimeLoader.Pinned() 统一推导。
func configureInstalledRuntimes(ctx context.Context, cfg config, hostFunctions []loader.HostedFunction) (hosts []loader.Host, records []loader.InstalledRecord, catalog *loader.Catalog, err error) {
	if cfg.runtimeInstallRoot == "" {
		return nil, nil, nil, nil
	}
	catalog, err = loader.NewCatalog(cfg.runtimeInstallRoot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create installed runtime catalog: %w", err)
	}
	records, err = catalog.Discover(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discover installed runtimes: %w", err)
	}
	if len(records) == 0 {
		observe.Info(ctx, "运行时安装目录校验完成",
			observe.IntAttr("runtime_count", 0),
		)
		return nil, nil, catalog, nil
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
			return nil, nil, nil, loader.ErrUnsupportedMode
		}
		if record.Runtime.Pin {
			pinnedCount++
		}
	}
	hosts = make([]loader.Host, 0, 3)
	if hostedWithFunctions > 0 {
		// 声明宿主函数的 hosted 包只能在内核进程内执行（宿主函数是内核特权）。
		host, hostErr := loader.NewWasmHost(loader.WasmHostConfig{
			ReadArtifact:         catalog.ReadArtifact,
			HostFunctions:        hostFunctions,
			RequireHostFunctions: true,
		})
		if hostErr != nil {
			return nil, nil, nil, fmt.Errorf("configure in-kernel hosted runtime boundary: %w", hostErr)
		}
		hosts = append(hosts, host)
	}
	if hostedWithoutFunctions > 0 {
		if cfg.runtimeHostAddress == "" {
			return nil, nil, nil, fmt.Errorf("configuration error: AILUO_RUNTIME_HOST_ADDRESS is required for installed hosted runtimes without host functions")
		}
		host, hostErr := loader.NewGRPCHost(loader.GRPCHostConfig{
			Mode: loader.ModeHosted, Address: cfg.runtimeHostAddress,
			VerifyInstalled: catalog.VerifyRuntime,
			DialTimeout:     10 * time.Second,
			MaxRuntimes:     hostedWithoutFunctions,
			MaxConcurrent:   64,
		})
		if hostErr != nil {
			return nil, nil, nil, fmt.Errorf("configure hosted runtime boundary: %w", hostErr)
		}
		hosts = append(hosts, host)
	}
	if isolatedCount > 0 {
		host, hostErr := loader.NewIsolatedProcessHost(loader.IsolatedProcessHostConfig{
			ResolveInstalled: catalog.ResolveProcess,
			VerifyInstalled:  catalog.VerifyProcess,
			DialTimeout:      10 * time.Second,
			StopGrace:        5 * time.Second,
			TerminateGrace:   2 * time.Second,
		})
		if hostErr != nil {
			return nil, nil, nil, fmt.Errorf("configure isolated runtime boundary: %w", hostErr)
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
	return hosts, records, catalog, nil
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

func migrateEnabledCapabilities(existing []string) []string {
	result := make([]string, 0, len(existing)+5)
	for _, capabilityID := range existing {
		switch capabilityID {
		case "agent.run":
			capabilityID = kernelecho.CreateChildRunCapabilityID
		case "agent.status":
			capabilityID = kernelecho.GetChildStatusCapabilityID
		}
		if !slices.Contains(result, capabilityID) {
			result = append(result, capabilityID)
		}
	}
	for _, capabilityID := range []string{
		kernelecho.GetChildStatusCapabilityID,
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
