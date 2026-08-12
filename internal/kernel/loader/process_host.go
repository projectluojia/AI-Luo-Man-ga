package loader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

var (
	ErrInvalidProcessSpec = errors.New("invalid isolated runtime process specification")
	ErrProcessCleanup     = errors.New("isolated runtime process cleanup failed")
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ProcessLimits 是 isolated 包的 OS 资源限额。Unix 平台在子进程启动后立即强制执行；
// 非 Linux Unix 与 Windows 平台对携带非零限额的包 fail-closed（0 表示不限制）。
type ProcessLimits struct {
	// MaxAddressBytes 是虚拟地址空间上限（RLIMIT_AS）。
	MaxAddressBytes uint64 `json:"max_address_bytes,omitempty"`
	// MaxCPUSeconds 是 CPU 时间上限（RLIMIT_CPU）。
	MaxCPUSeconds uint64 `json:"max_cpu_seconds,omitempty"`
	// MaxOpenFiles 是最大打开文件数（RLIMIT_NOFILE）。
	MaxOpenFiles uint64 `json:"max_open_files,omitempty"`
	// MaxFileBytes 是单个文件最大字节（RLIMIT_FSIZE）。
	MaxFileBytes uint64 `json:"max_file_bytes,omitempty"`
}

type ProcessSpec struct {
	Path    string
	Args    []string
	Env     []string
	WorkDir string
	Address string
	Limits  ProcessLimits
}

type IsolatedProcessHostConfig struct {
	ResolveInstalled func(context.Context, Manifest) (ProcessSpec, error)
	VerifyInstalled  func(context.Context, Manifest, ProcessSpec) error
	DialTimeout      time.Duration
	StopGrace        time.Duration
	TerminateGrace   time.Duration
}

// IsolatedProcessHost 只启动安装解析器返回且再次通过锁校验的本机进程。
type IsolatedProcessHost struct {
	config IsolatedProcessHostConfig

	mu       sync.Mutex
	runtimes map[*processRuntime]struct{}
	closed   bool
}

func NewIsolatedProcessHost(config IsolatedProcessHostConfig) (*IsolatedProcessHost, error) {
	if config.ResolveInstalled == nil || config.VerifyInstalled == nil {
		return nil, ErrInvalidManifest
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.StopGrace == 0 {
		config.StopGrace = 5 * time.Second
	}
	if config.TerminateGrace == 0 {
		config.TerminateGrace = 2 * time.Second
	}
	if !validProcessDuration(config.DialTimeout) || !validProcessDuration(config.StopGrace) ||
		!validProcessDuration(config.TerminateGrace) {
		return nil, ErrInvalidManifest
	}
	return &IsolatedProcessHost{
		config: config, runtimes: make(map[*processRuntime]struct{}),
	}, nil
}

func (h *IsolatedProcessHost) Verify(ctx context.Context, manifest Manifest) error {
	if manifest.Mode != ModeIsolated {
		return ErrUnsupportedMode
	}
	spec, err := h.config.ResolveInstalled(ctx, manifest)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if err := validateProcessSpec(spec); err != nil {
		return err
	}
	return h.config.VerifyInstalled(ctx, manifest, spec)
}

func (h *IsolatedProcessHost) Load(ctx context.Context, manifest Manifest) (Runtime, error) {
	if manifest.Mode != ModeIsolated {
		return nil, ErrUnsupportedMode
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return nil, ErrShuttingDown
	}

	// Load 再次解析并校验，避免 Verify 与真正执行之间替换安装单元。
	spec, err := h.config.ResolveInstalled(ctx, manifest)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := validateProcessSpec(spec); err != nil {
		return nil, err
	}
	if err := h.config.VerifyInstalled(ctx, manifest, spec); err != nil {
		return nil, err
	}
	process, err := startCommandProcess(ctx, spec)
	if err != nil {
		return nil, ErrUnavailable
	}
	wrapped := &processRuntime{
		process: process, host: h, socketPath: strings.TrimPrefix(spec.Address, "unix:"),
		stopGrace: h.config.StopGrace, terminateGrace: h.config.TerminateGrace,
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		cleanupErr := wrapped.cleanupAfterLoad(ctx)
		return nil, errors.Join(ErrShuttingDown, cleanupErr)
	}
	h.runtimes[wrapped] = struct{}{}
	h.mu.Unlock()

	grpcHost, err := NewGRPCHost(GRPCHostConfig{
		Mode: ModeIsolated, Address: spec.Address, DialTimeout: h.config.DialTimeout,
		VerifyInstalled: func(context.Context, Manifest) error { return nil },
	})
	if err != nil {
		cleanupErr := wrapped.cleanupAfterLoad(ctx)
		h.remove(wrapped)
		return nil, errors.Join(err, cleanupErr)
	}
	dialContext, cancelDial := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		select {
		case <-process.done:
			cancelDial()
		case <-monitorDone:
		}
	}()
	loaded, err := grpcHost.Load(dialContext, manifest)
	close(monitorDone)
	cancelDial()
	if err != nil {
		if process.exited() {
			err = ErrUnavailable
		}
		cleanupErr := wrapped.cleanupAfterLoad(ctx)
		h.remove(wrapped)
		return nil, errors.Join(err, cleanupErr)
	}
	runtime, ok := loaded.(*grpcRuntime)
	if !ok {
		cleanupErr := wrapped.cleanupAfterLoad(ctx)
		h.remove(wrapped)
		return nil, errors.Join(ErrUnavailable, cleanupErr)
	}
	wrapped.runtime = runtime
	return wrapped, nil
}

func (h *IsolatedProcessHost) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	runtimes := make([]*processRuntime, 0, len(h.runtimes))
	for runtime := range h.runtimes {
		runtimes = append(runtimes, runtime)
	}
	h.mu.Unlock()
	var result []error
	for _, runtime := range runtimes {
		if err := runtime.forceCleanup(ctx); err != nil {
			result = append(result, err)
		} else {
			h.remove(runtime)
		}
	}
	return errors.Join(result...)
}

