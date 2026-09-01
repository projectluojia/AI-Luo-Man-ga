package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const maxSettingsFileBytes = 256 << 10

// Service 持久化本机配置与秘密，并向内核监督循环发布配置修订。
type Service struct {
	mu             sync.RWMutex
	settingsPath   string
	qqSecretPath   string
	settings       Settings
	qqTokenPresent bool
	runtime        RuntimeState
	changes        chan struct{}
}

func NewService(root string) (*Service, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve local configuration root: %w", err)
	}
	service := &Service{
		settingsPath: filepath.Join(absoluteRoot, "ailuo-settings.json"),
		qqSecretPath: filepath.Join(absoluteRoot, "secrets", "qq-ws-token"),
		settings:     DefaultSettings(), runtime: RuntimeState{State: "setup_required", Message: "请完成首次配置"},
		changes: make(chan struct{}, 1),
	}
	if err := service.load(); err != nil {
		return nil, err
	}
	return service, nil
}

func (m *Service) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Snapshot{Settings: cloneSettings(m.settings), QQWSTokenConfigured: m.qqTokenPresent, Runtime: m.runtime}
}

func (m *Service) Save(input SaveInput) (Snapshot, error) {
	settings, err := normalize(input)
	if err != nil || len(input.QQWSToken) > MaxQQTokenBytes {
		return Snapshot{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if input.Revision != m.settings.Revision {
		return Snapshot{}, ErrConflict
	}
	if input.ClearQQWSToken {
		if err := os.Remove(m.qqSecretPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, fmt.Errorf("clear qq token: %w", err)
		}
		m.qqTokenPresent = false
	} else if input.QQWSToken != "" {
		if err := writePrivateFile(m.qqSecretPath, []byte(input.QQWSToken)); err != nil {
			return Snapshot{}, err
		}
		m.qqTokenPresent = true
	}
	settings.Revision = m.settings.Revision + 1
	settings.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(settings)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode local settings: %w", err)
	}
	if err := writePrivateFile(m.settingsPath, encoded); err != nil {
		return Snapshot{}, err
	}
	m.settings = settings
	m.runtime = RuntimeState{State: "restarting", Message: "正在应用新配置", Revision: settings.Revision}
	select {
	case m.changes <- struct{}{}:
	default:
	}
	return Snapshot{Settings: cloneSettings(m.settings), QQWSTokenConfigured: m.qqTokenPresent, Runtime: m.runtime}, nil
}

func (m *Service) CurrentResolved() (Resolved, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.settings.Revision == 0 {
		return Resolved{}, false
	}
	qqTokenFile := ""
	if m.qqTokenPresent {
		qqTokenFile = m.qqSecretPath
	}
	return Resolved{Settings: cloneSettings(m.settings), QQWSTokenFile: qqTokenFile}, true
}

func (m *Service) Changes() <-chan struct{} { return m.changes }

func (m *Service) SetRuntime(state, message string, revision uint64) {
	m.mu.Lock()
	m.runtime = RuntimeState{State: state, Message: message, Revision: revision}
	m.mu.Unlock()
}

func (m *Service) WaitReady(ctx context.Context) (Resolved, error) {
	for {
		if resolved, ready := m.CurrentResolved(); ready {
			return resolved, nil
		}
		select {
		case <-ctx.Done():
			return Resolved{}, ctx.Err()
		case <-m.changes:
		}
	}
}

func (m *Service) load() error {
	data, err := os.ReadFile(m.settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		m.refreshSecrets()
		return nil
	}
	if err != nil {
		return fmt.Errorf("read local settings: %w", err)
	}
	if len(data) == 0 || len(data) > maxSettingsFileBytes {
		return ErrInvalid
	}
	settings, err := decodeStoredSettings(data)
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	normalized, err := validateStored(settings)
	if err != nil || settings.Revision == 0 {
		return ErrInvalid
	}
	normalized.UpdatedAt = settings.UpdatedAt
	m.settings = normalized
	m.refreshSecrets()
	if m.settings.Revision > 0 {
		m.runtime = RuntimeState{State: "starting", Message: "等待内核启动", Revision: settings.Revision}
	}
	return nil
}

func (m *Service) refreshSecrets() {
	m.qqTokenPresent = regularNonEmptyFile(m.qqSecretPath, MaxQQTokenBytes)
}

func writePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create private file directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ailuo-config-*")
	if err != nil {
		return fmt.Errorf("create private temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("restrict private file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write private file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync private file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close private file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish private file: %w", err)
	}
	if err := restrictPrivateFileACL(path); err != nil {
		return err
	}
	return nil
}

func cloneSettings(settings Settings) Settings {
	settings.QQAllowedGroupIDs = slices.Clone(settings.QQAllowedGroupIDs)
	settings.QQAllowedPrivateUserIDs = slices.Clone(settings.QQAllowedPrivateUserIDs)
	settings.QQQuickReplies = slices.Clone(settings.QQQuickReplies)
	settings.QQPokeReplies = slices.Clone(settings.QQPokeReplies)
	settings.ExecutorConfig = append(json.RawMessage(nil), settings.ExecutorConfig...)
	return settings
}
