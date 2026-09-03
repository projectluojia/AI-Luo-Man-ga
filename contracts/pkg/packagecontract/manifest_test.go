package packagecontract_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

func TestValidateManifestAcceptsNeutralCore(t *testing.T) {
	extensions := json.RawMessage(`{"tools":[],"service":{},"capabilities":[]}`)
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "campus.bus", Version: "1.2.3",
		Pin: true, IdleTTLMS: 1000, Extensions: extensions,
		Components: []packagecontract.Component{{
			ID: "bus.core", Mode: packagecontract.ModeHosted, Entrypoint: "bus-core.wasm",
			Exports: []string{"campus.bus.query"}, Imports: []string{"campus.bus.transport"},
			HostFunctions: []packagecontract.HostedFunctionDecl{{Module: "ailuo.bus", Name: "query", Purpose: "权威存储查询"}},
		}},
		Storage: &packagecontract.Storage{
			Namespace: "campus/bus", SchemaVersion: 1,
			Sensitivity: packagecontract.SensitivityPublic, Retention: packagecontract.RetentionPermanent,
		},
		Dependencies: []packagecontract.Dependency{{ID: "bus.transport", Constraint: "^1.0.0", Source: "github:owner/repo"}},
	}
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifestAcceptsIsolatedProcessTemplate(t *testing.T) {
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "agent", Version: "1.0.0",
		Components: []packagecontract.Component{{
			ID: "executor", Mode: packagecontract.ModeIsolated, Role: packagecontract.RoleExecutor,
			Entrypoint: "runtime",
			Process: &packagecontract.ProcessTemplate{
				Path: ".venv/python", WorkDir: ".", Address: "127.0.0.1:50051",
			},
		}},
	}
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	if err := packagecontract.ValidateProcessTemplate(packagecontract.ProcessTemplate{Path: "../python"}); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("invalid process template error = %v, want ErrInvalidFormat", err)
	}
}

func TestValidateManifestRejectsInvalidCore(t *testing.T) {
	// 每个用例独立构造：Manifest 结构体复制不复制 Components 底层数组，
	// 共用一份会让 `m.Components[0].Mode = ...` 之类的改动污染后续子测试。
	validManifest := func() packagecontract.Manifest {
		return packagecontract.Manifest{
			SchemaVersion: packagecontract.SchemaVersion, ID: "campus.bus", Version: "1.0.0",
			Extensions: json.RawMessage(`{"tools":[]}`),
			Components: []packagecontract.Component{{
				ID: "bus.core", Mode: packagecontract.ModeHosted, Entrypoint: "bus-core.wasm",
			}},
		}
	}
	cases := []struct {
		name   string
		mutate func(*packagecontract.Manifest)
	}{
		{name: "wrong schema version", mutate: func(m *packagecontract.Manifest) { m.SchemaVersion = "ailuo.package.v1" }},
		{name: "invalid id", mutate: func(m *packagecontract.Manifest) { m.ID = "Campus.Bus" }},
		{name: "invalid version", mutate: func(m *packagecontract.Manifest) { m.Version = "1.2" }},
		{name: "excessive idle ttl", mutate: func(m *packagecontract.Manifest) { m.IdleTTLMS = 30*24*3600*1000 + 1 }},
		{name: "empty components", mutate: func(m *packagecontract.Manifest) { m.Components = nil }},
		{name: "unsupported mode", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].Mode = "embedded"
		}},
		{name: "isolated missing process template", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].Mode = packagecontract.ModeIsolated
		}},
		{name: "missing entrypoint", mutate: func(m *packagecontract.Manifest) { m.Components[0].Entrypoint = "" }},
		{name: "absolute entrypoint", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].Entrypoint = filepath.Join(string(filepath.Separator), "opt", "evil.wasm")
		}},
		{name: "escaping entrypoint", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].Entrypoint = filepath.Join("..", "..", "evil.wasm")
		}},
		{name: "duplicate component id", mutate: func(m *packagecontract.Manifest) {
			m.Components = append(m.Components, packagecontract.Component{ID: "bus.core", Mode: packagecontract.ModeHosted, Entrypoint: "x"})
		}},
		{name: "invalid export id", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].Exports = []string{"Campus.bus.query"}
		}},
		{name: "duplicate host function", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].HostFunctions = []packagecontract.HostedFunctionDecl{
				{Module: "ailuo.x", Name: "one"}, {Module: "ailuo.x", Name: "one"},
			}
		}},
		{name: "wasi module reserved", mutate: func(m *packagecontract.Manifest) {
			m.Components[0].HostFunctions = []packagecontract.HostedFunctionDecl{{Module: "wasi_snapshot_preview1", Name: "fd_write"}}
		}},
		{name: "invalid storage", mutate: func(m *packagecontract.Manifest) {
			m.Storage = &packagecontract.Storage{Namespace: "campus/bus", SchemaVersion: 0,
				Sensitivity: packagecontract.SensitivityPublic, Retention: packagecontract.RetentionPermanent}
		}},
		{name: "invalid dependency id", mutate: func(m *packagecontract.Manifest) {
			m.Dependencies = []packagecontract.Dependency{{ID: "Bus.query", Constraint: "^1.0.0"}}
		}},
		{name: "invalid dependency constraint", mutate: func(m *packagecontract.Manifest) {
			m.Dependencies = []packagecontract.Dependency{{ID: "bus.query", Constraint: "^"}}
		}},
		{name: "invalid extensions json", mutate: func(m *packagecontract.Manifest) { m.Extensions = json.RawMessage(`{`) }},
		{name: "capability exported twice", mutate: func(m *packagecontract.Manifest) {
			m.Components = append(m.Components, packagecontract.Component{
				ID: "bus.other", Mode: packagecontract.ModeHosted, Entrypoint: "x",
				Exports: []string{"campus.bus.query"},
			})
			m.Components[0].Exports = []string{"campus.bus.query"}
		}},
		{name: "cyclic imports", mutate: func(m *packagecontract.Manifest) {
			m.Components = []packagecontract.Component{
				{ID: "a", Mode: packagecontract.ModeHosted, Entrypoint: "a", Exports: []string{"x"}, Imports: []string{"y"}},
				{ID: "b", Mode: packagecontract.ModeHosted, Entrypoint: "b", Exports: []string{"y"}, Imports: []string{"x"}},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := validManifest()
			tc.mutate(&manifest)
			if err := packagecontract.ValidateManifest(manifest); err == nil {
				t.Fatalf("ValidateManifest accepted invalid manifest: %+v", manifest)
			}
		})
	}
}

