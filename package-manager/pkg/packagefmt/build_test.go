package packagefmt

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// TestParseBuildSpec 验证 component 级 build 段被解析为 BuildSpec，未声明时为空。
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

[component.build]
tool = "go-wasm"
source = "guest"
`)

	_, _, builds, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(builds) != 1 || builds[0].Tool != BuildToolGoWasm || builds[0].Source != "guest" ||
		len(builds[0].Components) != 1 || builds[0].Components[0] != "core" {
		t.Fatalf("builds = %+v, want one go-wasm plan for core", builds)
	}

	// 未声明 component build → 空计划。
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
	_, _, builds, err = Parse(pathPlain)
	if err != nil {
		t.Fatalf("Parse plain: %v", err)
	}
	if len(builds) != 0 {
		t.Fatalf("builds = %+v, want empty for plain source", builds)
	}
}

func TestParseBuildSpecsPerComponent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SourceFileName)
	writeSource(t, path, `
[package]
id = "mixed.pkg"
version = "1.0.0"

[[component]]
id = "brain"
mode = "isolated"
role = "executor"
entrypoint = "brain"

[component.process]
path = "python"

[component.build]
tool = "python-uv"
source = "python"

[[component]]
id = "prefs"
mode = "hosted"
entrypoint = "prefs.wasm"

[component.build]
tool = "go-wasm"
source = "go"
`)
	_, _, builds, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(builds) != 2 || builds[0].Tool != BuildToolPythonUV || builds[0].Source != "python" ||
		len(builds[0].Components) != 1 || builds[0].Components[0] != "brain" ||
		builds[1].Tool != BuildToolGoWasm || builds[1].Source != "go" ||
		len(builds[1].Components) != 1 || builds[1].Components[0] != "prefs" {
		t.Fatalf("builds = %+v, want component-specific python and go plans", builds)
	}
}

func TestBuildRejectsInvalidComponentTargets(t *testing.T) {
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion,
		ID:            "mixed.pkg",
		Version:       "1.0.0",
		Components: []packagecontract.Component{
			{ID: "brain", Mode: packagecontract.ModeIsolated, Role: packagecontract.RoleExecutor, Entrypoint: "brain",
				Process: &packagecontract.ProcessTemplate{Path: "brain", Address: "127.0.0.1:50051"}},
			{ID: "prefs", Mode: packagecontract.ModeHosted, Entrypoint: "prefs.wasm"},
		},
	}
	for _, test := range []struct {
		name string
		spec BuildSpec
	}{
		{name: "unknown component", spec: BuildSpec{Tool: BuildToolGoWasm, Components: []string{"missing"}}},
		{name: "hosted builder on isolated component", spec: BuildSpec{Tool: BuildToolGoWasm, Components: []string{"brain"}}},
		{name: "isolated builder on hosted component", spec: BuildSpec{Tool: BuildToolPythonUV, Components: []string{"prefs"}}},
		{name: "duplicate component", spec: BuildSpec{Tool: BuildToolGoWasm, Components: []string{"prefs", "prefs"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Build(context.Background(), t.TempDir(), manifest, []BuildSpec{test.spec}); err == nil {
				t.Fatal("invalid component build target was accepted")
			}
		})
	}
	t.Run("no matching component", func(t *testing.T) {
		hostedOnly := packagecontract.Manifest{
			SchemaVersion: packagecontract.SchemaVersion, ID: "hosted.pkg", Version: "1.0.0",
			Components: []packagecontract.Component{{ID: "main", Mode: packagecontract.ModeHosted, Entrypoint: "main.wasm"}},
		}
		if err := Build(context.Background(), t.TempDir(), hostedOnly, []BuildSpec{{Tool: BuildToolGoNative}}); err == nil {
			t.Fatal("build plan without a matching component was accepted")
		}
	})
	t.Run("duplicate across plans", func(t *testing.T) {
		if err := Build(context.Background(), t.TempDir(), manifest, []BuildSpec{
			{Tool: BuildToolGoWasm, Components: []string{"prefs"}},
			{Tool: BuildToolAssemblyScript, Components: []string{"prefs"}},
		}); err == nil {
			t.Fatal("component targeted by multiple build plans was accepted")
		}
	})
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

[component.build]
tool = "rust-wasm"
`)
	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("unknown build tool was accepted by Parse")
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

[component.build]
tool = "go-wasm"
source = "guest"
`)
	manifest, _, builds, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Build(context.Background(), sourceDir, manifest, builds); err != nil {
		t.Fatalf("Build: %v", err)
	}
	info, err := os.Stat(filepath.Join(sourceDir, "demo.wasm"))
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		t.Fatalf("编译产物缺失或非法: %v size=%d", err, info.Size())
	}
}

// TestBuildGoNativeBuildsExecutable 验证 Go isolated 构建器输出当前平台的可执行
// 工件，并使用包自身的 go.mod，不依赖仓库根模块。
func TestBuildGoNativeBuildsExecutable(t *testing.T) {
	sourceDir := t.TempDir()
	mainDir := filepath.Join(sourceDir, "cmd", "native-executor")
	if err := os.MkdirAll(mainDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeSource(t, filepath.Join(mainDir, "main.go"), `package main

func main() {}
`)
	writeSource(t, filepath.Join(sourceDir, "go.mod"), "module demo.executor\n\ngo 1.24\n")
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "demo.executor", Version: "1.0.0",
		Components: []packagecontract.Component{{
			ID: "executor", Mode: packagecontract.ModeIsolated, Role: packagecontract.RoleExecutor,
			Entrypoint: "native-executor",
			Process:    &packagecontract.ProcessTemplate{Path: "native-executor", Address: "127.0.0.1:50051"},
		}},
	}
	if err := Build(context.Background(), sourceDir, manifest, []BuildSpec{{
		Tool: BuildToolGoNative, Source: "cmd/native-executor", Components: []string{"executor"},
	}}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	info, err := os.Stat(filepath.Join(sourceDir, "native-executor"))
	if err != nil {
		t.Fatalf("native executable missing: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("native executable is invalid: size=%d", info.Size())
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

[component.build]
tool = "go-wasm"
source = "../outside"
`)
	if _, _, _, err := Parse(path); err == nil {
		t.Fatal("escaping build source was accepted by Parse")
	}
}
