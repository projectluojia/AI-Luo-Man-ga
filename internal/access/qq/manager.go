package qq

import (
	"context"
	"errors"
	"sync"
	"time"

	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
)

const defaultManagerStopTimeout = 5 * time.Second

// Runner 是 QQ 连接运行端口，便于 Manager 与具体 WebSocket 实现解耦。
type Runner interface {
	Run(context.Context) error
}

// RunnerFactory 根据当前非秘密配置创建 QQ 运行端。Token 等秘密由闭包提供，
// 不会进入设置快照或管理 API。
type RunnerFactory func(qqsettings.Settings, func(bool)) (Runner, error)

// RuntimeStatus 是管理面可公开的运行状态，不包含 Token 和底层错误正文。
type RuntimeStatus struct {
	Running   bool `json:"running"`
	Connected bool `json:"connected"`
}

// Manager 负责 QQ 配置的持久化、启动、停止和热更新。
type Manager struct {
	store       qqsettings.Store
	factory     RunnerFactory
	stopTimeout time.Duration

	operationMu sync.Mutex
	mu          sync.Mutex
	ctx         context.Context
	settings    qqsettings.Settings
	status      RuntimeStatus
	cancel      context.CancelFunc
	done        chan struct{}
	started     bool
}

func NewManager(store qqsettings.Store, factory RunnerFactory, stopTimeout time.Duration) (*Manager, error) {
	if store == nil || factory == nil {
		return nil, errors.New("qq manager configuration is incomplete")
	}
	if stopTimeout <= 0 {
		stopTimeout = defaultManagerStopTimeout
	}
	return &Manager{store: store, factory: factory, stopTimeout: stopTimeout}, nil
}

// Start 读取持久配置；首次启动时用 seed 建立默认记录。seed 只用于首次运行，
// 之后的修改以 WebUI 保存的数据库值为准。
func (m *Manager) Start(ctx context.Context, seed qqsettings.Settings) error {
	if ctx == nil {
		return errors.New("qq manager context is nil")
	}
	current, _, err := m.store.EnsureQQSettings(ctx, seed)
	if err != nil {
		return err
	}
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("qq manager already started")
	}
	m.ctx = ctx
	m.started = true
	m.mu.Unlock()
	return m.replaceLocked(current)
}

// Snapshot 返回当前配置和运行状态；配置来自内存中最近一次成功读取/保存的版本。
func (m *Manager) Snapshot(context.Context) (qqsettings.Settings, RuntimeStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return qqsettings.Settings{}, RuntimeStatus{}, errors.New("qq manager is not started")
	}
	return m.settings, m.status, nil
}

// Update 使用代数 CAS 保存并立即应用新配置。旧连接会先有界退出，再启动新连接。
func (m *Manager) Update(ctx context.Context, expectedGeneration uint64, replacement qqsettings.Settings) (qqsettings.Settings, RuntimeStatus, error) {
	if ctx == nil {
		return qqsettings.Settings{}, RuntimeStatus{}, errors.New("qq manager context is nil")
	}
	if _, err := qqsettings.Normalize(replacement); err != nil {
		return qqsettings.Settings{}, RuntimeStatus{}, err
	}
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return qqsettings.Settings{}, RuntimeStatus{}, errors.New("qq manager is not started")
	}
	m.mu.Unlock()
	updated, err := m.store.CompareAndSwapQQSettings(ctx, expectedGeneration, replacement)
	if err != nil {
		return qqsettings.Settings{}, RuntimeStatus{}, err
	}
	if err := m.replaceLocked(updated); err != nil {
		return updated, RuntimeStatus{}, err
	}
	_, status, err := m.Snapshot(ctx)
	if err != nil {
		return updated, RuntimeStatus{}, err
	}
	return updated, status, nil
}

func (m *Manager) replaceLocked(settings qqsettings.Settings) error {
	m.mu.Lock()
	oldCancel, oldDone := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.status = RuntimeStatus{}
	m.settings = settings
	root := m.ctx
	m.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
		if oldDone != nil {
			select {
			case <-oldDone:
			case <-time.After(m.stopTimeout):
				return errors.New("qq adapter did not stop before timeout")
			}
		}
	}
	if !settings.Enabled {
		return nil
	}
	generation := settings.Generation
	runner, err := m.factory(settings, func(connected bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.settings.Generation == generation {
			m.status.Connected = connected
		}
	})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(root)
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel, m.done, m.status.Running = cancel, done, true
	m.mu.Unlock()
	go func() {
		defer close(done)
		_ = runner.Run(runCtx)
		m.mu.Lock()
		if m.settings.Generation == generation {
			m.status.Running = false
			m.status.Connected = false
		}
		m.mu.Unlock()
	}()
	return nil
}

// Shutdown 有界停止当前适配器。
func (m *Manager) Shutdown(ctx context.Context) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.started = false
	m.status = RuntimeStatus{}
	m.mu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(m.stopTimeout):
		return errors.New("qq adapter did not stop before timeout")
	}
}
