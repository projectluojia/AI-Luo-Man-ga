package processhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	// ErrInvalidProcessSpec 表示 isolated 进程规格非法。
	ErrInvalidProcessSpec = errors.New("invalid isolated runtime process specification")
	// ErrProcessCleanup 表示 isolated 进程清理失败。
	ErrProcessCleanup = errors.New("isolated runtime process cleanup failed")
)

// ProcessHostConfig 是统一进程宿主的配置：服务 mode=isolated 的全部组件。
// 进程监督（启动、存活、优雅/强制回收、意外退出上报）与传输（runtime_host /
// executor.v1 协议连接）在装载时组合：监督策略与清理序列由 runtimeShared
// 统一持有，role 只决定传输面。内核不感知任何具体运行时的名字：进程规格由
// 组合根聚合的包源按清单解析。
type ProcessHostConfig struct {
	// Resolve 按清单返回锁定的进程规格；解析失败的清单即该宿主不服务的
	// 清单（Verify fail-closed）。
	Resolve func(context.Context, loader.Manifest) (packagecontract.ProcessSpec, error)
	// Verify 对解析出的规格做来源侧二次校验（例如安装锁），可空。
	Verify func(context.Context, loader.Manifest, packagecontract.ProcessSpec) error
	// Spawn 是没有 SpawnFor 时的默认进程管理策略。
	Spawn bool
	// SpawnFor 按清单决定是否由本宿主启动进程；未设置时使用 Spawn。返回
	// false 即连接模式：只拨号 spec.Address（本地回环），进程不由本宿主管理。
	SpawnFor func(loader.Manifest) bool
	// Stdout/Stderr 决定子进程输出去向；nil 默认丢弃。
	Stdout io.Writer
	Stderr io.Writer
	// OnRuntimeExit 在受监督进程于停止流程之外退出时被调用（恰一次，连接
	// 模式不通知）。反应策略由组合根决定（如执行者退出时停止内核）；回调
	// 在监督锁外异步触发，不得同步等待运行时操作。
	OnRuntimeExit func(manifest loader.Manifest, processErr error)

	DialTimeout    time.Duration
	StopGrace      time.Duration
	TerminateGrace time.Duration
}

// ProcessHost 是统一进程宿主：按清单解析进程规格并统一监督（两种 role 与
// 连接/启动两种装配共用同一套清理序列与意外退出上报），role 决定传输面
// （runtime_host / executor.v1 协议）。监督与传输在运行时内分离：
// runtimeShared 持有进程与回收序列，providerRuntime / executorRuntime 只做
// 协议委派。
type ProcessHost struct {
	config ProcessHostConfig

	mu       sync.Mutex
	runtimes map[*runtimeShared]struct{}
	closed   bool
}

func NewProcessHost(config ProcessHostConfig) (*ProcessHost, error) {
	if config.Resolve == nil {
		return nil, loader.ErrInvalidManifest
	}
	// Spawn=false 时进入连接模式：进程由部署方管理，本宿主只拨号本地地址。
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
		return nil, loader.ErrInvalidManifest
	}
	if config.Stdout == nil {
		config.Stdout = io.Discard
	}
	if config.Stderr == nil {
		config.Stderr = io.Discard
	}
	return &ProcessHost{
		config: config, runtimes: make(map[*runtimeShared]struct{}),
	}, nil
}

// Mode 返回宿主服务的运行模式：isolated 本机进程。
func (h *ProcessHost) Mode() string { return loader.ModeIsolated }

func (h *ProcessHost) Verify(ctx context.Context, manifest loader.Manifest) error {
	if manifest.Mode != loader.ModeIsolated {
		return loader.ErrUnsupportedMode
	}
	_, err := h.resolveVerifiedSpec(ctx, manifest)
	return err
}

