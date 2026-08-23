// Package packagefmt 的构建层：ailuo.toml 的 `[build]` 段声明构建方式，
// ailuo pack 统一驱动，不依赖每包手写 build.sh/build.ps1。
package packagefmt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
)

// BuildToolGoWasm 是内置的 Go→wasm32-wasi 构建器：跨平台一致（os/exec 设
// GOOS/GOARCH，不依赖 shell），Go 工具链是仓库门禁既有前提。
const BuildToolGoWasm = "go-wasm"

var (
	// ErrBuildUnsupported 是声明了未知构建器的错误（fail-closed，不静默跳过构建）。
	ErrBuildUnsupported = errors.New("unsupported build tool")
	// ErrBuildFailed 是构建执行失败的包裹错误。
	ErrBuildFailed = errors.New("build failed")
)

// BuildSpec 是 ailuo.toml 的 `[build]` 段（TOML 键与字段名一致）：源码目录
// 相对包目录（缺省 "."），必须位于包目录内（包自包含：构建器不越界进内核
// 目录，第三方作者按同一规则写包）。
type BuildSpec struct {
	Tool   string `toml:"tool"`
	Source string `toml:"source,omitempty"`
}

// Build 执行 [build] 声明的构建：为每个 hosted 组件交叉编译 entrypoint 工件。
// 当前支持 go-wasm（内置）；未知工具 fail-closed。源码目录相对包目录解析，
// 校验不逃逸包目录（path traversal 拒绝）。
func Build(ctx context.Context, sourceDir string, manifest packmgr.Manifest, spec BuildSpec) error {
	switch spec.Tool {
	case BuildToolGoWasm:
		return buildGoWasm(ctx, sourceDir, manifest, spec)
	default:
		return fmt.Errorf("%w: %q（支持：go-wasm）", ErrBuildUnsupported, spec.Tool)
	}
}

// buildGoWasm 用 Go 工具链交叉编译每个 hosted 组件：GOOS=wasip1 GOARCH=wasm
// go build -trimpath -o <entrypoint> .，在源码目录内执行（go.mod 来自包目录
// 或仓库根，向上查找；跨平台一致，无 shell 脚本）。工件输出到包目录根
// （entrypoint 相对包目录），源码目录是包目录内的相对位置。sourceDir 转绝对
// 路径，避免输出相对 workDir 解析错位。
func buildGoWasm(ctx context.Context, sourceDir string, manifest packmgr.Manifest, spec BuildSpec) error {
	if runtime.GOARCH == "wasm" {
		return fmt.Errorf("%w: 当前平台自身是 wasm，无法交叉编译", ErrBuildFailed)
	}
	absoluteSourceDir, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("%w: 解析包目录: %v", ErrBuildFailed, err)
	}
	source := spec.Source
	if source == "" {
		source = "."
	}
	if !filepath.IsLocal(source) {
		return fmt.Errorf("%w: 源码目录非法 %q", ErrBuildFailed, source)
	}
	workDir := filepath.Join(absoluteSourceDir, source)
	for _, component := range manifest.Components {
		if component.Mode != packmgr.ModeHosted {
			continue
		}
		if component.Entrypoint == "" || !filepath.IsLocal(component.Entrypoint) {
			return fmt.Errorf("%w: 组件 %s entrypoint 非法 %q", ErrBuildFailed, component.ID, component.Entrypoint)
		}
		output := filepath.Join(absoluteSourceDir, component.Entrypoint)
		if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
			return fmt.Errorf("%w: 创建输出目录: %v", ErrBuildFailed, err)
		}
		command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", output, ".")
		command.Dir = workDir
		command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
		outputBytes, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: 编译组件 %s: %v\n%s", ErrBuildFailed, component.ID, err, outputBytes)
		}
	}
	return nil
}
