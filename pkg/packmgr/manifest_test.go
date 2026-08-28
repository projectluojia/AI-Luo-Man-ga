package packmgr_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

func TestValidateManifestAcceptsNeutralCore(t *testing.T) {
	extensions := json.RawMessage(`{"tools":[],"service":{},"capabilities":[]}`)
	manifest := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: "campus.bus", Version: "1.2.3",
		Pin: true, IdleTTLMS: 1000, Extensions: extensions,
		Components: []packmgr.Component{{
			ID: "bus.core", Mode: packmgr.ModeHosted, Entrypoint: "bus-core.wasm",
			Exports: []string{"campus.bus.query"}, Imports: []string{"campus.bus.transport"},
			HostFunctions: []packmgr.HostedFunctionDecl{{Module: "ailuo.bus", Name: "query", Purpose: "权威存储查询"}},
		}},
		Storage: &packmgr.Storage{
			Namespace: "campus/bus", SchemaVersion: 1,
			Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent,
		},
		Dependencies: []packmgr.Dependency{{ID: "bus.transport", Constraint: "^1.0.0"}},
	}
	if err := packmgr.ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifestRejectsInvalidCore(t *testing.T) {
	// 每个用例独立构造：Manifest 结构体复制不复制 Components 底层数组，
	// 共用一份会让 `m.Components[0].Mode = ...` 之类的改动污染后续子测试。
	validManifest := func() packmgr.Manifest {
		return packmgr.Manifest{
			SchemaVersion: packmgr.SchemaVersion, ID: "campus.bus", Version: "1.0.0",
			Extensions: json.RawMessage(`{"tools":[]}`),
			Components: []packmgr.Component{{
				ID: "bus.core", Mode: packmgr.ModeHosted, Entrypoint: "bus-core.wasm",
			}},
		}
	}
	cases := []struct {
		name   string
		mutate func(*packmgr.Manifest)
	}{
		{name: "wrong schema version", mutate: func(m *packmgr.Manifest) { m.SchemaVersion = "ailuo.package.v1" }},
		{name: "invalid id", mutate: func(m *packmgr.Manifest) { m.ID = "Campus.Bus" }},
		{name: "invalid version", mutate: func(m *packmgr.Manifest) { m.Version = "1.2" }},
		{name: "excessive idle ttl", mutate: func(m *packmgr.Manifest) { m.IdleTTLMS = 30*24*3600*1000 + 1 }},
		{name: "empty components", mutate: func(m *packmgr.Manifest) { m.Components = nil }},
		{name: "unsupported mode", mutate: func(m *packmgr.Manifest) {
			m.Components[0].Mode = "embedded"
		}},
		{name: "missing entrypoint", mutate: func(m *packmgr.Manifest) { m.Components[0].Entrypoint = "" }},
		{name: "absolute entrypoint", mutate: func(m *packmgr.Manifest) {
			m.Components[0].Entrypoint = filepath.Join(string(filepath.Separator), "opt", "evil.wasm")
		}},
		{name: "escaping entrypoint", mutate: func(m *packmgr.Manifest) {
			m.Components[0].Entrypoint = filepath.Join("..", "..", "evil.wasm")
		}},
		{name: "duplicate component id", mutate: func(m *packmgr.Manifest) {
			m.Components = append(m.Components, packmgr.Component{ID: "bus.core", Mode: packmgr.ModeHosted, Entrypoint: "x"})
		}},
		{name: "invalid export id", mutate: func(m *packmgr.Manifest) {
			m.Components[0].Exports = []string{"Campus.bus.query"}
		}},
		{name: "duplicate host function", mutate: func(m *packmgr.Manifest) {
			m.Components[0].HostFunctions = []packmgr.HostedFunctionDecl{
				{Module: "ailuo.x", Name: "one"}, {Module: "ailuo.x", Name: "one"},
			}
		}},
		{name: "wasi module reserved", mutate: func(m *packmgr.Manifest) {
			m.Components[0].HostFunctions = []packmgr.HostedFunctionDecl{{Module: "wasi_snapshot_preview1", Name: "fd_write"}}
		}},
		{name: "invalid storage", mutate: func(m *packmgr.Manifest) {
			m.Storage = &packmgr.Storage{Namespace: "campus/bus", SchemaVersion: 0,
				Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent}
		}},
		{name: "invalid dependency id", mutate: func(m *packmgr.Manifest) {
			m.Dependencies = []packmgr.Dependency{{ID: "Bus.query", Constraint: "^1.0.0"}}
		}},
		{name: "invalid dependency constraint", mutate: func(m *packmgr.Manifest) {
			m.Dependencies = []packmgr.Dependency{{ID: "bus.query", Constraint: "^"}}
		}},
		{name: "invalid extensions json", mutate: func(m *packmgr.Manifest) { m.Extensions = json.RawMessage(`{`) }},
		{name: "capability exported twice", mutate: func(m *packmgr.Manifest) {
			m.Components = append(m.Components, packmgr.Component{
				ID: "bus.other", Mode: packmgr.ModeHosted, Entrypoint: "x",
				Exports: []string{"campus.bus.query"},
			})
			m.Components[0].Exports = []string{"campus.bus.query"}
		}},
		{name: "cyclic imports", mutate: func(m *packmgr.Manifest) {
			m.Components = []packmgr.Component{
				{ID: "a", Mode: packmgr.ModeHosted, Entrypoint: "a", Exports: []string{"x"}, Imports: []string{"y"}},
				{ID: "b", Mode: packmgr.ModeHosted, Entrypoint: "b", Exports: []string{"y"}, Imports: []string{"x"}},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := validManifest()
			tc.mutate(&manifest)
			if err := packmgr.ValidateManifest(manifest); err == nil {
				t.Fatalf("ValidateManifest accepted invalid manifest: %+v", manifest)
			}
		})
	}
}

func TestComponentOrderRespectsDependencyTopology(t *testing.T) {
	components := []packmgr.Component{
		{ID: "bus.core", Mode: packmgr.ModeHosted, Entrypoint: "core.wasm",
			Imports: []string{"campus.bus.transport"}},
		{ID: "bus.adapter", Mode: packmgr.ModeIsolated, Entrypoint: "adapter",
			Exports: []string{"campus.bus.transport"}},
		{ID: "bus.standalone", Mode: packmgr.ModeHosted, Entrypoint: "solo.wasm"},
	}
	order, err := packmgr.ComponentOrder(components)
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
	components := []packmgr.Component{
		{ID: "a", Mode: packmgr.ModeHosted, Entrypoint: "a", Exports: []string{"x"}, Imports: []string{"y"}},
		{ID: "b", Mode: packmgr.ModeHosted, Entrypoint: "b", Exports: []string{"y"}, Imports: []string{"x"}},
	}
	if _, err := packmgr.ComponentOrder(components); !errors.Is(err, packmgr.ErrInvalidFormat) {
		t.Fatalf("ComponentOrder cycle error = %v, want ErrInvalidFormat", err)
	}
}

func TestValidateLockMatchesComponents(t *testing.T) {
	installDir := t.TempDir()
	corePath := filepath.Join(installDir, "bus-core.wasm")
	adapterPath := filepath.Join(installDir, "bus-adapter")
	manifest := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: "campus.bus", Version: "1.0.0",
		Components: []packmgr.Component{
			{ID: "bus.core", Mode: packmgr.ModeHosted, Entrypoint: "bus-core.wasm"},
			{ID: "bus.adapter", Mode: packmgr.ModeIsolated, Entrypoint: "bus-adapter"},
		},
	}
	lock := packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion, PackageID: "campus.bus",
		PackageVersion: "1.0.0",
		ManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Artifacts: []packmgr.LockedArtifact{
			{ComponentID: "bus.core", Path: corePath,
				SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{ComponentID: "bus.adapter", Path: adapterPath,
				SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				Process: &packmgr.ProcessSpec{
					Path: adapterPath, WorkDir: t.TempDir(), Address: "127.0.0.1:9000",
				}},
		},
	}
	if err := packmgr.ValidateLock(lock, manifest); err != nil {
		t.Fatalf("ValidateLock: %v", err)
	}
	// hosted 组件不得携带进程规格。Artifacts 必须深拷贝：Lock 结构体复制共用
	// 底层数组，直接改 bad.Artifacts[0] 会同时改掉上面已校验过的 lock。
	cloneLock := func() packmgr.Lock {
		copied := lock
		copied.Artifacts = append([]packmgr.LockedArtifact(nil), lock.Artifacts...)
		return copied
	}
	bad := cloneLock()
	bad.Artifacts[0].Process = &packmgr.ProcessSpec{Path: corePath, WorkDir: t.TempDir(), Address: "127.0.0.1:9001"}
	if err := packmgr.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted hosted component with process spec")
	}
	// lock 的工件文件名必须与清单 entrypoint 一致：否则摘要可以绑到包目录外
	// 任意一个绝对路径文件上，装载的就不是清单声明的工件。
	bad = cloneLock()
	bad.Artifacts[0].Path = filepath.Join(installDir, "other.wasm")
	if err := packmgr.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted artifact path not matching entrypoint")
	}
	// 摘要必须是合法十六进制：只校验长度会让 64 个 "g" 混过完整性校验前置检查。
	bad = cloneLock()
	bad.Artifacts[0].SHA256 = strings.Repeat("g", 64)
	if err := packmgr.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted non-hex artifact digest")
	}
	bad = cloneLock()
	bad.ManifestSHA256 = strings.Repeat("g", 64)
	if err := packmgr.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted non-hex manifest digest")
	}
}