func (h *ProcessHost) Load(ctx context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	if manifest.Mode != loader.ModeIsolated {
		return nil, loader.ErrUnsupportedMode
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return nil, loader.ErrShuttingDown
	}

	// Load 再次解析并校验，避免 Verify 与真正执行之间替换安装单元。
	spec, err := h.resolveVerifiedSpec(ctx, manifest)
	if err != nil {
		return nil, err
	}
	switch manifest.Role {
	case loader.RoleExecutor:
		return h.loadExecutor(ctx, manifest, spec)
	case loader.RoleProvider:
		return h.loadCapability(ctx, manifest, spec)
	default:
		return nil, loader.ErrInvalidManifest
	}
}

// loadExecutor 启动（或连接）executor.v1 运行时进程。
func (h *ProcessHost) loadExecutor(ctx context.Context, manifest loader.Manifest, spec packagecontract.ProcessSpec) (loader.Runtime, error) {
	var process *Process
	if h.shouldSpawn(manifest) {
		var err error
		process, err = StartProcess(ctx, spec, h.config.Stdout, h.config.Stderr)
		if err != nil {
			return nil, loader.ErrUnavailable
		}
	}
	shared := &runtimeShared{
		manifest: manifest, process: process,
		socketPath: unixSocketPath(spec.Address), stopGrace: h.config.StopGrace,
		terminateGrace: h.config.TerminateGrace, host: h,
	}
	runtime := &executorRuntime{transportRuntime: transportRuntime{runtimeShared: shared}}
	runtime.transportClose = runtime.closeTransport

	connection, client, err := dialExecutor(ctx, spec.Address, process, h.config.DialTimeout)
	if err != nil {
		if process != nil {
			err = errors.Join(err, process.Reap(context.Background(), h.config.StopGrace, h.config.TerminateGrace))
		}
		return nil, errors.Join(loader.ErrUnavailable, err)
	}
	runtime.transport = &executorTransport{connection: connection}
	runtime.client = client
	h.track(shared)
	shared.watchProcessExit()
	return runtime, nil
}

// loadCapability 启动 capability 进程并经 runtime_host 协议装载。
func (h *ProcessHost) loadCapability(ctx context.Context, manifest loader.Manifest, spec packagecontract.ProcessSpec) (loader.Runtime, error) {
	if !h.shouldSpawn(manifest) {
		return nil, ErrInvalidProcessSpec
	}
	process, err := StartProcess(ctx, spec, h.config.Stdout, h.config.Stderr)
	if err != nil {
		return nil, loader.ErrUnavailable
	}
	shared := &runtimeShared{
		manifest: manifest, process: process,
		socketPath: unixSocketPath(spec.Address), stopGrace: h.config.StopGrace,
		terminateGrace: h.config.TerminateGrace, host: h,
	}
	wrapped := &providerRuntime{transportRuntime: transportRuntime{runtimeShared: shared}}
	wrapped.transportClose = wrapped.closeTransport
	h.track(shared)

	grpcHost, err := loader.NewGRPCHost(loader.GRPCHostConfig{
		Mode: loader.ModeIsolated, Address: spec.Address, DialTimeout: h.config.DialTimeout,
		VerifyInstalled: func(context.Context, loader.Manifest) error { return nil },
	})
	if err != nil {
		return nil, errors.Join(err, wrapped.releaseAfterLoad(ctx))
	}
	// 进程在加载完成前退出（启动失败）时取消加载，避免拨号/生命周期调用永不返回。
	watchContext, stopWatch := ProcessWatchContext(ctx, process)
	loaded, err := grpcHost.Load(watchContext, manifest)
	stopWatch()
	if err != nil {
		if process.Exited() {
			err = loader.ErrUnavailable
		}
		return nil, errors.Join(err, wrapped.releaseAfterLoad(ctx))
	}
	core, ok := loaded.(processRuntimeCore)
	if !ok {
		return nil, errors.Join(loader.ErrUnavailable, wrapped.releaseAfterLoad(ctx))
	}
	wrapped.core = core
	if closer, ok := core.(loader.TransportCloser); ok {
		wrapped.transport = closer
	}
	wrapped.start = core.Start
	wrapped.health = core.Health
	wrapped.invoke = core.Invoke
	wrapped.stopTransport = core.Stop
	shared.watchProcessExit()
	return wrapped, nil
}

