package loader

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateProcessSpecRejectsUnsafeExecutionInputs(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	base := ProcessSpec{
		Path: executable, WorkDir: workDir, Address: "unix:" + filepath.Join(workDir, "runtime.sock"),
	}
	tests := []ProcessSpec{
		func() ProcessSpec { value := base; value.Path = "relative"; return value }(),
		func() ProcessSpec { value := base; value.WorkDir = "relative"; return value }(),
		func() ProcessSpec { value := base; value.Address = "192.0.2.1:9000"; return value }(),
		func() ProcessSpec { value := base; value.Env = []string{"API_TOKEN=private"}; return value }(),
		func() ProcessSpec { value := base; value.Env = []string{"LD_PRELOAD=/tmp/inject.so"}; return value }(),
		func() ProcessSpec { value := base; value.Env = []string{"SAFE=1", "SAFE=2"}; return value }(),
		func() ProcessSpec { value := base; value.Args = []string{"bad\x00argument"}; return value }(),
		func() ProcessSpec {
			value := base
			value.Limits = ProcessLimits{MaxAddressBytes: 1 << 50}
			return value
		}(),
		func() ProcessSpec { value := base; value.Limits = ProcessLimits{MaxCPUSeconds: 1 << 40}; return value }(),
		func() ProcessSpec { value := base; value.Limits = ProcessLimits{MaxOpenFiles: 1 << 30}; return value }(),
		func() ProcessSpec { value := base; value.Limits = ProcessLimits{MaxFileBytes: 1 << 50}; return value }(),
	}
	for _, spec := range tests {
		if err := validateProcessSpec(spec); err == nil {
			t.Fatalf("unsafe spec accepted: %#v", spec)
		}
	}
}

func TestValidProcessLimits(t *testing.T) {
	cases := []struct {
		name   string
		limits ProcessLimits
		want   bool
	}{
		{name: "zero means unlimited", limits: ProcessLimits{}, want: true},
		{name: "reasonable address", limits: ProcessLimits{MaxAddressBytes: 1 << 30}, want: true},
		{name: "excessive address", limits: ProcessLimits{MaxAddressBytes: 1 << 50}, want: false},
		{name: "reasonable cpu", limits: ProcessLimits{MaxCPUSeconds: 3600}, want: true},
		{name: "excessive cpu", limits: ProcessLimits{MaxCPUSeconds: 1 << 40}, want: false},
		{name: "reasonable files", limits: ProcessLimits{MaxOpenFiles: 1024}, want: true},
		{name: "excessive files", limits: ProcessLimits{MaxOpenFiles: 1 << 30}, want: false},
		{name: "reasonable file size", limits: ProcessLimits{MaxFileBytes: 1 << 20}, want: true},
		{name: "excessive file size", limits: ProcessLimits{MaxFileBytes: 1 << 50}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validProcessLimits(tc.limits); got != tc.want {
				t.Fatalf("validProcessLimits(%+v) = %v, want %v", tc.limits, got, tc.want)
			}
		})
	}
}

func TestIsolatedProcessHostValidatesConfigurationAndMode(t *testing.T) {
	t.Parallel()
	resolve := func(context.Context, Manifest) (ProcessSpec, error) { return ProcessSpec{}, nil }
	verify := func(context.Context, Manifest, ProcessSpec) error { return nil }
	for _, config := range []IsolatedProcessHostConfig{
		{},
		{ResolveInstalled: resolve},
		{VerifyInstalled: verify},
		{ResolveInstalled: resolve, VerifyInstalled: verify, StopGrace: time.Millisecond},
	} {
		if _, err := NewIsolatedProcessHost(config); err == nil {
			t.Fatalf("invalid config accepted: %#v", config)
		}
	}
	host, err := NewIsolatedProcessHost(IsolatedProcessHostConfig{
		ResolveInstalled: resolve, VerifyInstalled: verify,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Verify(context.Background(), Manifest{Mode: ModeHosted}); err != ErrUnsupportedMode {
		t.Fatalf("mode error=%v", err)
	}
}
