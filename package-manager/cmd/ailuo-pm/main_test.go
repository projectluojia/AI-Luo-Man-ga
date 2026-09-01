package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
)

func TestResolveSourceRequiresExplicitManifest(t *testing.T) {
	_, _, err := resolveSource(t.Context(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "ailuo.toml") {
		t.Fatalf("resolveSource error=%v, want explicit ailuo.toml requirement", err)
	}
}

func TestResolveSDKSourceRequiresExplicitManifest(t *testing.T) {
	_, _, err := resolveSDKSource(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "ailuo.toml") {
		t.Fatalf("resolveSDKSource error=%v, want explicit ailuo.toml requirement", err)
	}
}

func TestInspectSourceReportsDeclaredBuildTools(t *testing.T) {
	directory := t.TempDir()
	manifest := `[package]
id = "demo.package"
version = "1.0.0"

[[component]]
id = "executor"
mode = "isolated"
role = "executor"
entrypoint = "executor"

[component.process]
path = "python"
address = "127.0.0.1:50051"

[component.build]
tool = "python-uv"
source = "runtime"

[[component]]
id = "bus"
mode = "hosted"
entrypoint = "bus.wasm"

[component.build]
tool = "go-wasm"
source = "guest"
`
	if err := os.WriteFile(filepath.Join(directory, packagefmt.SourceFileName), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	metadata, err := inspectSource(directory)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ID != "demo.package" || metadata.Version != "1.0.0" ||
		!slices.Equal(metadata.BuildTools, []string{"go-wasm", "python-uv"}) {
		t.Fatalf("metadata=%+v, want sorted declared build tools", metadata)
	}
}

func TestPreparePackageSourcePacksAuthorManifest(t *testing.T) {
	directory := t.TempDir()
	manifest := `[package]
id = "demo.package"
version = "1.0.0"

[[component]]
id = "core"
mode = "hosted"
entrypoint = "core.wasm"
`
	if err := os.WriteFile(filepath.Join(directory, packagefmt.SourceFileName), []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "core.wasm"), []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive, cleanup, err := preparePackageSource(t.Context(), directory)
	if err != nil {
		t.Fatalf("preparePackageSource: %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("prepared archive missing: %v", err)
	}
	cleanup()
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("prepared archive was not cleaned up: %v", err)
	}
}