func TestComponentOrderRespectsDependencyTopology(t *testing.T) {
	components := []packagecontract.Component{
		{ID: "bus.core", Mode: packagecontract.ModeHosted, Entrypoint: "core.wasm",
			Imports: []string{"campus.bus.transport"}},
		{ID: "bus.adapter", Mode: packagecontract.ModeIsolated, Entrypoint: "adapter",
			Process: &packagecontract.ProcessTemplate{Path: "adapter", Address: "127.0.0.1:9000"},
			Exports: []string{"campus.bus.transport"}},
		{ID: "bus.standalone", Mode: packagecontract.ModeHosted, Entrypoint: "solo.wasm"},
	}
	order, err := packagecontract.ComponentOrder(components)
	if err != nil {
		t.Fatalf("ComponentOrder: %v", err)
	}
	indexOf := func(id string) int {
		for i, componentID := range order {
			if componentID == id {
				return i
			}
		}
		t.Fatalf("component %q missing from order %v", id, order)
		return -1
	}
	if indexOf("bus.adapter") >= indexOf("bus.core") {
		t.Fatalf("provider bus.adapter must start before consumer bus.core: order=%v", order)
	}
}

func TestComponentOrderRejectsCycle(t *testing.T) {
	components := []packagecontract.Component{
		{ID: "a", Mode: packagecontract.ModeHosted, Entrypoint: "a", Exports: []string{"x"}, Imports: []string{"y"}},
		{ID: "b", Mode: packagecontract.ModeHosted, Entrypoint: "b", Exports: []string{"y"}, Imports: []string{"x"}},
	}
	if _, err := packagecontract.ComponentOrder(components); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("ComponentOrder cycle error = %v, want ErrInvalidFormat", err)
	}
}

func TestValidateLockMatchesComponents(t *testing.T) {
	installDir := t.TempDir()
	corePath := filepath.Join(installDir, "bus-core.wasm")
	adapterPath := filepath.Join(installDir, "bus-adapter")
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion, ID: "campus.bus", Version: "1.0.0",
		Components: []packagecontract.Component{
			{ID: "bus.core", Mode: packagecontract.ModeHosted, Entrypoint: "bus-core.wasm"},
			{ID: "bus.adapter", Mode: packagecontract.ModeIsolated, Entrypoint: "bus-adapter",
				Process: &packagecontract.ProcessTemplate{Path: "bus-adapter", Address: "127.0.0.1:9000"}},
		},
	}
	lock := packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: "campus.bus",
		PackageVersion: "1.0.0",
		ManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Artifacts: []packagecontract.LockedArtifact{
			{ComponentID: "bus.core", Path: corePath,
				SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{ComponentID: "bus.adapter", Path: adapterPath,
				SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				Process: &packagecontract.ProcessSpec{
					Path: adapterPath, WorkDir: installDir, Address: "127.0.0.1:9000",
				}},
		},
	}
	if err := packagecontract.ValidateLock(lock, manifest); err != nil {
		t.Fatalf("ValidateLock: %v", err)
	}
	// hosted 组件不得携带进程规格。Artifacts 必须深拷贝：Lock 结构体复制共用
	// 底层数组，直接改 bad.Artifacts[0] 会同时改掉上面已校验过的 lock。
	cloneLock := func() packagecontract.Lock {
		copied := lock
		copied.Artifacts = append([]packagecontract.LockedArtifact(nil), lock.Artifacts...)
		return copied
	}
	bad := cloneLock()
	bad.Artifacts[0].Process = &packagecontract.ProcessSpec{Path: corePath, WorkDir: t.TempDir(), Address: "127.0.0.1:9001"}
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted hosted component with process spec")
	}
	// lock 的工件文件名必须与清单 entrypoint 一致：否则摘要可以绑到包目录外
	// 任意一个绝对路径文件上，装载的就不是清单声明的工件。
	bad = cloneLock()
	bad.Artifacts[0].Path = filepath.Join(installDir, "other.wasm")
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted artifact path not matching entrypoint")
	}
	// 摘要必须是合法十六进制：只校验长度会让 64 个 "g" 混过完整性校验前置检查。
	bad = cloneLock()
	bad.Artifacts[0].SHA256 = strings.Repeat("g", 64)
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted non-hex artifact digest")
	}
	bad = cloneLock()
	bad.ManifestSHA256 = strings.Repeat("g", 64)
	if err := packagecontract.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted non-hex manifest digest")
	}
}