func (h *ProcessHost) track(runtime *runtimeShared) {
	h.mu.Lock()
	h.runtimes[runtime] = struct{}{}
	h.mu.Unlock()
}

func (h *ProcessHost) remove(runtime *runtimeShared) {
	h.mu.Lock()
	delete(h.runtimes, runtime)
	h.mu.Unlock()
}

// Close 强制清理本宿主监督的全部运行时（连接关闭 + 进程回收 + socket 清理）。
func (h *ProcessHost) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	runtimes := make([]*runtimeShared, 0, len(h.runtimes))
	for runtime := range h.runtimes {
		runtimes = append(runtimes, runtime)
	}
	h.mu.Unlock()
	var result []error
	for _, runtime := range runtimes {
		if err := runtime.hostClose(ctx); err != nil {
			result = append(result, err)
		} else {
			h.remove(runtime)
		}
	}
	// 停止进程退出监视（避免向已注销运行的订阅者发送意外退出通知）。
	for _, runtime := range runtimes {
		runtime.stopExitWatch()
	}
	return errors.Join(result...)
}

// validateSpec 校验解析出的进程规格：连接模式的 executor 只校验地址与限额，
// 由本宿主启动的进程叠加文件系统与内容安全校验。
func (h *ProcessHost) validateSpec(manifest loader.Manifest, spec packagecontract.ProcessSpec) error {
	if manifest.Role == loader.RoleExecutor && !h.shouldSpawn(manifest) {
		if !packagecontract.IsLocalRuntimeAddress(spec.Address) || !packagecontract.ValidProcessLimits(spec.Limits) {
			return ErrInvalidProcessSpec
		}
		return nil
	}
	return validateProcessSpec(spec)
}

// shouldSpawn 返回当前清单的进程管理策略。
func (h *ProcessHost) shouldSpawn(manifest loader.Manifest) bool {
	if h.config.SpawnFor != nil {
		return h.config.SpawnFor(manifest)
	}
	return h.config.Spawn
}

// resolveVerifiedSpec 读取并校验来源锁定的规格。
func (h *ProcessHost) resolveVerifiedSpec(ctx context.Context, manifest loader.Manifest) (packagecontract.ProcessSpec, error) {
	spec, err := h.config.Resolve(ctx, manifest)
	if err != nil {
		return packagecontract.ProcessSpec{}, errors.Join(loader.ErrUnavailable, err)
	}
	if err := h.validateSpec(manifest, spec); err != nil {
		return packagecontract.ProcessSpec{}, err
	}
	if h.config.Verify != nil {
		if err := h.config.Verify(ctx, manifest, spec); err != nil {
			return packagecontract.ProcessSpec{}, err
		}
	}
	return spec, nil
}

