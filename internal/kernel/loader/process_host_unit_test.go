package loader

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

func TestProcessWatchContextCancelsWhenProcessExits(t *testing.T) {
	process := &Process{done: make(chan struct{})}
	derived, stop := ProcessWatchContext(context.Background(), process)
	defer stop()
	if err := derived.Err(); err != nil {
		t.Fatalf("derived context 初始已取消: %v", err)
	}
	close(process.done)
	select {
	case <-derived.Done():
	case <-time.After(time.Second):
		t.Fatal("进程退出未取消派生上下文")
	}
}

func TestProcessWatchContextStopReleasesAndIsIdempotent(t *testing.T) {
	process := &Process{done: make(chan struct{})}
	derived, stop := ProcessWatchContext(context.Background(), process)
	stop()
	stop() // 幂等：重复调用不得 panic
	if err := derived.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop 后错误=%v, want context.Canceled", err)
	}
}

func TestProcessWatchContextNilProcessIsPlainContext(t *testing.T) {
	derived, stop := ProcessWatchContext(context.Background(), nil)
	stop()
	if err := derived.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop 后错误=%v, want context.Canceled", err)
	}
}

func TestValidateProcessSpecRejectsUnsafeExecutionInputs(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	base := packmgr.ProcessSpec{
		Path: executable, WorkDir: workDir, Address: "unix:" + filepath.Join(workDir, "runtime.sock"),
	}
	tests := []packmgr.ProcessSpec{
		func() packmgr.ProcessSpec { value := base; value.Path = "relative"; return value }(),
		func() packmgr.ProcessSpec { value := base; value.WorkDir = "relative"; return value }(),
		func() packmgr.ProcessSpec { value := base; value.Address = "192.0.2.1:9000"; return value }(),
		func() packmgr.ProcessSpec { value := base; value.Env = []string{"API_TOKEN=private"}; return value }(),
		func() packmgr.ProcessSpec {
			value := base
			value.Env = []string{"LD_PRELOAD=/tmp/inject.so"}
			return value
		}(),
		func() packmgr.ProcessSpec { value := base; value.Env = []string{"SAFE=1", "SAFE=2"}; return value }(),
		func() packmgr.ProcessSpec { value := base; value.Args = []string{"bad\x00argument"}; return value }(),
		func() packmgr.ProcessSpec {
			value := base
			value.Limits = packmgr.ProcessLimits{MaxAddressBytes: 1 << 50}
			return value
		}(),
		func() packmgr.ProcessSpec {
			value := base
			value.Limits = packmgr.ProcessLimits{MaxCPUSeconds: 1 << 40}
			return value
		}(),
		func() packmgr.ProcessSpec {
			value := base
			value.Limits = packmgr.ProcessLimits{MaxOpenFiles: 1 << 30}
			return value
		}(),
		func() packmgr.ProcessSpec {
			value := base
			value.Limits = packmgr.ProcessLimits{MaxFileBytes: 1 << 50}
			return value
		}(),
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
		limits packmgr.ProcessLimits
		want   bool
	}{
		{name: "zero means unlimited", limits: packmgr.ProcessLimits{}, want: true},
		{name: "reasonable address", limits: packmgr.ProcessLimits{MaxAddressBytes: 1 << 30}, want: true},
		{name: "excessive address", limits: packmgr.ProcessLimits{MaxAddressBytes: 1 << 50}, want: false},
		{name: "reasonable cpu", limits: packmgr.ProcessLimits{MaxCPUSeconds: 3600}, want: true},
		{name: "excessive cpu", limits: packmgr.ProcessLimits{MaxCPUSeconds: 1 << 40}, want: false},
		{name: "reasonable files", limits: packmgr.ProcessLimits{MaxOpenFiles: 1024}, want: true},
		{name: "excessive files", limits: packmgr.ProcessLimits{MaxOpenFiles: 1 << 30}, want: false},
		{name: "reasonable file size", limits: packmgr.ProcessLimits{MaxFileBytes: 1 << 20}, want: true},
		{name: "excessive file size", limits: packmgr.ProcessLimits{MaxFileBytes: 1 << 50}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := packmgr.ValidProcessLimits(tc.limits); got != tc.want {
				t.Fatalf("packmgr.ValidProcessLimits(%+v) = %v, want %v", tc.limits, got, tc.want)
			}
		})
	}
}

func TestIsolatedProcessHostValidatesConfigurationAndMode(t *testing.T) {
	t.Parallel()
	resolve := func(context.Context, Manifest) (packmgr.ProcessSpec, error) { return packmgr.ProcessSpec{}, nil }
	verify := func(context.Context, Manifest, packmgr.ProcessSpec) error { return nil }
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

// TestProcessReapStopsStubbornChild 验证 Reap 原语：先优雅终止（辅助进程忽略
// 中断信号），宽限未退出则强制终止，最终进程已回收且限额已释放。
func TestProcessReapStopsStubbornChild(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process, err := StartProcess(context.Background(), packmgr.ProcessSpec{
		Path:    executable,
		Args:    []string{"-test.run=TestReapHelperChild"},
		Env:     append(os.Environ(), "AILUO_REAP_HELPER=1"),
		WorkDir: t.TempDir(),
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
		t.Fatal("helper exited before Reap")
	case <-time.After(300 * time.Millisecond):
	}
	if err := process.Reap(context.Background(), 300*time.Millisecond, 3*time.Second); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !process.Exited() {
		t.Fatal("child still alive after Reap")
	}
	// Reap 已释放限额：重复释放安全。
	process.Release()
}

// TestReapHelperChild 是 Reap 测试的辅助子进程：忽略中断信号并长时间驻留，
// 供父进程验证优雅终止失败后的强制终止路径。
func TestReapHelperChild(t *testing.T) {
	if os.Getenv("AILUO_REAP_HELPER") != "1" {
		t.Skip("helper process")
	}
	signal.Ignore(os.Interrupt)
	time.Sleep(time.Hour)
}
