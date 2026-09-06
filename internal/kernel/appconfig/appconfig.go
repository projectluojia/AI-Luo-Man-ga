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
	AppID              string
	Revision           string
	Generation         uint64
	Enabled            bool
	ExecutorID         string
	ExecutorConfig     json.RawMessage
	MaxSteps           uint32
	MaxCapabilityCalls uint32
	MaxExecutionUnits  uint64
	MaxOutputBytes     uint64
	MaxCostMicrousd    uint64
	ExecutionTimeout   time.Duration
	CapabilityGrants   []capability.Grant
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Source interface {
	Current(context.Context, string) (Config, error)
	Revision(context.Context, string, string) (Config, error)
}

type PolicySnapshot struct {
	AppID            string
	Revision         string
	Generation       uint64
	Enabled          bool
	CapabilityGrants []capability.Grant
}

// CapabilityIDs 返回当前策略可投影的 Capability 标识，结果按稳定顺序排列。
func (s PolicySnapshot) CapabilityIDs() []string {
	ids := make([]string, 0, len(s.CapabilityGrants))
	for _, grant := range s.CapabilityGrants {
		ids = append(ids, grant.CapabilityID)
	}
	sort.Strings(ids)
	return unique(ids)
}

// HasCapability 判断策略是否包含指定 Capability 的授权。
func (s PolicySnapshot) HasCapability(capabilityID string) bool {
	for _, grant := range s.CapabilityGrants {
		if grant.CapabilityID == capabilityID {
			return true
		}
	}
	return false
}

func (s PolicySnapshot) Verify(expectedAppID string) error {
	if s.AppID != expectedAppID || !stableIDPattern.MatchString(s.AppID) ||
		!stableIDPattern.MatchString(s.Revision) || s.Generation == 0 ||
		len(s.CapabilityGrants) > 256 || !validGrants(s.AppID, s.CapabilityGrants) {
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
		CapabilityGrants: cloneGrants(config.CapabilityGrants),
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
	if len(config.CapabilityGrants) > 256 {
		return Config{}, ErrInvalid
	}
	var err error
	config.CapabilityGrants, err = normalizeGrants(config.AppID, config.CapabilityGrants)
	if err != nil {
		return Config{}, err
	}
	if err := Validate(config); err != nil {
		return Config{}, err
	}
	revisionInput := struct {
		AppID              string
		Enabled            bool
		ExecutorID         string
		ExecutorConfig     json.RawMessage
		MaxSteps           uint32
		MaxCapabilityCalls uint32
		MaxExecutionUnits  uint64
		MaxOutputBytes     uint64
		MaxCostMicrousd    uint64
		ExecutionTimeoutMS int64
		CapabilityGrants   []capability.Grant
	}{
		AppID: config.AppID, Enabled: config.Enabled, ExecutorID: config.ExecutorID,
		ExecutorConfig: config.ExecutorConfig, MaxSteps: config.MaxSteps,
		MaxCapabilityCalls: config.MaxCapabilityCalls, MaxExecutionUnits: config.MaxExecutionUnits,
		MaxOutputBytes: config.MaxOutputBytes, MaxCostMicrousd: config.MaxCostMicrousd,
		ExecutionTimeoutMS: config.ExecutionTimeout.Milliseconds(),
		CapabilityGrants:   config.CapabilityGrants,
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
		len(config.CapabilityGrants) > 256 || !validGrants(config.AppID, config.CapabilityGrants) {
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

func normalizeGrants(appID string, grants []capability.Grant) ([]capability.Grant, error) {
	result := cloneGrants(grants)
	for index := range result {
		grant, err := capability.NormalizeGrant(result[index])
		if err != nil || grant.AppID != appID {
			return nil, ErrInvalid
		}
		result[index] = grant
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	if !validGrants(appID, result) {
		return nil, ErrInvalid
	}
	return result, nil
}

func cloneGrants(grants []capability.Grant) []capability.Grant {
	result := make([]capability.Grant, len(grants))
	copy(result, grants)
	for index := range result {
		result[index].Resource.IDs = append([]string(nil), result[index].Resource.IDs...)
	}
	return result
}

func validGrants(appID string, grants []capability.Grant) bool {
	for index, grant := range grants {
		if grant.AppID != appID || (index > 0 && grants[index-1].ID >= grant.ID) {
			return false
		}
		if _, err := capability.NormalizeGrant(grant); err != nil {
			return false
		}
	}
	return true
}

func unique(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
