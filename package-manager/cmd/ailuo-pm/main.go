package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/projectmgr"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/sdkgen"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("configuration error: package command is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return runPackageCommand(ctx, arguments, output)
}

// runPackageCommand 执行包管理 CLI：sync/install 支持项目或本地包，install 还
// 支持 GitHub Release 源（owner/repo[@约束]），upgrade/uninstall/list/pack/publish
// 见各分支。
func runPackageCommand(parent context.Context, arguments []string, output io.Writer) error {
	command := arguments[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "安装根目录（默认 AILUO_PACKAGE_INSTALL_ROOT，再默认用户配置目录 ailuo/runtime）")
	project := flags.String("project", ".", "项目目录或 ailuo.toml 路径（sync 使用）")
	repo := flags.String("repo", "", "GitHub 仓库（owner/repo），publish 使用")
	if err := flags.Parse(arguments[1:]); err != nil {
		return fmt.Errorf("configuration error: %s", err)
	}
	if *root == "" {
		*root = os.Getenv("AILUO_PACKAGE_INSTALL_ROOT")
	}
	if *root == "" {
		*root = packmgr.DefaultInstallRoot()
	}
	if *root == "" && command != "pack" && command != "publish" && command != "inspect" && command != "sdk-go" && command != "sdk-py" && command != "sdk-ts" {
		return fmt.Errorf("configuration error: %s requires --root 或 AILUO_PACKAGE_INSTALL_ROOT", command)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	switch command {
	case "sync":
		if flags.NArg() != 0 {
			return fmt.Errorf("configuration error: sync 不接受位置参数")
		}
		projectFile, err := projectManifestPath(*project)
		if err != nil {
			return err
		}
		lock, err := projectmgr.Sync(ctx, projectFile, *root, packmgr.NewGitHubClient())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "已同步项目 %s：%d 个包\n", lock.ProjectID, len(lock.Packages))
		return err
	case "install":
		if flags.NArg() != 1 {
			return fmt.Errorf("configuration error: install requires exactly one source package directory")
		}
		source := flags.Arg(0)
		owner, name, constraint, isRegistry := splitRegistryRef(source)
		var record packmgr.InstalledRecord
		var err error
		_, sourceErr := os.Stat(source)
		if errors.Is(sourceErr, fs.ErrNotExist) && isRegistry {
			record, err = packmgr.InstallFromRelease(ctx, *root, packmgr.NewGitHubClient(), owner, name, constraint)
		} else if sourceErr != nil {
			return sourceErr
		} else {
			packageSource, cleanup, prepareErr := preparePackageSource(ctx, source)
			if prepareErr != nil {
				return prepareErr
			}
			defer cleanup()
			record, err = packmgr.Install(ctx, *root, packageSource)
		}
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "已安装 %s@%s（%s）\n", record.Manifest.ID, record.Manifest.Version, componentModes(record.Manifest)); err != nil {
			return err
		}
		return nil
	case "upgrade":
		if flags.NArg() != 2 {
			return fmt.Errorf("configuration error: upgrade requires package id and source package directory")
		}
		packageSource, cleanup, prepareErr := preparePackageSource(ctx, flags.Arg(1))
		if prepareErr != nil {
			return prepareErr
		}
		defer cleanup()
		record, err := packmgr.Upgrade(ctx, *root, flags.Arg(0), packageSource)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "已升级 %s@%s（%s）\n", record.Manifest.ID, record.Manifest.Version, componentModes(record.Manifest))
		return err
	case "uninstall":
		if flags.NArg() != 1 {
			return fmt.Errorf("configuration error: uninstall requires exactly one package id")
		}
		if err := packmgr.Uninstall(ctx, *root, flags.Arg(0)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "已卸载 %s\n", flags.Arg(0)); err != nil {
			return err
		}
		return nil
	case "pack":
		if flags.NArg() < 1 || flags.NArg() > 2 {
			return fmt.Errorf("configuration error: pack requires source package directory [and optional output directory]")
		}
		// 输出目录只由位置参数决定：--root 是安装根，拿它当打包输出目录会把
		// tarball 丢进已安装包的目录树里。
		outputDir := "."
		if flags.NArg() == 2 {
			outputDir = flags.Arg(1)
		}
		manifest, manifestBytes, err := resolveSource(ctx, flags.Arg(0))
		if err != nil {
			return err
		}
		tarballPath, err := packmgr.PackFromSource(ctx, flags.Arg(0), outputDir, manifest, manifestBytes)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "已打包 %s\n", tarballPath)
		return err
	case "inspect":
		if flags.NArg() != 1 {
			return fmt.Errorf("configuration error: inspect requires source package directory")
		}
		metadata, err := inspectSource(flags.Arg(0))
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(metadata)
	case "publish":
		if flags.NArg() != 1 {
			return fmt.Errorf("configuration error: publish requires exactly one source package directory or .tgz")
		}
		owner, name, _, ok := splitRegistryRef(*repo)
		if !ok || owner == "" || name == "" {
			return fmt.Errorf("configuration error: publish requires --repo owner/repo")
		}
		client := packmgr.NewGitHubClient()
		var htmlURL string
		var err error
		if strings.HasSuffix(strings.ToLower(flags.Arg(0)), ".tgz") {
			htmlURL, err = client.PublishTarball(ctx, owner, name, flags.Arg(0))
		} else {
			manifest, manifestBytes, resolveErr := resolveSource(ctx, flags.Arg(0))
			if resolveErr != nil {
				return resolveErr
			}
			htmlURL, err = client.PublishFromSource(ctx, owner, name, flags.Arg(0), manifest, manifestBytes)
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "已发布 %s\n", htmlURL)
		return err
	case "sdk-go", "sdk-py", "sdk-ts":
		if flags.NArg() < 1 || flags.NArg() > 2 {
			return fmt.Errorf("configuration error: %s requires source package directory [and optional output directory]", command)
		}
		language := sdkgen.LanguageGo
		if command == "sdk-py" {
			language = sdkgen.LanguagePython
		}
		if command == "sdk-ts" {
			language = sdkgen.LanguageTypeScript
		}
		packageID, extensions, err := resolveSDKSource(flags.Arg(0))
		if err != nil {
			return err
		}
		files, err := sdkgen.Generate(extensions, sdkgen.Options{Language: language, PackageID: packageID})
		if err != nil {
			return err
		}
		outputDir := "."
		if flags.NArg() == 2 {
			outputDir = flags.Arg(1)
		}
		for _, file := range files {
			path := filepath.Join(outputDir, file.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, file.Code, 0o644); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(output, "已生成 SDK：%d 个文件到 %s\n", len(files), outputDir)
		return err
	default: // list
		if command != "list" {
			return fmt.Errorf("configuration error: unknown command")
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("configuration error: list takes no positional arguments")
		}
		records, err := packageio.ListInstalled(ctx, *root)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			_, err := fmt.Fprintln(output, "安装根目录为空")
			return err
		}
		for _, record := range records {
			pin := ""
			if record.Manifest.Pin {
				pin = " [pin]"
			}
			if _, err := fmt.Fprintf(output, "%s@%s\t%s%s\n", record.Manifest.ID, record.Manifest.Version, componentModes(record.Manifest), pin); err != nil {
				return err
			}
		}
		return nil
	}
}