func TestValidateProcessSpecRejectsNonLoopback(t *testing.T) {
	spec := packmgr.ProcessSpec{Address: "192.0.2.1:9000"}
	if err := packmgr.ValidateProcessSpec(spec); err == nil {
		t.Fatal("ValidateProcessSpec accepted non-loopback address")
	}
}

func TestSchemaVersionConstantIsNeutral(t *testing.T) {
	if packmgr.SchemaVersion != "ailuo.package.v2" {
		t.Fatalf("SchemaVersion = %q, want ailuo.package.v2", packmgr.SchemaVersion)
	}
}

func TestIsPackagePathIsPlatformNeutral(t *testing.T) {
	for _, valid := range []string{".", "guest", "guest/main.ts"} {
		if !packmgr.IsPackagePath(valid) {
			t.Errorf("IsPackagePath(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", "../outside", `..\outside`, `C:\\pkg\\main.wasm`, "/tmp/main.wasm", "guest//main.ts", "guest/./main.ts"} {
		if packmgr.IsPackagePath(invalid) {
			t.Errorf("IsPackagePath(%q) = true", invalid)
		}
	}
}

func TestValidateDependencyRejectsMalformedConstraint(t *testing.T) {
	if err := packmgr.ValidateDependency(packmgr.Dependency{ID: "bus.query", Constraint: "not-a-constraint"}); !errors.Is(err, packmgr.ErrInvalidFormat) {
		t.Fatalf("ValidateDependency error = %v, want ErrInvalidFormat", err)
	}
}

// module 与 name 都允许含 `.`，因此 {"ailuo.bus","query"} 与 {"ailuo","bus.query"}
// 是两个不同声明；用 `.` 拼键会让它们撞成同一个键并被误判为重复声明。
func TestHostedFunctionKeyDistinguishesDottedSegments(t *testing.T) {
	decls := []packmgr.HostedFunctionDecl{
		{Module: "ailuo.bus", Name: "query"},
		{Module: "ailuo", Name: "bus.query"},
	}
	if err := packmgr.ValidateHostedFunctions(decls); err != nil {
		t.Fatalf("ValidateHostedFunctions: %v", err)
	}
	if packmgr.HostedFunctionKey("ailuo.bus", "query") == packmgr.HostedFunctionKey("ailuo", "bus.query") {
		t.Fatal("HostedFunctionKey 对歧义的 module/name 切分产生了相同键")
	}
}
