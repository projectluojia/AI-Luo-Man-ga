// Package packagefmt 的构建层：ailuo.toml 的 component 级 build 段声明构建方式，
// ailuo-pm pack 统一驱动，不依赖每包手写 shell 脚本。
package packagefmt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// BuildToolGoWasm 是内置的 Go→wasm32-wasi 构建器：跨平台一致（os/exec 设
// GOOS/GOARCH，不依赖 shell），Go 工具链是仓库门禁既有前提。
const BuildToolGoWasm = "go-wasm"

// BuildToolAssemblyScript 是 TypeScript→wasm 构建器：调用 AssemblyScript
// 编译器（npx --package assemblyscript asc，node 生态成熟工具链）。
// 需 node/npx 环境；guest 用 AssemblyScript 语法（TS 严格子集）。
const BuildToolAssemblyScript = "ts-as"

const assemblyScriptPackage = "assemblyscript@0.27.31"

var (
	// ErrBuildUnsupported 是声明了未知构建器的错误（fail-closed，不静默跳过构建）。
	ErrBuildUnsupported = errors.New("unsupported build tool")
	// ErrBuildFailed 是构建执行失败的包裹错误。
	ErrBuildFailed = errors.New("build failed")
)

// BuildSpec 是一个 component 的构建计划：Tool 是构建器，Source 是相对包目录
// 的源码目录（缺省 "."），Components 是该计划负责的组件。源码和工件必须位于
// 包目录内，构建器不越界进入宿主或内核目录。
type BuildSpec struct {
	Tool       string
	Source     string
	Components []string
}

// BuildToolPythonUV 是内置的 Python isolated 执行形态构建器：在源码目录执行
// `uv sync --locked`（依赖严格按 uv.lock，离线可复现），产物为包目录内 .venv。
// 进程组件的 python 解释器路径由安装器按平台解析（.venv/bin/python 或
// .venv/Scripts/python.exe），写进安装期生成的 lock 进程规格。
const BuildToolPythonUV = "python-uv"

// Build 执行 component 级构建计划：为包生成声明的 hosted 工件或 isolated 运行环境。
// 支持 go-wasm（Go，内置）、python-uv（Python，内置）与 ts-as（TypeScript，
// AssemblyScript 编译器）；未知工具 fail-closed。源码目录相对包目录解析，
// 校验不逃逸包目录。
func Build(ctx context.Context, sourceDir string, manifest packagecontract.Manifest, specs []BuildSpec) error {
	builtPythonSources := make(map[string]struct{})
	for _, spec := range specs {
		if err := validateBuildTargets(manifest, spec); err != nil {
			return err
		}
		switch spec.Tool {
		case BuildToolGoWasm:
			if err := buildGoWasm(ctx, sourceDir, manifest, spec); err != nil {
				return err
			}
		case BuildToolPythonUV:
			source := spec.Source
			if source == "" {
				source = "."
			}
			if _, exists := builtPythonSources[source]; exists {
				continue
			}
			spec.Source = source
			if err := buildPythonUV(ctx, sourceDir, spec); err != nil {
				return err
			}
			builtPythonSources[source] = struct{}{}
		case BuildToolAssemblyScript:
			if err := buildAssemblyScript(ctx, sourceDir, manifest, spec); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q（支持：go-wasm、python-uv、ts-as）", ErrBuildUnsupported, spec.Tool)
		}
	}
	return nil
}

func validateBuildTargets(manifest packagecontract.Manifest, spec BuildSpec) error {
	seen := make(map[string]struct{}, len(spec.Components))
	for _, componentID := range spec.Components {
		component, ok := packagecontract.FindComponent(manifest, componentID)
		if !ok ||
			(spec.Tool == BuildToolPythonUV && component.Mode != packagecontract.ModeIsolated) ||
			(spec.Tool != BuildToolPythonUV && component.Mode != packagecontract.ModeHosted) {
			return fmt.Errorf("%w: 构建计划引用了不适用的组件 %q", ErrBuildFailed, componentID)
		}
		if _, duplicate := seen[componentID]; duplicate {
			return fmt.Errorf("%w: 构建计划重复引用组件 %q", ErrBuildFailed, componentID)
		}
		seen[componentID] = struct{}{}
	}
	return nil
}