func (h *IsolatedProcessHost) remove(runtime *processRuntime) {
	h.mu.Lock()
	delete(h.runtimes, runtime)
	h.mu.Unlock()
}

type commandProcess struct {
	command *exec.Cmd
	done    chan struct{}
	// release 是平台资源限额释放器（Windows Job Object 句柄）；其余平台为 nil。
	// 必须在子进程回收后调用，提前释放会立即终止子进程（KILL_ON_JOB_CLOSE）。
	release func() error
}

func startCommandProcess(ctx context.Context, spec ProcessSpec) (*commandProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(spec.Path, spec.Args...)
	command.Env = append([]string(nil), spec.Env...)
	command.Dir = spec.WorkDir
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	// 启动后立即应用资源限额：Linux 用 prlimit，Windows 用 Job Object，
	// 其余平台对非零限额 fail-closed。
	release, err := applyProcessLimits(command.Process, spec.Limits)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	process := &commandProcess{command: command, done: make(chan struct{}), release: release}
	go func() {
		_ = command.Wait()
		close(process.done)
	}()
	return process, nil
}

func (p *commandProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *commandProcess) wait(ctx context.Context, grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

type processRuntime struct {
	runtime        *grpcRuntime
	process        *commandProcess
	host           *IsolatedProcessHost
	socketPath     string
	stopGrace      time.Duration
	terminateGrace time.Duration

	mu      sync.Mutex
	stopped bool
}

func (r *processRuntime) Describe(ctx context.Context) (Description, error) {
	if r.process.exited() {
		return Description{}, ErrUnavailable
	}
	return r.runtime.Describe(ctx)
}

func (r *processRuntime) Start(ctx context.Context) error {
	if r.process.exited() {
		return ErrUnavailable
	}
	return r.runtime.Start(ctx)
}

func (r *processRuntime) Health(ctx context.Context) error {
	if r.process.exited() {
		return ErrUnavailable
	}
	return r.runtime.Health(ctx)
}

func (r *processRuntime) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	if r.process.exited() {
		return nil, ErrUnavailable
	}
	return r.runtime.Invoke(ctx, request, payload)
}

func (r *processRuntime) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	if r.process.exited() {
		return r.finish()
	}
	stopErr := r.runtime.Stop(ctx)
	if stopErr == nil && r.process.wait(ctx, r.stopGrace) {
		return r.finish()
	}
	cleanupContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), r.stopGrace+r.terminateGrace+time.Second,
	)
	defer cancel()
	if err := r.forceCleanupLocked(cleanupContext); err != nil {
		return errors.Join(ErrProcessCleanup, err)
	}
	observe.Warn(ctx, "隔离运行时未在宽限期内退出，已完成强制清理")
	return r.finish()
}

func (r *processRuntime) forceCleanup(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	if err := r.forceCleanupLocked(ctx); err != nil {
		return err
	}
	return r.finish()
}

func (r *processRuntime) forceCleanupLocked(ctx context.Context) error {
	var cleanupFailed bool
	if r.runtime != nil {
		if err := r.runtime.closeTransport(); err != nil {
			cleanupFailed = true
		}
	}
	if r.process.exited() {
		return r.completeProcessCleanup(cleanupFailed)
	}
	if err := terminateCommandProcess(r.process.command.Process); err != nil && !r.process.exited() {
		return ErrProcessCleanup
	}
	if r.process.wait(ctx, r.terminateGrace) {
		return r.completeProcessCleanup(cleanupFailed)
	}
	if err := killCommandProcess(r.process.command.Process); err != nil && !r.process.exited() {
		return ErrProcessCleanup
	}
	if !r.process.wait(ctx, r.stopGrace) {
		return ErrProcessCleanup
	}
	return r.completeProcessCleanup(cleanupFailed)
}