func TestValidateProcessSpecRejectsNonLoopback(t *testing.T) {
	spec := packagecontract.ProcessSpec{Address: "192.0.2.1:9000"}
	if err := packagecontract.ValidateProcessSpec(spec); err == nil {
		t.Fatal("ValidateProcessSpec accepted non-loopback address")
	}
}

func TestValidateProcessTemplateRejectsUnixAddressOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 使用该进程地址策略")
	}
	if err := packagecontract.ValidateProcessTemplate(packagecontract.ProcessTemplate{
		Path: "runner", Address: "unix:/runtime.sock",
	}); err == nil {
		t.Fatal("ValidateProcessTemplate accepted Unix address on Windows")
	}
}

func TestLocalRuntimeAddressPolicy(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9000", "[::1]:9000", "unix:/runtime.sock"} {
		if !packagecontract.IsLocalRuntimeAddress(address) {
			t.Errorf("IsLocalRuntimeAddress(%q) = false", address)
		}
	}
	for _, address := range []string{"0.0.0.0:9000", "192.0.2.1:9000", "localhost:9000", "unix:relative.sock", "unix:/runtime/../socket.sock"} {
		if packagecontract.IsLocalRuntimeAddress(address) {
			t.Errorf("IsLocalRuntimeAddress(%q) = true", address)
		}
	}
}

func TestSchemaVersionConstantIsNeutral(t *testing.T) {
	if packagecontract.SchemaVersion != "ailuo.package.v2" {
		t.Fatalf("SchemaVersion = %q, want ailuo.package.v2", packagecontract.SchemaVersion)
	}
}

func TestIsPackagePathIsPlatformNeutral(t *testing.T) {
	for _, valid := range []string{".", "guest", "guest/main.ts"} {
		if !packagecontract.IsPackagePath(valid) {
			t.Errorf("IsPackagePath(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", "../outside", `..\outside`, `C:\\pkg\\main.wasm`, "/tmp/main.wasm", "guest//main.ts", "guest/./main.ts"} {
		if packagecontract.IsPackagePath(invalid) {
			t.Errorf("IsPackagePath(%q) = true", invalid)
		}
	}
}

func TestValidateDependencyRejectsMalformedConstraint(t *testing.T) {
	if err := packagecontract.ValidateDependency(packagecontract.Dependency{ID: "bus.query", Constraint: "not-a-constraint"}); !errors.Is(err, packagecontract.ErrInvalidFormat) {
		t.Fatalf("ValidateDependency error = %v, want ErrInvalidFormat", err)
	}
}

func TestValidateSourceRequiresExplicitScheme(t *testing.T) {
	for _, source := range []string{"path:packages/demo", "github:owner/repo"} {
		if err := packagecontract.ValidateSource(source); err != nil {
			t.Fatalf("ValidateSource(%q): %v", source, err)
		}
	}
	for _, source := range []string{
		"", "packages/demo", "path:../demo", "github:owner", "github:owner/repo/extra",
		"github:./repo", "github:owner/..",
	} {
		if err := packagecontract.ValidateSource(source); !errors.Is(err, packagecontract.ErrInvalidFormat) {
			t.Fatalf("ValidateSource(%q) = %v, want ErrInvalidFormat", source, err)
		}
	}
}

// module 与 name 都允许含 `.`，因此 {"ailuo.bus","query"} 与 {"ailuo","bus.query"}
// 是两个不同声明；用 `.` 拼键会让它们撞成同一个键并被误判为重复声明。
func TestHostedFunctionKeyDistinguishesDottedSegments(t *testing.T) {
	decls := []packagecontract.HostedFunctionDecl{
		{Module: "ailuo.bus", Name: "query"},
		{Module: "ailuo", Name: "bus.query"},
	}
	if err := packagecontract.ValidateHostedFunctions(decls); err != nil {
		t.Fatalf("ValidateHostedFunctions: %v", err)
	}
	if packagecontract.HostedFunctionKey("ailuo.bus", "query") == packagecontract.HostedFunctionKey("ailuo", "bus.query") {
		t.Fatal("HostedFunctionKey 对歧义的 module/name 切分产生了相同键")
	}
}