// buildGoWasm 用 Go 工具链交叉编译每个 hosted 组件：GOOS=wasip1 GOARCH=wasm
// go build -trimpath -o <entrypoint> .，在源码目录内执行（go.mod 来自包目录
// 或仓库根，向上查找；跨平台一致，无 shell 脚本）。工件输出到包目录根
// （entrypoint 相对包目录），源码目录是包目录内的相对位置。sourceDir 转绝对
// 路径，避免输出相对 workDir 解析错位。
func buildGoWasm(ctx context.Context, sourceDir string, manifest packagecontract.Manifest, spec BuildSpec) error {
	if runtime.GOARCH == "wasm" {
		return fmt.Errorf("%w: 当前平台自身是 wasm，无法交叉编译", ErrBuildFailed)
	}
	return buildHostedComponents(ctx, sourceDir, manifest, spec,
		func(ctx context.Context, workDir string, _ packagecontract.Component, output string) ([]byte, error) {
			command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", output, ".")
			command.Dir = workDir
			command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
			return command.CombinedOutput()
		})
}

// buildPythonUV 用 uv 在源码目录创建虚拟环境并按 uv.lock 安装依赖：
// `uv sync --locked --no-dev --link-mode=copy`（跨平台 os/exec 设目录，无 shell，
// 不依赖 PATH 里的 python）。venv 是包级产物，与组件数量无关，只执行一次。
func buildPythonUV(ctx context.Context, sourceDir string, spec BuildSpec) error {
	absoluteSourceDir, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("%w: 解析包目录: %v", ErrBuildFailed, err)
	}
	source := spec.Source
	if source == "" {
		source = "."
	}
	if !packagecontract.IsPackagePath(source) {
		return fmt.Errorf("%w: 源码目录非法 %q", ErrBuildFailed, source)
	}
	command := exec.CommandContext(ctx, "uv", "sync", "--locked", "--no-dev", "--link-mode=copy")
	command.Dir = filepath.Join(absoluteSourceDir, source)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: uv sync: %v\n%s", ErrBuildFailed, err, output)
	}
	if err := materializeVirtualEnvironment(filepath.Join(absoluteSourceDir, source, ".venv")); err != nil {
		return fmt.Errorf("%w: 实体化 Python 虚拟环境链接: %v", ErrBuildFailed, err)
	}
	return nil
}

// materializeVirtualEnvironment 将 uv 在 Unix 上创建的解释器/目录符号链接
// 转为包内实体文件。安装包统一拒绝符号链接，因此构建产物不能直接携带
// venv 的平台链接；文件链接允许指向包外的 Python 解释器，构建时会复制进包。
func materializeVirtualEnvironment(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return packagecontract.ErrInvalidFormat
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	// macOS 的临时目录可能经过 /tmp 等符号链接；边界比较必须使用同一物理路径。
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	for {
		changed := false
		err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink == 0 {
				return nil
			}
			target, err := filepath.EvalSymlinks(current)
			if err != nil {
				return err
			}
			targetInfo, err := os.Stat(target)
			if err != nil {
				return err
			}
			if targetInfo.IsDir() {
				// POSIX venv 的 lib64 只是 lib 别名；复制会重复整棵依赖树并污染发布物。
				if filepath.Base(current) == "lib64" && filepath.Base(target) == "lib" &&
					filepath.Dir(current) == filepath.Dir(target) {
					if err := os.Remove(current); err != nil {
						return err
					}
					changed = true
					return nil
				}
				if !pathWithinDirectory(root, target) || pathWithinDirectory(target, filepath.Dir(current)) {
					return packagecontract.ErrInvalidFormat
				}
				if err := replaceWithCopiedDirectory(current, target, targetInfo.Mode().Perm()); err != nil {
					return err
				}
			} else if targetInfo.Mode().IsRegular() {
				if err := replaceWithCopiedFile(current, target, targetInfo.Mode().Perm()); err != nil {
					return err
				}
			} else {
				return packagecontract.ErrInvalidFormat
			}
			changed = true
			return nil
		})
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
	}
}