// processRuntimeCore 是隔离 provider 进程承载的运行时面：生命周期与治理
// 调用（provider 角色由 GRPCHost 装载，运行时同时实现两者）。
type processRuntimeCore interface {
	loader.Runtime
	loader.Invoker
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
// 清理（优雅终止 → 强制终止）与限额释放封装为原语，供监督面复用。
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
// 输出去向：默认丢弃，只有组合根显式选择时才透传。
func StartProcess(ctx context.Context, spec packagecontract.ProcessSpec, stdout, stderr io.Writer) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(spec.Path, spec.Args...)
	// 空环境必须使用非 nil 切片；nil 会让 os/exec 继承 Core 的全部环境。
	command.Env = []string{}
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

// runtimeShared 是进程监督面：静态身份、存活门控、优雅停止与强制清理
// （传输释放 → 进程回收 → socket 清理 → 限额释放）。process 为 nil 表示
// 连接模式（进程不由本宿主管理，监督只剩传输与 socket 回收）；
// stopTransport / transportClose 是装配时接线的传输面释放口。
type runtimeShared struct {
	manifest       loader.Manifest
	process        *Process
	socketPath     string
	stopGrace      time.Duration
	terminateGrace time.Duration
	host           *ProcessHost
	stopTransport  func(context.Context) error
	transportClose func() error

	mu      sync.Mutex
	stopped bool
	// exitNotified 保证意外退出通知恰好一次（即使进程被监督清理回收）。
	exitNotified bool
}

// watchProcessExit 在进程退出时回调宿主的 OnRuntimeExit；已进入停止流程
// （stopped）或退出原因已知为监督清理时静默。由 Load 在登记后启动，hostClose
// 注销时停止。
func (r *runtimeShared) watchProcessExit() {
	if r.process == nil || r.host == nil || r.host.config.OnRuntimeExit == nil {
		return
	}
	go func() {
		<-r.process.Done()
		r.mu.Lock()
		notified := r.stopped || r.exitNotified
		r.exitNotified = true
		r.mu.Unlock()
		if notified {
			return
		}
		r.host.config.OnRuntimeExit(r.manifest, r.process.Err())
	}()
}

// stopExitWatch 撤销意外退出通知（停止流程已接管进程回收）。
func (r *runtimeShared) stopExitWatch() {
	r.mu.Lock()
	r.exitNotified = true
	r.mu.Unlock()
}

func (r *runtimeShared) Describe(context.Context) (loader.Description, error) {
	return loader.Description{ID: r.manifest.ID, Version: r.manifest.Version, Mode: r.manifest.Mode}, nil
}

// alive 报告监督对象是否存活：连接模式（无进程）视为存活。
func (r *runtimeShared) alive() bool {
	return r.process == nil || !r.process.Exited()
}

// waitExit 在宽限期内等待进程退出；无进程（连接模式）视为已退出。
func (r *runtimeShared) waitExit(ctx context.Context, grace time.Duration) bool {
	if r.process == nil {
		return true
	}
	return r.process.Wait(ctx, grace)
}

// Stop 优雅停止：先通知传输面收尾（生命周期 RPC），宽限期内未退出则强制
// 清理并告警。
func (r *runtimeShared) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	if !r.alive() {
		return r.finishLocked()
	}
	if r.stopTransport != nil {
		if stopErr := r.stopTransport(ctx); stopErr == nil && r.waitExit(ctx, r.stopGrace) {
			return r.finishLocked()
		}
	}
	// 进入强制清理：进程回收由监督接管，撤销意外退出通知（已持锁）。
	r.exitNotified = true
	cleanupContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), r.stopGrace+r.terminateGrace+time.Second,
	)
	defer cancel()
	if err := r.forceCleanupLocked(cleanupContext); err != nil {
		return errors.Join(ErrProcessCleanup, err)
	}
	observe.Warn(ctx, "隔离运行时未在宽限期内退出，已完成强制清理")
	return r.finishLocked()
}

// hostClose 强制回收：不经生命周期 RPC 直接关闭传输并回收进程与 socket，
// 幂等（宿主 Close 与装载失败清理共用）。
func (r *runtimeShared) hostClose(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	r.exitNotified = true
	if err := r.forceCleanupLocked(ctx); err != nil {
		return err
	}
	return r.finishLocked()
}

