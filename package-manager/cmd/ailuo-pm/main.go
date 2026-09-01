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
	"strings"
	"syscall"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
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

// runPackageCommand 执行包管理 CLI：install 支持本地目录/tarball/GitHub
// Release 源（owner/repo[@约束]），upgrade/uninstall/list/pack/publish 见各分支。
func runPackageCommand(parent context.Context, arguments []string, output io.Writer) error {
	command := arguments[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "安装根目录（默认 AILUO_PACKAGE_INSTALL_ROOT，再默认用户配置目录 ailuo/runtime）")
	repo := flags.String("repo", "", "GitHub 仓库（owner/repo），publish 使用")
	version := flags.String("version", "", "零声明包自动生成的 semver 版本")
	if err := flags.Parse(arguments[1:]); err != nil {
		return fmt.Errorf("configuration error: %s", err)
	}
	if *root == "" {
		*root = os.Getenv("AILUO_PACKAGE_INSTALL_ROOT")
	}
	if *root == "" {
		*root = packmgr.DefaultInstallRoot()
	}
	if *root == "" && command != "pack" && command != "publish" && command != "sdk-go" && command != "sdk-py" && command != "sdk-ts" {
		return fmt.Errorf("configuration error: %s requires --root 或 AILUO_PACKAGE_INSTALL_ROOT", command)
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	switch command {
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
			record, err = packmgr.Install(ctx, *root, source)
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
		record, err := packmgr.Upgrade(ctx, *root, flags.Arg(0), flags.Arg(1))
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
		manifest, manifestBytes, err := resolveSource(ctx, flags.Arg(0), *version)
		if err != nil {
			return err
		}
		tarballPath, err := packmgr.PackFromSource(ctx, flags.Arg(0), outputDir, manifest, manifestBytes)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "已打包 %s\n", tarballPath)
		return err
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
			manifest, manifestBytes, resolveErr := resolveSource(ctx, flags.Arg(0), *version)
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
		packageID, extensions, err := resolveSDKSource(ctx, flags.Arg(0))
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

// resolveSource 解析源包目录：优先 ailuo.toml（显式声明，含宿主函数/存储），
// 无则从源码自动提取清单并构建（作者零声明，纯计算包）。清单声明 [build] 时
// 先执行构建再返回（pack/publish 共用，构建失败即报错，不打包残缺工件）。
func resolveSource(ctx context.Context, sourceDir, version string) (manifest packagecontract.Manifest, manifestBytes []byte, err error) {
	path := packagefmt.SourcePath(sourceDir)
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		if version == "" {
			return packagecontract.Manifest{}, nil, fmt.Errorf("配置错误：零声明包必须通过 --version 提供版本")
		}
		capabilities, buildTool, extractErr := packagefmt.AutoExtract(ctx, sourceDir)
		if extractErr != nil {
			return packagecontract.Manifest{}, nil, extractErr
		}
		absolute, absErr := filepath.Abs(sourceDir)
		if absErr != nil {
			return packagecontract.Manifest{}, nil, absErr
		}
		manifest, manifestBytes, err = packagefmt.ManifestFromCapabilities(filepath.Base(absolute), version, capabilities)
		if err != nil {
			return packagecontract.Manifest{}, nil, err
		}
		if err := packagefmt.Build(ctx, sourceDir, manifest, packagefmt.BuildSpec{Tool: buildTool}); err != nil {
			return packagecontract.Manifest{}, nil, err
		}
		return manifest, manifestBytes, nil
	}
	if statErr != nil {
		return packagecontract.Manifest{}, nil, fmt.Errorf("读取源清单失败: %w", statErr)
	}
	manifest, manifestBytes, build, err := packagefmt.Parse(path)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	if build != nil {
		if err := packagefmt.Build(ctx, sourceDir, manifest, *build); err != nil {
			return packagecontract.Manifest{}, nil, err
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
