package loader

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ExecutorHostConfig 描述一个 executor.v1 运行时的连接与可选进程监督。
// 运行时的实现语言、启动参数和安装身份由组合根提供，宿主只处理进程、连接
// 和协议生命周期。
type ExecutorHostConfig struct {
	Manifest Manifest
	Resolve  func(context.Context) (packmgr.ProcessSpec, error)
	Spawn    bool
	Model    string
	Stdout   io.Writer
	Stderr   io.Writer

	DialTimeout    time.Duration
	StopGrace      time.Duration
	TerminateGrace time.Duration
}

// ExecutorHost 是 executor.v1 的通用宿主。它既可以连接外部已启动的执行者，
// 也可以监督组合根提供的本地进程；不包含 Agent、Provider 或 Python 特化逻辑。
type ExecutorHost struct {
	config ExecutorHostConfig
}

// NewExecutorHost 构造通用执行者宿主。
func NewExecutorHost(config ExecutorHostConfig) (*ExecutorHost, error) {
	if config.Resolve == nil || config.Model == "" || config.Manifest.Role != RoleExecutor {
		return nil, ErrInvalidManifest
	}
	if config.Manifest.Mode != ModeHosted && config.Manifest.Mode != ModeIsolated {
		return nil, ErrUnsupportedMode
	}
	if err := ValidateManifest(config.Manifest); err != nil {
		return nil, err
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
	return &ExecutorHost{config: config}, nil
}

// Mode 返回该执行者清单声明的运行模式。
func (h *ExecutorHost) Mode() string { return h.config.Manifest.Mode }

// Manifest 返回宿主绑定的执行者清单。
func (h *ExecutorHost) Manifest() Manifest { return h.config.Manifest }

// Verify 要求运行时身份与宿主绑定的完整清单一致。
func (h *ExecutorHost) Verify(_ context.Context, manifest Manifest) error {
	if !manifest.Equal(h.config.Manifest) {
		return ErrDescribeMismatch
	}
	return nil
}

// Load 建立执行者协议连接；Spawn=true 时先启动并监督组合根提供的进程。
func (h *ExecutorHost) Load(ctx context.Context, manifest Manifest) (Runtime, error) {
	if err := h.Verify(ctx, manifest); err != nil {
		return nil, err
	}
	spec, err := h.config.Resolve(ctx)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := validateExecutorSpec(spec, h.config.Spawn); err != nil {
		return nil, err
	}
	var process *Process
	if h.config.Spawn {
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
	return &executorRuntime{
		id: manifest.ID, version: manifest.Version, mode: manifest.Mode,
		process: process, connection: connection, client: client,
		model: h.config.Model, stopGrace: h.config.StopGrace, terminateGrace: h.config.TerminateGrace,
	}, nil
}

func validateExecutorSpec(spec packmgr.ProcessSpec, spawn bool) error {
	if !packmgr.IsLocalRuntimeAddress(spec.Address) || !packmgr.ValidProcessLimits(spec.Limits) {
		return ErrInvalidProcessSpec
	}
	if !spawn {
		return nil
	}
	if err := validateProcessSpec(spec); err != nil {
		return err
	}
	return nil
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

type executorRuntime struct {
	id, version, mode string
	process           *Process
	connection        *grpc.ClientConn
	client            executor.Client
	model             string
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
	return health.ExecutorChecker{Client: r.client, Model: r.model}.Ping(ctx)
}

func (r *executorRuntime) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		var processErr error
		if r.process != nil {
			processErr = r.process.Reap(ctx, r.stopGrace, r.terminateGrace)
		}
		var connectionErr error
		if r.connection != nil {
			connectionErr = r.connection.Close()
			r.connection = nil
		}
		r.stopErr = errors.Join(processErr, connectionErr)
	})
	return r.stopErr
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

var (
	_ Runtime                   = (*executorRuntime)(nil)
	_ executor.ClientProvider   = (*executorRuntime)(nil)
	_ executor.ProcessLifecycle = (*executorRuntime)(nil)
)