// forceCleanupLocked 强制清理（锁内）：关闭传输 → 优雅终止 + terminateGrace
// 等待 → 强制终止 + stopGrace 等待 → 回收 socket 与限额。
func (r *runtimeShared) forceCleanupLocked(ctx context.Context) error {
	var cleanupFailed bool
	if r.transportClose != nil {
		if err := r.transportClose(); err != nil {
			cleanupFailed = true
		}
	}
	// 连接模式（无进程）或进程已退出：只回收传输与 socket。
	if r.process == nil || r.process.Exited() {
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

// finishLocked 标记停止、从宿主注销并释放传输与限额。
func (r *runtimeShared) finishLocked() error {
	var cleanupFailed bool
	if r.transportClose != nil {
		if err := r.transportClose(); err != nil {
			cleanupFailed = true
		}
	}
	if err := removeRuntimeSocket(r.socketPath); err != nil {
		cleanupFailed = true
	}
	r.stopped = true
	if r.host != nil {
		r.host.remove(r)
	}
	if r.process != nil {
		r.process.Release()
	}
	if cleanupFailed {
		return ErrProcessCleanup
	}
	return nil
}

func (r *runtimeShared) completeProcessCleanup(cleanupFailed bool) error {
	if err := removeRuntimeSocket(r.socketPath); err != nil {
		cleanupFailed = true
	}
	if cleanupFailed {
		return ErrProcessCleanup
	}
	return nil
}

// releaseAfterLoad 是装载失败的清理原语：运行时已登记到宿主，强制清理
// （传输 + 进程 + socket）后注销，等待上限与 Stop 一致。
func (r *runtimeShared) releaseAfterLoad(ctx context.Context) error {
	cleanupContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), r.stopGrace+r.terminateGrace+time.Second,
	)
	defer cancel()
	return r.hostClose(cleanupContext)
}

// transportRuntime 是传输面骨架：存活门控 + 委派（Describe 直接回放静态身份，
// Start/Health/Invoke 委派给传输闭包，角色差异收在闭包里；Stop 走监督面优雅
// 停止），两种 role 的运行时内嵌本骨架。
type transportRuntime struct {
	*runtimeShared
	// transport 是承载协议的传输面；nil 表示尚未装载完成或无连接（无进程
	// 且无长连接的形态）。
	transport loader.TransportCloser
	// start 是传输面的启动闭包（executor 为空操作）。
	start func(context.Context) error
	// health/invoke 是传输面的健康与调用闭包（前置存活门控）。
	health func(context.Context) error
	invoke func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error)
}

// closeTransport 关闭底层连接（不经生命周期 RPC），幂等。
func (r *transportRuntime) closeTransport() error {
	if r.transport == nil {
		return nil
	}
	return r.transport.CloseTransport()
}

func (r *transportRuntime) Stop(ctx context.Context) error {
	return r.runtimeShared.Stop(ctx)
}

func (r *transportRuntime) Start(ctx context.Context) error {
	if !r.alive() {
		return loader.ErrUnavailable
	}
	if r.start == nil {
		return nil
	}
	return r.start(ctx)
}

// providerRuntime 是 runtime_host 协议面的运行时：传输面 = GRPCHost 装载的
// runtime_host 实现（生命周期 RPC + 治理调用），监督面统一。
type providerRuntime struct {
	transportRuntime
	// core 是装载完成的 runtime_host 实现；nil 表示装载未完成。
	core processRuntimeCore
}

func (r *providerRuntime) Health(ctx context.Context) error {
	if !r.alive() {
		return loader.ErrUnavailable
	}
	return r.core.Health(ctx)
}

func (r *providerRuntime) Invoke(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
	if !r.alive() {
		return nil, loader.ErrUnavailable
	}
	return r.core.Invoke(ctx, request, payload)
}

// executorRuntime 是 executor.v1 协议面的运行时：传输面 = executor.v1 会话
// 客户端，监督面统一。
type executorRuntime struct {
	transportRuntime
	// client 是 executor.v1 会话客户端（loader.RoleExecutor 的能力面）。
	client executor.Client
}

func (r *executorRuntime) Health(ctx context.Context) error {
	if !r.alive() {
		return loader.ErrUnavailable
	}
	return validateExecutorHealth(ctx, r.client)
}

// Client 实现 executor.ClientProvider。
func (r *executorRuntime) Client() executor.Client { return r.client }

