package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxSettingsFileBytes = 256 << 10

// Service 持久化本机配置与秘密，并向内核监督循环发布配置修订。
type Service struct {
	mu              sync.RWMutex
	settingsPath    string
	modelSecretPath string
	qqSecretPath    string
	settings        Settings
	modelKeyPresent bool
	qqTokenPresent  bool
	runtime         RuntimeState
	changes         chan struct{}
}

func NewService(root string) (*Service, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve local configuration root: %w", err)
	}
	service := &Service{
		settingsPath:    filepath.Join(absoluteRoot, "ailuo-settings.json"),
		modelSecretPath: filepath.Join(absoluteRoot, "secrets", "model-api-key"),
		qqSecretPath:    filepath.Join(absoluteRoot, "secrets", "qq-ws-token"),
		settings:        DefaultSettings(), runtime: RuntimeState{State: "setup_required", Message: "请完成首次配置"},
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
	return Snapshot{Settings: cloneSettings(m.settings), ModelAPIKeyConfigured: m.modelKeyPresent, QQWSTokenConfigured: m.qqTokenPresent, Runtime: m.runtime}
}

func (m *Service) Save(input SaveInput) (Snapshot, error) {
	settings, err := normalize(input)
	if err != nil || len(input.ModelAPIKey) > MaxModelAPIKeyBytes || len(input.QQWSToken) > MaxQQTokenBytes {
		return Snapshot{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if input.Revision != m.settings.Revision {
		return Snapshot{}, ErrConflict
	}
	if !m.modelKeyPresent && input.ModelAPIKey == "" {
		return Snapshot{}, ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(m.modelSecretPath), 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create local secret directory: %w", err)
	}
	if input.ModelAPIKey != "" {
		if err := writePrivateFile(m.modelSecretPath, []byte(input.ModelAPIKey)); err != nil {
			return Snapshot{}, err
		}
		m.modelKeyPresent = true
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
	return Snapshot{Settings: cloneSettings(m.settings), ModelAPIKeyConfigured: m.modelKeyPresent, QQWSTokenConfigured: m.qqTokenPresent, Runtime: m.runtime}, nil
}

func (m *Service) CurrentResolved() (Resolved, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.settings.Revision == 0 || !m.modelKeyPresent {
		return Resolved{}, false
	}
	return Resolved{Settings: cloneSettings(m.settings), ModelAPIKeyFile: m.modelSecretPath, QQWSTokenFile: optionalPath(m.qqTokenPresent, m.qqSecretPath)}, true
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var settings Settings
	if err := decoder.Decode(&settings); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	normalized, err := validateStored(settings)
	if err != nil || settings.Revision == 0 {
		return ErrInvalid
	}
	normalized.UpdatedAt = settings.UpdatedAt
	m.settings = normalized
	m.refreshSecrets()
	if m.modelKeyPresent {
		m.runtime = RuntimeState{State: "starting", Message: "等待内核启动", Revision: settings.Revision}
	}
	return nil
}

func (m *Service) refreshSecrets() {
	m.modelKeyPresent = regularNonEmptyFile(m.modelSecretPath, MaxModelAPIKeyBytes)
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
	return nil
}

func regularNonEmptyFile(path string, maximum int) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && info.Size() > 0 && info.Size() <= int64(maximum)
}

func cloneSettings(settings Settings) Settings {
	settings.QQAllowedGroupIDs = append([]string(nil), settings.QQAllowedGroupIDs...)
	settings.QQAllowedPrivateUserIDs = append([]string(nil), settings.QQAllowedPrivateUserIDs...)
	settings.QQQuickReplies = append([]QQQuickReply(nil), settings.QQQuickReplies...)
	settings.QQPokeReplies = append([]string(nil), settings.QQPokeReplies...)
	settings.PromptCatalog = settings.PromptCatalog.Clone()
	settings.ChannelPrompts = cloneStringMap(settings.ChannelPrompts)
	return settings
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func optionalPath(present bool, path string) string {
	if present {
		return path
	}
	return ""
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else {
		return err
	}
}