func replaceWithCopiedFile(path, source string, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ailuo-materialize-file-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	_, copyErr := io.Copy(temporary, input)
	closeInputErr := input.Close()
	if copyErr != nil || closeInputErr != nil {
		_ = temporary.Close()
		return errors.Join(copyErr, closeInputErr)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func replaceWithCopiedDirectory(path, source string, mode os.FileMode) error {
	temporary, err := os.MkdirTemp(filepath.Dir(path), ".ailuo-materialize-dir-")
	if err != nil {
		return err
	}
	temporaryPath := temporary
	defer func() { _ = os.RemoveAll(temporaryPath) }()
	if err := copyBuildDirectory(source, temporaryPath); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, mode); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func copyBuildDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return packagecontract.ErrInvalidFormat
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return packagecontract.ErrInvalidFormat
		}
		return copyBuildFile(current, target, info.Mode().Perm())
	})
}

func copyBuildFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func pathWithinDirectory(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// buildAssemblyScript 用 AssemblyScript 编译器编译每个 hosted 组件：
// npx --yes --package assemblyscript asc <入口>.ts -o <entrypoint>，
// 在源码目录内执行（需 node/npx 环境）。入口约定：entrypoint 声明为
// <名>.wasm，对应源码 <名>.ts（与 schemaextract 的 main.ts 约定一致）。
func buildAssemblyScript(ctx context.Context, sourceDir string, manifest packagecontract.Manifest, spec BuildSpec) error {
	return buildHostedComponents(ctx, sourceDir, manifest, spec,
		func(ctx context.Context, workDir string, component packagecontract.Component, output string) ([]byte, error) {
			// 源码名 = entrypoint 去 .wasm 后缀 + .ts（main.wasm → main.ts）。
			input := filepath.Join(workDir, strings.TrimSuffix(filepath.Base(component.Entrypoint), ".wasm")+".ts")
			command := exec.CommandContext(ctx, "npx", "--yes", "--package", assemblyScriptPackage, "asc", input, "-o", output)
			command.Dir = workDir
			return command.CombinedOutput()
		})
}

// buildHostedComponents 统一校验包路径、输出目录并逐个构建 hosted 组件。
func buildHostedComponents(
	ctx context.Context,
	sourceDir string,
	manifest packagecontract.Manifest,
	spec BuildSpec,
	build func(context.Context, string, packagecontract.Component, string) ([]byte, error),
) error {
	absoluteSourceDir, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("%w: 解析包目录: %v", ErrBuildFailed, err)
	}
	source := spec.Source
	if source == "" {
		source = "."
	}
	if !packagecontract.IsPackagePath(source) {
		return fmt.Errorf("%w: 源码目录非法 %q", ErrBuildFailed, source)
	}
	workDir := filepath.Join(absoluteSourceDir, source)
	for _, component := range manifest.Components {
		if component.Mode != packagecontract.ModeHosted {
			continue
		}
		if len(spec.Components) > 0 && !slices.Contains(spec.Components, component.ID) {
			continue
		}
		if component.Entrypoint == "" || !packagecontract.IsPackagePath(component.Entrypoint) {
			return fmt.Errorf("%w: 组件 %s entrypoint 非法 %q", ErrBuildFailed, component.ID, component.Entrypoint)
		}
		output := filepath.Join(absoluteSourceDir, component.Entrypoint)
		if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
			return fmt.Errorf("%w: 创建输出目录: %v", ErrBuildFailed, err)
		}
		outputBytes, err := build(ctx, workDir, component, output)
		if err != nil {
			return fmt.Errorf("%w: 编译组件 %s: %v\n%s", ErrBuildFailed, component.ID, err, outputBytes)
		}
	}
	return nil
}