// validateExecutorHealth 是 executor.v1 健康检查：Health RPC + 协议版本与
// 就绪校验。
func validateExecutorHealth(ctx context.Context, client executor.Client) error {
	response, err := client.Health(ctx, &executor.HealthRequest{
		AcceptedProtocolVersions: []string{executor.Version},
	})
	if err != nil {
		return err
	}
	if err := executor.ValidateHealthResponse(response); err != nil {
		return errors.Join(loader.ErrRuntimeProtocol, err)
	}
	if !response.Ready || !executor.Supports(response.SupportedProtocolVersions) {
		return loader.ErrUnavailable
	}
	return nil
}

// executorTransport 是 executor.v1 传输面：gRPC 连接（连接释放归监督面）。
type executorTransport struct {
	connection *grpc.ClientConn
}

// CloseTransport 实现 loader.TransportCloser。
func (t *executorTransport) CloseTransport() error {
	if t.connection == nil {
		return nil
	}
	err := t.connection.Close()
	t.connection = nil
	return err
}

func dialExecutor(ctx context.Context, address string, process *Process, dialTimeout time.Duration) (*grpc.ClientConn, executor.Client, error) {
	watchContext, stopWatch := ProcessWatchContext(ctx, process)
	defer stopWatch()
	dialContext, cancel := context.WithTimeout(watchContext, dialTimeout)
	defer cancel()
	connection, err := grpc.DialContext(
		dialContext,
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(executor.MaxGRPCMessageBytes),
			grpc.MaxCallSendMsgSize(executor.MaxGRPCMessageBytes),
		),
	)
	if err != nil {
		return nil, nil, err
	}
	return connection, executor.NewClient(connection), nil
}

// unixSocketPath 提取 unix 地址的 socket 路径；非 unix 地址（连接模式 TCP）
// 返回空串（无本地 socket 需要回收）。
func unixSocketPath(address string) string {
	if strings.HasPrefix(address, "unix:") {
		return strings.TrimPrefix(address, "unix:")
	}
	return ""
}

var (
	_ loader.Runtime          = (*providerRuntime)(nil)
	_ loader.Runtime          = (*executorRuntime)(nil)
	_ loader.Invoker          = (*providerRuntime)(nil)
	_ executor.ClientProvider = (*executorRuntime)(nil)
)

func validateProcessSpec(spec packagecontract.ProcessSpec) error {
	// 运行时重新校验形状，再叠加装载时刻的文件系统与内容安全校验。
	if err := packagecontract.ValidateProcessSpec(spec); err != nil {
		return ErrInvalidProcessSpec
	}
	if err := packageio.ValidateSecurePath(spec.Path); err != nil {
		return ErrInvalidProcessSpec
	}
	info, err := os.Lstat(spec.Path)
	if err != nil || !info.Mode().IsRegular() || !executableFile(info) {
		return ErrInvalidProcessSpec
	}
	if err := packageio.ValidateSecureDirectory(spec.WorkDir); err != nil {
		return ErrInvalidProcessSpec
	}
	if strings.HasPrefix(spec.Address, "unix:") {
		socketPath := strings.TrimPrefix(spec.Address, "unix:")
		relativeSocket, err := filepath.Rel(spec.WorkDir, socketPath)
		if err != nil || relativeSocket == "." || relativeSocket == ".." ||
			strings.HasPrefix(relativeSocket, ".."+string(filepath.Separator)) {
			return ErrInvalidProcessSpec
		}
		if _, err := os.Lstat(socketPath); err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrInvalidProcessSpec
		}
	}
	for _, argument := range spec.Args {
		if len(argument) > 4096 || strings.ContainsRune(argument, '\x00') {
			return ErrInvalidProcessSpec
		}
	}
	return nil
}

func removeRuntimeSocket(socketPath string) error {
	if socketPath == "" {
		return nil
	}
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

// ValidProcessDuration 校验进程生命周期宽限期的合理范围。
func ValidProcessDuration(value time.Duration) bool {
	return value >= 100*time.Millisecond && value <= time.Minute
}
