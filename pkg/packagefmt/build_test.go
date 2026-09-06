package packagefmt

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestParseBuildSpec 验证 [build] 段被解析为 BuildSpec，未声明时为 nil。
func TestParseBuildSpec(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"

[build]
tool = "go-wasm"
source = "guest"
`)

	_, _, build, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if build == nil || build.Tool != BuildToolGoWasm || build.Source != "guest" {
		t.Fatalf("build = %+v, want go-wasm with package-relative source", build)
	}

	// 未声明 [build] → nil。
	pathPlain := filepath.Join(dir, "plain", SourceFileName)
	if err := os.MkdirAll(filepath.Dir(pathPlain), 0o750); err != nil {
		t.Fatal(err)
	}
	writeSource(t, pathPlain, `
[package]
id = "plain.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "plain.wasm"
`)
	_, _, build, err = Parse(pathPlain)
	if err != nil {
		t.Fatalf("Parse plain: %v", err)
	}
	if build != nil {
		t.Fatalf("build = %+v, want nil for plain source", build)
	}
}

// TestBuildUnknownToolFailsClosed 验证未知构建器被拒绝（不静默跳过构建）。
func TestBuildUnknownToolFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"

[build]
tool = "rust-wasm"
`)
	manifest, _, build, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Build(context.Background(), dir, manifest, *build); err == nil {
		t.Fatal("unknown build tool = nil, want error")
	}
}

// TestBuildGoWasmCrossCompiles 验证 go-wasm 构建器真实交叉编译（跳过 wasm 平台
// 与无 Go 工具链环境；构建产物可被 packmgr 工件校验接受）。
func TestBuildGoWasmCrossCompiles(t *testing.T) {
	if runtime.GOARCH == "wasm" {
		t.Skip("当前平台自身是 wasm")
	}
	sourceDir := t.TempDir()
	guestDir := filepath.Join(sourceDir, "guest")
	if err := os.MkdirAll(guestDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeSource(t, filepath.Join(guestDir, "main.go"), `//go:build wasip1

package main

import "os"

func main() { os.Exit(0) }
`)
	// 包目录内 go.mod：源码自包含，构建器不依赖仓库根模块。
	writeSource(t, filepath.Join(sourceDir, "go.mod"), "module demo.pkg\n\ngo 1.24\n")
	path := filepath.Join(sourceDir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"

[build]
tool = "go-wasm"
source = "guest"
`)
	manifest, _, build, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Build(context.Background(), sourceDir, manifest, *build); err != nil {
		t.Fatalf("Build: %v", err)
	}
	info, err := os.Stat(filepath.Join(sourceDir, "demo.wasm"))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		t.Fatalf("编译产物缺失或非法: %v size=%d", err, info.Size())
	}
}

// TestBuildRejectsEscapingSource 验证源码目录逃逸包目录被拒绝（包自包含边界）。
func TestBuildRejectsEscapingSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "demo.pkg"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "demo.wasm"

[build]
tool = "go-wasm"
source = "../outside"
`)
	manifest, _, build, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Build(context.Background(), dir, manifest, *build); err == nil {
		t.Fatal("escaping source = nil, want error")
	}
}
