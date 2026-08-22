package packmgr_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
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
	validComponents := []packmgr.Component{{
		ID: "bus.core", Mode: packmgr.ModeHosted, Entrypoint: "bus-core.wasm",
	}}
	valid := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: "campus.bus", Version: "1.0.0",
		Components: validComponents,
	}
	validJSON := json.RawMessage(`{"tools":[]}`)
	cases := []struct {
		name   string
		mutate func(*packmgr.Manifest)
	}{
		{name: "wrong schema version", mutate: func(m *packmgr.Manifest) { m.SchemaVersion = "ailuo.install.v2" }},
		{name: "invalid id", mutate: func(m *packmgr.Manifest) { m.ID = "Campus.Bus" }},
		{name: "invalid version", mutate: func(m *packmgr.Manifest) { m.Version = "1.2" }},
		{name: "excessive idle ttl", mutate: func(m *packmgr.Manifest) { m.IdleTTLMS = 30*24*3600*1000 + 1 }},
		{name: "empty components", mutate: func(m *packmgr.Manifest) { m.Components = nil }},
		{name: "unsupported mode", mutate: func(m *packmgr.Manifest) {
			m.Components[0].Mode = "embedded"
		}},
		{name: "missing entrypoint", mutate: func(m *packmgr.Manifest) { m.Components[0].Entrypoint = "" }},
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
			manifest := valid
			manifest.Extensions = validJSON
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
	artifactPath := filepath.Join(t.TempDir(), "bus-core.wasm")
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
			{ComponentID: "bus.core", Path: artifactPath,
				SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{ComponentID: "bus.adapter", Path: artifactPath,
				SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				Process: &packmgr.ProcessSpec{
					Path: artifactPath, WorkDir: t.TempDir(), Address: "127.0.0.1:9000",
				}},
		},
	}
	if err := packmgr.ValidateLock(lock, manifest); err != nil {
		t.Fatalf("ValidateLock: %v", err)
	}
	// hosted 组件不得携带进程规格。
	bad := lock
	bad.Artifacts[0].Process = &packmgr.ProcessSpec{Path: artifactPath, WorkDir: t.TempDir(), Address: "127.0.0.1:9001"}
	if err := packmgr.ValidateLock(bad, manifest); err == nil {
		t.Fatal("ValidateLock accepted hosted component with process spec")
	}
}

func TestValidateProcessSpecRejectsNonLoopback(t *testing.T) {
	spec := packmgr.ProcessSpec{Address: "192.0.2.1:9000"}
	if err := packmgr.ValidateProcessSpec(spec); err == nil {
		t.Fatal("ValidateProcessSpec accepted non-loopback address")
	}
}

func TestSchemaVersionConstantIsNeutral(t *testing.T) {
	if packmgr.SchemaVersion != "ailuo.package.v1" {
		t.Fatalf("SchemaVersion = %q, want ailuo.package.v1", packmgr.SchemaVersion)
	}
}

func TestValidateDependencyRejectsMalformedConstraint(t *testing.T) {
	if err := packmgr.ValidateDependency(packmgr.Dependency{ID: "bus.query", Constraint: "not-a-constraint"}); !errors.Is(err, packmgr.ErrInvalidFormat) {
		t.Fatalf("ValidateDependency error = %v, want ErrInvalidFormat", err)
	}
}