func (r *processRuntime) completeProcessCleanup(cleanupFailed bool) error {
	if err := removeRuntimeSocket(r.socketPath); err != nil {
		cleanupFailed = true
	}
	if cleanupFailed {
		return ErrProcessCleanup
	}
	return nil
}

func (r *processRuntime) cleanupAfterLoad(ctx context.Context) error {
	cleanupContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), r.stopGrace+r.terminateGrace+time.Second,
	)
	defer cancel()
	return r.forceCleanup(cleanupContext)
}

func (r *processRuntime) finish() error {
	if r.runtime != nil {
		if err := r.runtime.closeTransport(); err != nil {
			r.stopped = true
			r.host.remove(r)
			r.releaseProcessLimits()
			return ErrProcessCleanup
		}
	}
	if err := removeRuntimeSocket(r.socketPath); err != nil {
		r.stopped = true
		r.host.remove(r)
		r.releaseProcessLimits()
		return ErrProcessCleanup
	}
	r.stopped = true
	r.host.remove(r)
	r.releaseProcessLimits()
	return nil
}

// releaseProcessLimits 释放平台资源限额句柄（Windows Job Object）。进程已回收，
// 释放句柄不会误杀子进程；提前释放会触发 KILL_ON_JOB_CLOSE 立即终止。
func (r *processRuntime) releaseProcessLimits() {
	if r.process.release != nil {
		_ = r.process.release()
		r.process.release = nil
	}
}

func validateProcessSpec(spec ProcessSpec) error {
	if !filepath.IsAbs(spec.Path) || filepath.Clean(spec.Path) != spec.Path ||
		!filepath.IsAbs(spec.WorkDir) || filepath.Clean(spec.WorkDir) != spec.WorkDir ||
		!IsLocalRuntimeAddress(spec.Address) || len(spec.Args) > 128 || len(spec.Env) > 64 ||
		!validProcessLimits(spec.Limits) {
		return ErrInvalidProcessSpec
	}
	info, err := os.Lstat(spec.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return ErrInvalidProcessSpec
	}
	if info, err = os.Lstat(spec.WorkDir); err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return ErrInvalidProcessSpec
	}
	socketPath := strings.TrimPrefix(spec.Address, "unix:")
	relativeSocket, err := filepath.Rel(spec.WorkDir, socketPath)
	if err != nil || relativeSocket == "." || relativeSocket == ".." ||
		strings.HasPrefix(relativeSocket, ".."+string(filepath.Separator)) {
		return ErrInvalidProcessSpec
	}
	if _, err := os.Lstat(socketPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return ErrInvalidProcessSpec
	}
	for _, argument := range spec.Args {
		if len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return ErrInvalidProcessSpec
		}
	}
	seenEnvironment := make(map[string]struct{}, len(spec.Env))
	for _, item := range spec.Env {
		name, value, found := strings.Cut(item, "=")
		if !found || !environmentNamePattern.MatchString(name) || len(value) > 4096 ||
			strings.ContainsRune(value, '\x00') || forbiddenProcessEnvironment(name) {
			return ErrInvalidProcessSpec
		}
		if _, exists := seenEnvironment[name]; exists {
			return ErrInvalidProcessSpec
		}
		seenEnvironment[name] = struct{}{}
	}
	return nil
}

func removeRuntimeSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return ErrProcessCleanup
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrProcessCleanup
	}
	return nil
}

func forbiddenProcessEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	if upper == "LD_PRELOAD" || upper == "LD_LIBRARY_PATH" || strings.HasPrefix(upper, "DYLD_") ||
		upper == "PYTHONPATH" {
		return true
	}
	for _, protected := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "COOKIE", "AUTH"} {
		if strings.Contains(upper, protected) {
			return true
		}
	}
	return false
}

func validProcessDuration(value time.Duration) bool {
	return value >= 100*time.Millisecond && value <= time.Minute
}

// maxProcessLimit* 是资源限额的合理性上限，防止异常配置写入系统限制。
const (
	maxProcessLimitAddress = uint64(1 << 40) // 1 TiB
	maxProcessLimitCPU     = uint64(1 << 31)
	maxProcessLimitFiles   = uint64(1 << 20)
	maxProcessLimitFile    = uint64(1 << 40)
)

// validProcessLimits 校验限额在合理性上限内。
func validProcessLimits(limits ProcessLimits) bool {
	return limits.MaxAddressBytes <= maxProcessLimitAddress &&
		limits.MaxCPUSeconds <= maxProcessLimitCPU &&
		limits.MaxOpenFiles <= maxProcessLimitFiles &&
		limits.MaxFileBytes <= maxProcessLimitFile
}
