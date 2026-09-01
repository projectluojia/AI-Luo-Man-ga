package appconfig

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
)

var (
	ErrNotFound = errors.New("app configuration not found")
	ErrConflict = errors.New("app configuration version conflict")
	ErrInvalid  = errors.New("invalid app configuration")
)

var (
	stableIDPattern = id.AppID
)

// Config 是 App 的持久配置。ExecutorConfig 是只由具体 Executor 解释的
// 不透明 JSON；Core 只负责限制大小、验证 JSON 和参与修订摘要，不解析其中字段。
type Config struct {
	AppID               string
	Revision            string
	Generation          uint64
	Enabled             bool
	ExecutorID          string
	ExecutorConfig      json.RawMessage
	MaxSteps            uint32
	MaxCapabilityCalls  uint32
	MaxExecutionUnits   uint64
	MaxOutputBytes      uint64
	MaxCostMicrousd     uint64
	ExecutionTimeout    time.Duration
	EnabledCapabilities []string
	PermissionScope     []string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Source interface {
	Current(context.Context, string) (Config, error)
	Revision(context.Context, string, string) (Config, error)
}

type PolicySnapshot struct {
	AppID               string
	Revision            string
	Generation          uint64
	Enabled             bool
	EnabledCapabilities []string
	PermissionScope     []string
}

func (s PolicySnapshot) CapabilityEnabled(capabilityID string) bool {
	index := sort.SearchStrings(s.EnabledCapabilities, capabilityID)
	return index < len(s.EnabledCapabilities) && s.EnabledCapabilities[index] == capabilityID
}

func (s PolicySnapshot) Verify(expectedAppID string) error {
	if s.AppID != expectedAppID || !stableIDPattern.MatchString(s.AppID) ||
		!stableIDPattern.MatchString(s.Revision) || s.Generation == 0 ||
		len(s.EnabledCapabilities) > 256 || len(s.PermissionScope) > 256 ||
		!validValues(s.EnabledCapabilities, capability.IsStableID) ||
		!validValues(s.PermissionScope, id.IsPermission) {
		return ErrInvalid
	}
	return nil
}

type PersistentPolicy struct {
	source Source
}

func NewPersistentPolicy(source Source) (*PersistentPolicy, error) {
	if source == nil {
		return nil, ErrInvalid
	}
	return &PersistentPolicy{source: source}, nil
}

func (p *PersistentPolicy) Snapshot(ctx context.Context, appID string) (PolicySnapshot, error) {
	config, err := p.source.Current(ctx, appID)
	if err != nil {
		return PolicySnapshot{}, err
	}
	if err := VerifyCurrent(config, appID); err != nil {
		return PolicySnapshot{}, err
	}
	return Snapshot(config), nil
}

func Snapshot(config Config) PolicySnapshot {
	return PolicySnapshot{
		AppID: config.AppID, Revision: config.Revision, Generation: config.Generation, Enabled: config.Enabled,
		EnabledCapabilities: append([]string(nil), config.EnabledCapabilities...),
		PermissionScope:     append([]string(nil), config.PermissionScope...),
	}
}

func Normalize(config Config) (Config, error) {
	config.AppID = strings.TrimSpace(config.AppID)
	config.ExecutorID = strings.TrimSpace(config.ExecutorID)
	if len(config.ExecutorConfig) == 0 {
		config.ExecutorConfig = json.RawMessage(`{}`)
	} else {
		config.ExecutorConfig = append(json.RawMessage(nil), config.ExecutorConfig...)
	}
	if len(config.EnabledCapabilities) > 256 || len(config.PermissionScope) > 256 {
		return Config{}, ErrInvalid
	}
	config.EnabledCapabilities = canonical(config.EnabledCapabilities)
	config.PermissionScope = canonical(config.PermissionScope)
	if err := Validate(config); err != nil {
		return Config{}, err
	}
	revisionInput := struct {
		AppID               string
		Enabled             bool
		ExecutorID          string
		ExecutorConfig      json.RawMessage
		MaxSteps            uint32
		MaxCapabilityCalls  uint32
		MaxExecutionUnits   uint64
		MaxOutputBytes      uint64
		MaxCostMicrousd     uint64
		ExecutionTimeoutMS  int64
		EnabledCapabilities []string
		PermissionScope     []string
	}{
		AppID: config.AppID, Enabled: config.Enabled, ExecutorID: config.ExecutorID,
		ExecutorConfig: config.ExecutorConfig, MaxSteps: config.MaxSteps,
		MaxCapabilityCalls: config.MaxCapabilityCalls, MaxExecutionUnits: config.MaxExecutionUnits,
		MaxOutputBytes: config.MaxOutputBytes, MaxCostMicrousd: config.MaxCostMicrousd,
		ExecutionTimeoutMS:  config.ExecutionTimeout.Milliseconds(),
		EnabledCapabilities: config.EnabledCapabilities, PermissionScope: config.PermissionScope,
	}
	encoded, err := json.Marshal(revisionInput)
	if err != nil {
		return Config{}, errors.Join(ErrInvalid, err)
	}
	config.Revision = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return config, nil
}

func Validate(config Config) error {
	if !stableIDPattern.MatchString(config.AppID) || !stableIDPattern.MatchString(config.ExecutorID) ||
		len(config.ExecutorConfig) == 0 || len(config.ExecutorConfig) > 64<<10 || !json.Valid(config.ExecutorConfig) ||
		config.MaxSteps < 1 || config.MaxSteps > 64 ||
		config.MaxCapabilityCalls < 1 || config.MaxCapabilityCalls > 256 ||
		config.MaxExecutionUnits < 1 || config.MaxExecutionUnits > 1_000_000_000 ||
		config.MaxOutputBytes < 1 || config.MaxOutputBytes > 256<<10 ||
		config.MaxCostMicrousd > 1_000_000_000_000_000 ||
		config.ExecutionTimeout < 100*time.Millisecond || config.ExecutionTimeout > 5*time.Minute ||
		len(config.EnabledCapabilities) > 256 || len(config.PermissionScope) > 256 ||
		!validValues(config.EnabledCapabilities, capability.IsStableID) ||
		!validValues(config.PermissionScope, id.IsPermission) {
		return ErrInvalid
	}
	return nil
}

func ValidateAppID(appID string) error {
	if !stableIDPattern.MatchString(appID) {
		return ErrInvalid
	}
	return nil
}

func ValidateRevision(revision string) error {
	if len(revision) != 64 {
		return ErrInvalid
	}
	for _, character := range revision {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return ErrInvalid
		}
	}
	return nil
}

func Verify(config Config, expectedAppID, expectedRevision string) error {
	if config.AppID != expectedAppID {
		return ErrInvalid
	}
	if expectedRevision != "" && config.Revision != expectedRevision {
		return ErrInvalid
	}
	normalized, err := Normalize(config)
	if err != nil || normalized.Revision != config.Revision {
		return ErrInvalid
	}
	return nil
}

func VerifyCurrent(config Config, expectedAppID string) error {
	if config.Generation == 0 {
		return ErrInvalid
	}
	return Verify(config, expectedAppID, "")
}

func canonical(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	sort.Strings(result)
	return result
}

func validValues(values []string, valid func(string) bool) bool {
	for index, value := range values {
		if !valid(value) || (index > 0 && values[index-1] >= value) {
			return false
		}
	}
	return true
}
