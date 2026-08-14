//go:build windows

package loader

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObjectEndOfJobTimeInformation 设置 Job 累计 CPU 时间耗尽时的动作
// （对应 Windows JobObjectEndOfJobTimeInformation 信息类）。
type jobObjectEndOfJobTimeInformation struct {
	EndOfJobTimeAction uint32
}

const (
	// jobObjectTerminateAtEndOfJobTime 是 Job 时间耗尽时的终止动作（0 = 终止）。
	jobObjectTerminateAtEndOfJobTime = 0
	// windowsSecondsToFiletime 是 1 秒对应的 FILETIME 单位（100ns）。
	windowsSecondsToFiletime = int64(10_000_000)
)

// applyProcessLimits 在 Windows 上用 Job Object 强制 isolated 包资源限额：
//   - MaxAddressBytes → JOB_OBJECT_LIMIT_PROCESS_MEMORY（进程内存上限，等效 RLIMIT_AS）；
//   - MaxCPUSeconds → JOB_OBJECT_LIMIT_JOB_TIME（Job 累计 CPU 时间，耗尽即终止，
//     等效 RLIMIT_CPU；isolated 每个运行一个子进程，Job 即该进程）；
//   - MaxOpenFiles / MaxFileBytes：Windows 无对应进程级原语，非零值 fail-closed
//     （Windows 无法强制这些资源，拒绝配置是唯一正确行为，不是降级路径）。
//
// 无论限额是否为零，都会创建 Job 并附加 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE：
// Job 句柄随子进程存续，句柄释放时强制终止子进程（等效 Unix 进程组清理，且在内核
// 崩溃后由操作系统兜底防孤儿）。返回的释放器必须在子进程回收后调用以释放句柄；
// 提前释放会立即终止子进程。分配或分配失败一律 fail-closed：未受 Job 约束的
// 进程不允许继续运行。
func applyProcessLimits(process *os.Process, limits ProcessLimits) (func() error, error) {
	if !ValidProcessLimits(limits) {
		return nil, ErrInvalidProcessSpec
	}
	if limits.MaxOpenFiles != 0 || limits.MaxFileBytes != 0 {
		return nil, fmt.Errorf("isolated runtime limits max_open_files/max_file_bytes are not supported on windows: %w", ErrInvalidProcessSpec)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	assigned := false
	defer func() {
		if !assigned {
			windows.CloseHandle(job)
		}
	}()

	basic := windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
		LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
	}
	if limits.MaxCPUSeconds > 0 {
		basic.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_TIME
		basic.PerJobUserTimeLimit = int64(limits.MaxCPUSeconds) * windowsSecondsToFiletime
	}
	extended := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation: basic}
	if limits.MaxAddressBytes > 0 {
		extended.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		extended.ProcessMemoryLimit = uintptr(limits.MaxAddressBytes)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&extended)), uint32(unsafe.Sizeof(extended))); err != nil {
		return nil, fmt.Errorf("set job object limits: %w", err)
	}
	if limits.MaxCPUSeconds > 0 {
		action := jobObjectEndOfJobTimeInformation{EndOfJobTimeAction: jobObjectTerminateAtEndOfJobTime}
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectEndOfJobTimeInformation,
			uintptr(unsafe.Pointer(&action)), uint32(unsafe.Sizeof(action))); err != nil {
			return nil, fmt.Errorf("set job end-of-time action: %w", err)
		}
	}
	processHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return nil, fmt.Errorf("open child process handle: %w", err)
	}
	defer windows.CloseHandle(processHandle)
	if err := windows.AssignProcessToJobObject(job, processHandle); err != nil {
		return nil, fmt.Errorf("assign child process to job object: %w", err)
	}
	assigned = true
	return func() error { return windows.CloseHandle(job) }, nil
}
