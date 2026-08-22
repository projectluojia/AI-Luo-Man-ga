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
	"github.com/projectluojia/AI-Luo-Man-ga/internal/packmgr"
)

var (
	ErrInvalidProcessSpec = errors.New("invalid isolated runtime process specification")
	ErrProcessCleanup     = errors.New("isolated runtime process cleanup failed")
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type IsolatedProcessHostConfig struct {
	ResolveInstalled func(context.Context, Manifest) (packmgr.ProcessSpec, error)
	VerifyInstalled  func(context.Context, Manifest, packmgr.ProcessSpec) error
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
	if !ValidProcessDuration(config.DialTimeout) || !ValidProcessDuration(config.StopGrace) ||
		!ValidProcessDuration(config.TerminateGrace) {
		return nil, ErrInvalidManifest
	}
	return &IsolatedProcessHost{
		config: config, runtimes: make(map[*processRuntime]struct{}),
	}, nil
}

// Mode 返回宿主服务的运行模式：isolated 本机进程。
func (h *IsolatedProcessHost) Mode() string { return ModeIsolated }

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
	process, err := StartProcess(ctx, spec, io.Discard, io.Discard)
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
	// 进程在加载完成前退出（启动失败）时取消加载，避免拨号/生命周期调用永不返回。
	watchContext, stopWatch := ProcessWatchContext(ctx, process)
	loaded, err := grpcHost.Load(watchContext, manifest)
	stopWatch()
	if err != nil {
		if process.Exited() {
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

// ProcessWatchContext 派生一个在受监督进程退出时自动取消的上下文，供 Spawn
// 模式加载使用：进程在拨号/加载完成前退出（启动即失败）时立即失败，避免
// 连接或加载永不返回。stop 停止监控并释放派生上下文（幂等），调用方须在
// 完成后调用。process 为 nil（连接模式，不拥有进程）时等价于普通派生上下文。
func ProcessWatchContext(ctx context.Context, process *Process) (derived context.Context, stop func()) {
	derived, cancel := context.WithCancel(ctx)
	if process == nil {
		return derived, cancel
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-process.Done():
			cancel()
		case <-done:
		}
	}()
	var stopOnce sync.Once
	return derived, func() {
		stopOnce.Do(func() {
			close(done)
			cancel()
		})
	}
}

// Process 是受监督子进程：启动时应用资源限额，退出经 done channel 通知，
// 清理（优雅终止 → 强制终止）与限额释放封装为原语，供 isolated 形态的
// 内置 Agent 与 installed 扩展 Runtime 共用。
type Process struct {
	command *exec.Cmd
	done    chan struct{}
	// waitErr 是 command.Wait 的返回；在 close(done) 前写入，channel 关闭提供 happens-before。
	waitErr error
	// release 是平台资源限额释放器（Windows Job Object 句柄）；其余平台为 nil。
	// 必须在子进程回收后调用，提前释放会立即终止子进程（KILL_ON_JOB_CLOSE）。
	release func() error
}

// StartProcess 启动受监督子进程并应用资源限额。stdout/stderr 决定子进程
// 输出去向：installed 扩展默认丢弃，内置 agent 直接透传内核输出。
func StartProcess(ctx context.Context, spec packmgr.ProcessSpec, stdout, stderr io.Writer) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(spec.Path, spec.Args...)
	command.Env = append([]string(nil), spec.Env...)
	command.Dir = spec.WorkDir
	command.Stdin = nil
	command.Stdout = stdout
	command.Stderr = stderr
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
	process := &Process{command: command, done: make(chan struct{}), release: release}
	go func() {
		process.waitErr = command.Wait()
		close(process.done)
	}()
	return process, nil
}

// Done 返回子进程退出通知 channel（进程退出时关闭；连接模式下为 nil）。
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Err 返回 command.Wait 的错误；仅子进程退出后有意义。
func (p *Process) Err() error {
	return p.waitErr
}

// Exited 报告子进程是否已经退出。
func (p *Process) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Wait 在宽限期内等待子进程退出；已退出立即返回 true。
func (p *Process) Wait(ctx context.Context, grace time.Duration) bool {
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

// Terminate 优雅终止子进程（Unix 进程组 SIGTERM；其余平台发送中断信号）。
func (p *Process) Terminate() error {
	return terminateCommandProcess(p.command.Process)
}

// Kill 强制终止子进程（Unix 进程组 SIGKILL；其余平台直接 Kill）。
func (p *Process) Kill() error {
	return killCommandProcess(p.command.Process)
}

// Reap 回收受监督子进程：先优雅终止并在 stopGrace 内等待退出，未退出则强制
// 终止并在 terminateGrace 内等待。进程已退出或回收成功后释放平台资源限额；
// 限额句柄绝不在子进程存活期间释放（Windows KILL_ON_JOB_CLOSE 会立即终止）。
// 清理等待从调用方 context 解耦（不因调用方取消而中止），总上限为
// stopGrace+terminateGrace+1s。
func (p *Process) Reap(ctx context.Context, stopGrace, terminateGrace time.Duration) error {
	if p.Exited() {
		p.Release()
		return nil
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopGrace+terminateGrace+time.Second)
	defer cancel()
	if err := p.Terminate(); err == nil && p.Wait(cleanupContext, stopGrace) {
		p.Release()
		return nil
	}
	if err := p.Kill(); err != nil && !p.Exited() {
		return ErrProcessCleanup
	}
	if !p.Wait(cleanupContext, terminateGrace) {
		return ErrProcessCleanup
	}
	p.Release()
	return nil
}

// Release 释放平台资源限额句柄；仅在子进程已回收后调用，重复调用安全。
func (p *Process) Release() {
	if p.release != nil {
		_ = p.release()
		p.release = nil
	}
}

type processRuntime struct {
	runtime        *grpcRuntime
	process        *Process
	host           *IsolatedProcessHost
	socketPath     string
	stopGrace      time.Duration
	terminateGrace time.Duration

	mu      sync.Mutex
	stopped bool
}

func (r *processRuntime) Describe(ctx context.Context) (Description, error) {
	if r.process.Exited() {
		return Description{}, ErrUnavailable
	}
	return r.runtime.Describe(ctx)
}

func (r *processRuntime) Start(ctx context.Context) error {
	if r.process.Exited() {
		return ErrUnavailable
	}
	return r.runtime.Start(ctx)
}

func (r *processRuntime) Health(ctx context.Context) error {
	if r.process.Exited() {
		return ErrUnavailable
	}
	return r.runtime.Health(ctx)
}

func (r *processRuntime) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	if r.process.Exited() {
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
	if r.process.Exited() {
		return r.finish()
	}
	stopErr := r.runtime.Stop(ctx)
	if stopErr == nil && r.process.Wait(ctx, r.stopGrace) {
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
	if r.process.Exited() {
		return r.completeProcessCleanup(cleanupFailed)
	}
	if err := r.process.Terminate(); err != nil && !r.process.Exited() {
		return ErrProcessCleanup
	}
	if r.process.Wait(ctx, r.terminateGrace) {
		return r.completeProcessCleanup(cleanupFailed)
	}
	if err := r.process.Kill(); err != nil && !r.process.Exited() {
		return ErrProcessCleanup
	}
	if !r.process.Wait(ctx, r.stopGrace) {
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
	r.process.Release()
}

func validateProcessSpec(spec packmgr.ProcessSpec) error {
	// 形状校验（绝对路径/本地地址/上限闭式）由中立格式层负责；此处只做
	// 装载时刻的文件系统与内容安全校验。
	if err := packmgr.ValidateProcessSpec(spec); err != nil {
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

// ValidProcessDuration 校验进程生命周期宽限期的合理范围。
func ValidProcessDuration(value time.Duration) bool {
	return value >= 100*time.Millisecond && value <= time.Minute
}