func projectManifestPath(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("configuration error: --project 不能为空")
	}
	info, err := os.Stat(value)
	if err != nil {
		return "", fmt.Errorf("读取项目路径失败: %w", err)
	}
	if info.IsDir() {
		return filepath.Join(value, packagefmt.ProjectFileName), nil
	}
	if filepath.Base(value) != packagefmt.ProjectFileName {
		return "", fmt.Errorf("configuration error: --project 必须是项目目录或 %s", packagefmt.ProjectFileName)
	}
	return value, nil
}

// componentModes 汇总包内组件的运行形态：单组件直接给出 mode，多组件按形态计数。
func componentModes(manifest packagecontract.Manifest) string {
	if len(manifest.Components) == 1 {
		return manifest.Components[0].Mode
	}
	counts := make(map[string]int, 2)
	for _, component := range manifest.Components {
		counts[component.Mode]++
	}
	parts := make([]string, 0, len(counts))
	for _, mode := range []string{packagecontract.ModeHosted, packagecontract.ModeIsolated} {
		if counts[mode] > 0 {
			parts = append(parts, fmt.Sprintf("%s×%d", mode, counts[mode]))
		}
	}
	return strings.Join(parts, "+")
}

// registryRefPattern 匹配 GitHub Release 源：owner/repo[@constraint]。
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

// resolveSource 解析源包目录中的显式 ailuo.toml，并执行清单声明的构建计划。
func resolveSource(ctx context.Context, sourceDir string) (manifest packagecontract.Manifest, manifestBytes []byte, err error) {
	return packagefmt.Resolve(ctx, sourceDir)
}

// preparePackageSource 将作者侧 ailuo.toml 源目录一次性打成临时发布物；已有
// manifest.json 目录或 tarball 直接交给 packmgr，避免 CLI 复制安装/升级逻辑。
func preparePackageSource(ctx context.Context, source string) (string, func(), error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", func() {}, err
	}
	if !info.IsDir() {
		return source, func() {}, nil
	}
	manifestPath := packagefmt.SourcePath(source)
	if _, err := os.Stat(manifestPath); errors.Is(err, fs.ErrNotExist) {
		return source, func() {}, nil
	} else if err != nil {
		return "", func() {}, err
	}
	manifest, manifestBytes, err := resolveSource(ctx, source)
	if err != nil {
		return "", func() {}, err
	}
	stage, err := os.MkdirTemp("", "ailuo-package-source-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(stage) }
	archive, err := packmgr.PackFromSource(ctx, source, stage, manifest, manifestBytes)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	return archive, cleanup, nil
}

type sourceMetadata struct {
	ID         string   `json:"id"`
	Version    string   `json:"version"`
	BuildTools []string `json:"build_tools,omitempty"`
}

// inspectSource 只解析显式源清单，不执行构建；workflow 用它从同一份清单
// 判断需要准备的语言工具链，避免调用方重复声明构建器。
func inspectSource(sourceDir string) (sourceMetadata, error) {
	manifest, _, builds, err := packagefmt.Parse(packagefmt.SourcePath(sourceDir))
	if err != nil {
		return sourceMetadata{}, err
	}
	seen := make(map[string]struct{}, len(builds))
	tools := make([]string, 0, len(builds))
	for _, build := range builds {
		if _, exists := seen[build.Tool]; exists {
			continue
		}
		seen[build.Tool] = struct{}{}
		tools = append(tools, build.Tool)
	}
	sort.Strings(tools)
	return sourceMetadata{ID: manifest.ID, Version: manifest.Version, BuildTools: tools}, nil
}

// resolveSDKSource 读取显式源清单中的宿主扩展段；SDK 生成不构建 guest 工件。
func resolveSDKSource(sourceDir string) (string, json.RawMessage, error) {
	path := packagefmt.SourcePath(sourceDir)
	manifest, _, _, err := packagefmt.Parse(path)
	if err != nil {
		return "", nil, err
	}
	return manifest.ID, manifest.Extensions, nil
}
