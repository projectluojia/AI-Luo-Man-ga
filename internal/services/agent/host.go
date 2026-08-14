package agent

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"time"

	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/executorv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Spec 是内置 agent 进程的启动规格，由内核装配提供。
type Spec struct {
	PythonPath string
	WorkDir    string
	Address    string
	Env        []string
	Limits     loader.ProcessLimits
}

// Config 装配内置 AI 执行者宿主。
type Config struct {
	// Resolve 返回 agent 进程启动规格（python 路径、工作目录、监听地址、限额）。
	Resolve func(context.Context) (Spec, error)
	// Spawn 为 true 时内核负责启动与监督 agent 进程；false 表示连接运维已启动的
	// agent（内核不拥有进程，仅连接其 gRPC 地址）。
	Spawn bool
	// Model 是加载期健康检查使用的模型标识（协议协商 + Provider 就绪）。
	Model string
	// Stdout / Stderr 是 agent 进程的输出目标（仅 Spawn 时使用；agent 的 Python
	// 日志透传内核输出，与 installed 扩展的丢弃策略不同）。
	Stdout, Stderr io.Writer
	// DialTimeout 是 agent 协议拨号上限；StopGrace/TerminateGrace 是进程停止的
	// 优雅宽限与强制清理宽限。
	DialTimeout    time.Duration
	StopGrace      time.Duration
	TerminateGrace time.Duration
}

// Host 以 isolated 形态监督内置 AI 执行者：实现 loader.Host，进程启动、
// 限额与清理复用 loader 进程原语（StartProcess/Process），不含专用宿主代码。
type Host struct {
	config   Config
	manifest loader.Manifest
}

// NewHost 构造内置 agent 宿主；配置非法时返回显式错误。
func NewHost(config Config) (*Host, error) {
	if config.Resolve == nil || config.Model == "" {
		return nil, loader.ErrUnavailable
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
	if !loader.ValidProcessDuration(config.DialTimeout) || !loader.ValidProcessDuration(config.StopGrace) ||
		!loader.ValidProcessDuration(config.TerminateGrace) {
		return nil, loader.ErrInvalidManifest
	}
	return &Host{
		config: config,
		manifest: loader.Manifest{
			ID: RuntimeID, Version: runtimeVersion, Mode: loader.ModeIsolated,
			Role: loader.RoleExecutor, LockedDigest: runtimeDigest, Pin: true,
		},
	}, nil
}

// Mode 返回宿主服务的运行模式：isolated 本机进程。
func (h *Host) Mode() string { return loader.ModeIsolated }

// Manifest 返回内置 agent 清单（pinned：常驻不随 IdleTTL 卸载）。
func (h *Host) Manifest() loader.Manifest {
	return h.manifest
}

// Verify 要求 manifest 与内置清单精确一致；任何字段不一致都拒绝加载。
func (h *Host) Verify(_ context.Context, manifest loader.Manifest) error {
	if manifest.ID != h.manifest.ID || manifest.Version != h.manifest.Version ||
		manifest.Mode != h.manifest.Mode || manifest.LockedDigest != h.manifest.LockedDigest {
		return loader.ErrDescribeMismatch
	}
	return nil
}

// Load 启动（或连接）agent 进程并建立 agent 协议连接，返回 runtime。
// 启动或连接失败一律 fail-closed：未就绪的 agent 不允许进入 Ready。
func (h *Host) Load(ctx context.Context, manifest loader.Manifest) (loader.Runtime, error) {
	if err := h.Verify(ctx, manifest); err != nil {
		return nil, err
	}
	spec, err := h.config.Resolve(ctx)
	if err != nil {
		return nil, errors.Join(loader.ErrUnavailable, err)
	}
	if err := validateSpec(spec, h.config.Spawn); err != nil {
		return nil, err
	}
	var process *loader.Process
	if h.config.Spawn {
		process, err = loader.StartProcess(ctx, loader.ProcessSpec{
			Path:    spec.PythonPath,
			Args:    []string{"-m", "agent.runtime", "--listen", spec.Address},
			Env:     spec.Env,
			WorkDir: spec.WorkDir,
			Address: spec.Address,
			Limits:  spec.Limits,
		}, h.config.Stdout, h.config.Stderr)
		if err != nil {
			return nil, loader.ErrUnavailable
		}
	}
	connection, client, err := dial(ctx, spec.Address, process, h.config.DialTimeout)
	if err != nil {
		if process != nil {
			_ = reap(process, h.config.TerminateGrace)
		}
		return nil, errors.Join(loader.ErrUnavailable, err)
	}
	return &Runtime{
		id: manifest.ID, version: manifest.Version,
		process: process, connection: connection, client: client,
		model: h.config.Model, stopGrace: h.config.StopGrace, terminateGrace: h.config.TerminateGrace,
	}, nil
}

// validateSpec 校验 agent 进程规格：监听地址必须是 loopback、限额在合理上限内；
// Spawn 模式额外要求绝对 python 路径与可选绝对工作目录。
func validateSpec(spec Spec, spawn bool) error {
	if !loader.IsLocalRuntimeAddress(spec.Address) || !loader.ValidProcessLimits(spec.Limits) {
		return loader.ErrInvalidProcessSpec
	}
	if spawn {
		if !filepath.IsAbs(spec.PythonPath) || filepath.Clean(spec.PythonPath) != spec.PythonPath {
			return loader.ErrInvalidProcessSpec
		}
		if spec.WorkDir != "" && (!filepath.IsAbs(spec.WorkDir) || filepath.Clean(spec.WorkDir) != spec.WorkDir) {
			return loader.ErrInvalidProcessSpec
		}
	}
	return nil
}

// dial 连接执行者协议（executor.proto），与能力提供者扩展的 runtime_host.proto
// 不同。Spawn 模式下进程退出（启动失败）会取消拨号，避免连接永不返回。
func dial(ctx context.Context, address string, process *loader.Process, dialTimeout time.Duration) (*grpc.ClientConn, executorv1.ExecutorRuntimeClient, error) {
	watchContext, stopWatch := loader.ProcessWatchContext(ctx, process)
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
	return connection, executorv1.NewExecutorRuntimeClient(connection), nil
}

// reap 在加载失败时回收 agent 进程：强制终止、等待退出并释放限额。
func reap(process *loader.Process, terminateGrace time.Duration) error {
	if process.Exited() {
		process.Release()
		return nil
	}
	if err := process.Kill(); err != nil && !process.Exited() {
		return loader.ErrProcessCleanup
	}
	cleanupContext, cancel := context.WithTimeout(context.Background(), terminateGrace)
	defer cancel()
	if !process.Wait(cleanupContext, terminateGrace) {
		return loader.ErrProcessCleanup
	}
	process.Release()
	return nil
}
