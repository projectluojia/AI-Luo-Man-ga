//go:build integration && windows

package loader

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
)

// TestWindowsCPUBurnHelper 是 CPU 燃烧子进程入口：AILUO_CPU_BURN=1 时无限旋转。
func TestWindowsCPUBurnHelper(t *testing.T) {
	if os.Getenv("AILUO_CPU_BURN") != "1" {
		return
	}
	for {
		_ = time.Now().UnixNano()
	}
}

// TestApplyProcessLimitsTerminatesCPUOverrun 验证 Windows Job Object 的累计 CPU
// 时间限额（JOB_OBJECT_LIMIT_JOB_TIME）：子进程燃烧 CPU 超过 1 秒后被操作系统
// 强制终止。这是 Windows 上 RLIMIT_CPU 的等效强制，不是协作式。Job 句柄必须
// 存活到进程回收（KILL_ON_JOB_CLOSE），因此测试在进程退出后才释放。
func TestApplyProcessLimitsTerminatesCPUOverrun(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestWindowsCPUBurnHelper$")
	command.Env = append(os.Environ(), "AILUO_CPU_BURN=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	release, err := applyProcessLimits(command.Process, packagecontract.ProcessLimits{MaxCPUSeconds: 1})
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("applyProcessLimits: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Job 累计 CPU 时间耗尽，操作系统已终止进程；进程回收后才可释放句柄。
		_ = release()
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = release()
		t.Fatal("CPU 超限进程未被 Job Object 终止")
	}
}

// TestApplyProcessLimitsRejectsUnmappableLimits 验证 Windows 对无对应进程级原语的
// 限额 fail-closed（max_open_files / max_file_bytes）：Windows 无法强制这些资源，
// 拒绝配置是唯一正确行为。
func TestApplyProcessLimitsRejectsUnmappableLimits(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spawn := func() *exec.Cmd {
		command := exec.Command(executable, "-test.run=^TestWindowsCPUBurnHelper$")
		command.Env = append(os.Environ(), "AILUO_CPU_BURN=1")
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	for _, limits := range []packagecontract.ProcessLimits{
		{MaxOpenFiles: 100},
		{MaxFileBytes: 1024},
	} {
		command := spawn()
		release, err := applyProcessLimits(command.Process, limits)
		if err == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			if release != nil {
				_ = release()
			}
			t.Fatalf("limits=%#v must fail closed on windows", limits)
		}
		_ = command.Process.Kill()
		_ = command.Wait()
	}
}
