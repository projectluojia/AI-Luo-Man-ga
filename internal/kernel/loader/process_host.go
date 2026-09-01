package loader

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
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	ErrInvalidProcessSpec = errors.New("invalid isolated runtime process specification")
	ErrProcessCleanup     = errors.New("isolated runtime process cleanup failed")
)

// ProcessHostConfig 是统一进程宿主的配置：服务 mode=isolated 的全部组件，
// role 决定装载后的协议面——capability 走 runtime_host 协议（Invoker），
// executor 走 executor.v1 协议（ClientProvider + ProcessLifecycle）。内核不
// 感知任何具体运行时的名字：进程规格由组合根聚合的包源按清单解析。
type ProcessHostConfig struct {
	// Resolve 按清单返回锁定的进程规格；解析失败的清单即该宿主不服务的
	// 清单（Verify fail-closed）。
	Resolve func(context.Context, Manifest) (packagecontract.ProcessSpec, error)
	// Verify 对解析出的规格做来源侧二次校验（例如安装锁），可空。
	Verify func(context.Context, Manifest, packagecontract.ProcessSpec) error
	// Spawn 是没有 SpawnFor 时的默认进程管理策略。
	Spawn bool
	// SpawnFor 按运行角色决定是否由本宿主启动进程；未设置时使用 Spawn。
	// capability 角色若返回 false 会在加载期 fail-closed。
	SpawnFor func(Manifest) bool
	// Stdout/Stderr 决定子进程输出去向；nil 默认丢弃。
	Stdout io.Writer
	Stderr io.Writer

	DialTimeout    time.Duration
	StopGrace      time.Duration
	TerminateGrace time.Duration
}

// ProcessHost 是统一进程宿主：按清单解析并监督本机子进程，role 决定协议面。
type ProcessHost struct {
	config ProcessHostConfig

	mu       sync.Mutex
	runtimes map[hostManagedRuntime]struct{}
	closed   bool
}

func NewProcessHost(config ProcessHostConfig) (*ProcessHost, error) {
	if config.Resolve == nil {
		return nil, ErrInvalidManifest
	}
	// capability 组件必须由本宿主启动（Load 期校验）；executor 组件允许连接
	// 外部已启动进程（Spawn=false）。
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
	if config.Stdout == nil {
		config.Stdout = io.Discard
	}
	if config.Stderr == nil {
		config.Stderr = io.Discard
	}
	return &ProcessHost{
		config: config, runtimes: make(map[hostManagedRuntime]struct{}),
	}, nil
}

// Mode 返回宿主服务的运行模式：isolated 本机进程。
func (h *ProcessHost) Mode() string { return ModeIsolated }

func (h *ProcessHost) Verify(ctx context.Context, manifest Manifest) error {
	if manifest.Mode != ModeIsolated {
		return ErrUnsupportedMode
	}
	_, err := h.resolveVerifiedSpec(ctx, manifest)
	return err
}

func (h *ProcessHost) Load(ctx context.Context, manifest Manifest) (Runtime, error) {
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
	spec, err := h.resolveVerifiedSpec(ctx, manifest)
	if err != nil {
		return nil, err
	}
	switch manifest.Role {
	case RoleExecutor:
		return h.loadExecutor(ctx, manifest, spec)
	case RoleCapability:
		return h.loadCapability(ctx, manifest, spec)
	default:
		return nil, ErrInvalidManifest
	}
}

