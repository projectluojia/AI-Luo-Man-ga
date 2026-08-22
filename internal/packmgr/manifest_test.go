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
		Mode: packmgr.ModeHosted, Pin: true, IdleTTLMS: 1000, Extensions: extensions,
		HostFunctions: []packmgr.HostedFunctionDecl{{Module: "ailuo.bus", Name: "query", Purpose: "权威存储查询"}},
		Storage: &packmgr.Storage{
			Namespace: "campus/bus", SchemaVersion: 1,
			Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent,
		},
		Dependencies: []packmgr.Dependency{{ID: "bus.query", Constraint: "^1.0.0"}},
	}
	if err := packmgr.ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
}

func TestValidateManifestRejectsInvalidCore(t *testing.T) {
	valid := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion, ID: "campus.bus", Version: "1.0.0",
		Mode: packmgr.ModeHosted,
	}
	validJSON := json.RawMessage(`{"tools":[]}`)
	cases := []struct {
		name   string
		mutate func(*packmgr.Manifest)
	}{
		{name: "wrong schema version", mutate: func(m *packmgr.Manifest) { m.SchemaVersion = "ailuo.install.v2" }},
		{name: "invalid id", mutate: func(m *packmgr.Manifest) { m.ID = "Campus.Bus" }},
		{name: "invalid version", mutate: func(m *packmgr.Manifest) { m.Version = "1.2" }},
		{name: "unsupported mode", mutate: func(m *packmgr.Manifest) { m.Mode = "embedded" }},
		{name: "excessive idle ttl", mutate: func(m *packmgr.Manifest) { m.IdleTTLMS = 30*24*3600*1000 + 1 }},
		{name: "duplicate host function", mutate: func(m *packmgr.Manifest) {
			m.HostFunctions = []packmgr.HostedFunctionDecl{
				{Module: "ailuo.x", Name: "one"}, {Module: "ailuo.x", Name: "one"},
			}
		}},
		{name: "wasi module reserved", mutate: func(m *packmgr.Manifest) {
			m.HostFunctions = []packmgr.HostedFunctionDecl{{Module: "wasi_snapshot_preview1", Name: "fd_write"}}
		}},
		{name: "invalid storage", mutate: func(m *packmgr.Manifest) {
			m.Storage = &packmgr.Storage{Namespace: "campus/bus", SchemaVersion: 0,
				Sensitivity: packmgr.SensitivityPublic, Retention: packmgr.RetentionPermanent}
		}},
		{name: "invalid dependency id", mutate: func(m *packmgr.Manifest) {
			m.Dependencies = []packmgr.Dependency{{ID: "Bus.query", Constraint: "^1.0.0"}}
		}},
		{name: "invalid dependency constraint", mutate: func(m *packmgr.Manifest) {
			m.Dependencies = []packmgr.Dependency{{ID: "bus.query", Constraint: "^1.0"}}
		}},
		{name: "invalid extensions json", mutate: func(m *packmgr.Manifest) { m.Extensions = json.RawMessage(`{`) }},
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

func TestValidateLockAndProcessSpec(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "artifact.wasm")
	workDir := filepath.Join(root, "work")
	lock := packmgr.Lock{
		SchemaVersion: packmgr.SchemaVersion, PackageID: "campus.bus",
		PackageVersion: "1.0.0", Mode: packmgr.ModeHosted,
		ManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ArtifactSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ArtifactPath:   artifactPath,
	}
	if err := packmgr.ValidateLock(lock); err != nil {
		t.Fatalf("ValidateLock: %v", err)
	}
	badLock := lock
	badLock.PackageVersion = "1.0"
	if err := packmgr.ValidateLock(badLock); err == nil {
		t.Fatal("ValidateLock accepted invalid version")
	}

	spec := packmgr.ProcessSpec{
		Path:    artifactPath,
		WorkDir: workDir,
		Address: "127.0.0.1:9000",
	}
	if err := packmgr.ValidateProcessSpec(spec); err != nil {
		t.Fatalf("ValidateProcessSpec: %v", err)
	}
	if err := packmgr.ValidateProcessSpec(packmgr.ProcessSpec{Address: "192.0.2.1:9000"}); err == nil {
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