// loadExecutor 启动（或连接）executor.v1 运行时进程。
func (h *ProcessHost) loadExecutor(ctx context.Context, manifest Manifest, spec packagecontract.ProcessSpec) (Runtime, error) {
	var process *Process
	if h.shouldSpawn(manifest) {
		var err error
		process, err = StartProcess(ctx, spec, h.config.Stdout, h.config.Stderr)
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	connection, client, err := dialExecutor(ctx, spec.Address, process, h.config.DialTimeout)
	if err != nil {
		if process != nil {
			err = errors.Join(err, process.Reap(context.Background(), h.config.StopGrace, h.config.TerminateGrace))
		}
		return nil, errors.Join(ErrUnavailable, err)
	}
	runtime := &executorRuntime{
		id: manifest.ID, version: manifest.Version, mode: manifest.Mode,
		process: process, connection: connection, client: client, host: h,
		stopGrace: h.config.StopGrace, terminateGrace: h.config.TerminateGrace,
	}
	h.track(runtime)
	return runtime, nil
}

// loadCapability 启动 capability 进程并经 runtime_host 协议装载。
func (h *ProcessHost) loadCapability(ctx context.Context, manifest Manifest, spec packagecontract.ProcessSpec) (Runtime, error) {
	if !h.shouldSpawn(manifest) {
		return nil, ErrInvalidProcessSpec
	}
	process, err := StartProcess(ctx, spec, h.config.Stdout, h.config.Stderr)
	if err != nil {
		return nil, ErrUnavailable
	}
	wrapped := &processRuntime{
		process: process, host: h, socketPath: strings.TrimPrefix(spec.Address, "unix:"),
		stopGrace: h.config.StopGrace, terminateGrace: h.config.TerminateGrace,
	}
	h.track(wrapped)

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

func (h *ProcessHost) track(runtime hostManagedRuntime) {
	h.mu.Lock()
	h.runtimes[runtime] = struct{}{}
	h.mu.Unlock()
}

func (h *ProcessHost) remove(runtime hostManagedRuntime) {
	h.mu.Lock()
	delete(h.runtimes, runtime)
	h.mu.Unlock()
}

// Close 强制清理本宿主监督的全部运行时（进程回收 + 连接关闭 + socket 清理）。
func (h *ProcessHost) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	runtimes := make([]hostManagedRuntime, 0, len(h.runtimes))
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
	return errors.Join(result...)
}

// validateSpec 校验解析出的进程规格：连接模式的 executor 只校验地址与限额，
// 由本宿主启动的进程叠加文件系统与内容安全校验。
func (h *ProcessHost) validateSpec(manifest Manifest, spec packagecontract.ProcessSpec) error {
	if manifest.Role == RoleExecutor && !h.shouldSpawn(manifest) {
		if !packagecontract.IsLocalRuntimeAddress(spec.Address) || !packagecontract.ValidProcessLimits(spec.Limits) {
			return ErrInvalidProcessSpec
		}
		return nil
	}
	return validateProcessSpec(spec)
}

// shouldSpawn 返回当前清单的进程管理策略。
func (h *ProcessHost) shouldSpawn(manifest Manifest) bool {
	if h.config.SpawnFor != nil {
		return h.config.SpawnFor(manifest)
	}
	return h.config.Spawn
}

// resolveVerifiedSpec 读取并校验来源锁定的规格。
func (h *ProcessHost) resolveVerifiedSpec(ctx context.Context, manifest Manifest) (packagecontract.ProcessSpec, error) {
	spec, err := h.config.Resolve(ctx, manifest)
	if err != nil {
		return packagecontract.ProcessSpec{}, errors.Join(ErrUnavailable, err)
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

// hostManagedRuntime 是进程宿主可强制清理的运行时统一面。
type hostManagedRuntime interface {
	Runtime
	// hostClose 强制回收进程与连接（宿主 Close 与装载失败清理用）。
	hostClose(ctx context.Context) error
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
// 所有 isolated Runtime 共用。
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

type processRuntime struct {
	runtime        *grpcRuntime
	process        *Process
	host           *ProcessHost
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

func (r *processRuntime) hostClose(ctx context.Context) error {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	if err := r.forceCleanupLocked(cleanupContext); err != nil {
		return err
	}
	return r.finish()
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

// executorRuntime 是 executor.v1 协议面的运行时：进程监督 + 会话客户端。
type executorRuntime struct {
	id, version, mode string
	process           *Process
	connection        *grpc.ClientConn
	client            executor.Client
	host              *ProcessHost
	stopGrace         time.Duration
	terminateGrace    time.Duration

	stopOnce sync.Once
	stopErr  error
}

func (r *executorRuntime) Describe(context.Context) (Description, error) {
	return Description{ID: r.id, Version: r.version, Mode: r.mode}, nil
}

func (r *executorRuntime) Start(context.Context) error { return nil }

func (r *executorRuntime) Health(ctx context.Context) error {
	if r.process != nil && r.process.Exited() {
		return ErrUnavailable
	}
	if r.client == nil {
		return ErrUnavailable
	}
	response, err := r.client.Health(ctx, &executor.HealthRequest{
		AcceptedProtocolVersions: []string{executor.Version},
	})
	if err != nil {
		return err
	}
	if err := executor.ValidateHealthResponse(response); err != nil {
		return errors.Join(ErrRuntimeProtocol, err)
	}
	if !response.Ready || !executor.Supports(response.SupportedProtocolVersions) {
		return ErrUnavailable
	}
	return nil
}

func (r *executorRuntime) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.stopErr = r.close(ctx)
	})
	return r.stopErr
}

// hostClose 强制回收：与 Stop 同语义（优雅终止 + 连接关闭），幂等。
func (r *executorRuntime) hostClose(ctx context.Context) error {
	r.stopOnce.Do(func() {
		r.stopErr = r.close(ctx)
	})
	return r.stopErr
}

func (r *executorRuntime) close(ctx context.Context) error {
	var processErr error
	if r.process != nil {
		processErr = r.process.Reap(ctx, r.stopGrace, r.terminateGrace)
	}
	var connectionErr error
	if r.connection != nil {
		connectionErr = r.connection.Close()
		r.connection = nil
	}
	if r.host != nil {
		r.host.remove(r)
	}
	return errors.Join(processErr, connectionErr)
}

func (r *executorRuntime) Client() executor.Client { return r.client }

func (r *executorRuntime) Done() <-chan struct{} {
	if r.process == nil {
		return nil
	}
	return r.process.Done()
}

func (r *executorRuntime) Err() error {
	if r.process == nil {
		return nil
	}
	return r.process.Err()
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

var (
	_ Runtime                   = (*executorRuntime)(nil)
	_ executor.ClientProvider   = (*executorRuntime)(nil)
	_ executor.ProcessLifecycle = (*executorRuntime)(nil)
	_ hostManagedRuntime        = (*executorRuntime)(nil)
	_ hostManagedRuntime        = (*processRuntime)(nil)
)

func validateProcessSpec(spec packagecontract.ProcessSpec) error {
	// 运行时重新校验形状，再叠加装载时刻的文件系统与内容安全校验。
	if err := packagecontract.ValidateProcessSpec(spec); err != nil {
		return ErrInvalidProcessSpec
	}
	info, err := os.Lstat(spec.Path)
	if err != nil || !info.Mode().IsRegular() || !executableFile(info) || unsafePermissions(info) {
		return ErrInvalidProcessSpec
	}
	if info, err = os.Lstat(spec.WorkDir); err != nil || !info.IsDir() || unsafePermissions(info) {
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
