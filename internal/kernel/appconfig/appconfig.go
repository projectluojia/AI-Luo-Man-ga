package appconfig

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
)

var (
	ErrNotFound = errors.New("app configuration not found")
	ErrConflict = errors.New("app configuration version conflict")
	ErrInvalid  = errors.New("invalid app configuration")
)

var (
	stableIDPattern   = id.AppID
	capabilityPattern = id.StableLower
	permissionPattern = id.Permission
	channelPattern    = id.StableLower
	modelPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

type Config struct {
	AppID               string
	Revision            string
	Generation          uint64
	Enabled             bool
	Model               string
	SystemPrompt        string
	ChannelPrompts      map[string]string // 渠道提示段（channel → 提示）；装配时按 Run 渠道追加
	Timezone            string
	MaxSteps            uint32
	MaxToolCalls        uint32
	MaxInputTokens      uint64
	MaxOutputTokens     uint64
	MaxTotalTokens      uint64
	MaxOutputBytes      uint64
	MaxCostMicrousd     uint64
	ProviderTimeout     time.Duration
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
		!validValues(s.EnabledCapabilities, capabilityPattern) ||
		!validValues(s.PermissionScope, permissionPattern) {
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
	config.Model = strings.TrimSpace(config.Model)
	config.Timezone = strings.TrimSpace(config.Timezone)
	if len(config.EnabledCapabilities) > 256 || len(config.PermissionScope) > 256 {
		return Config{}, ErrInvalid
	}
	config.EnabledCapabilities = canonical(config.EnabledCapabilities)
	config.PermissionScope = canonical(config.PermissionScope)
	config.ChannelPrompts = canonicalChannelPrompts(config.ChannelPrompts)
	if err := Validate(config); err != nil {
		return Config{}, err
	}
	revisionInput := struct {
		AppID               string
		Enabled             bool
		Model               string
		SystemPrompt        string
		ChannelPrompts      map[string]string `json:",omitempty"` // 空时省略，旧行摘要与迁移前一致
		Timezone            string
		MaxSteps            uint32
		MaxToolCalls        uint32
		MaxInputTokens      uint64
		MaxOutputTokens     uint64
		MaxTotalTokens      uint64
		MaxOutputBytes      uint64
		MaxCostMicrousd     uint64
		ProviderTimeoutMS   int64
		EnabledCapabilities []string
		PermissionScope     []string
	}{
		AppID: config.AppID, Enabled: config.Enabled, Model: config.Model,
		SystemPrompt: config.SystemPrompt, ChannelPrompts: config.ChannelPrompts, Timezone: config.Timezone,
		MaxSteps: config.MaxSteps, MaxToolCalls: config.MaxToolCalls,
		MaxInputTokens: config.MaxInputTokens, MaxOutputTokens: config.MaxOutputTokens,
		MaxTotalTokens: config.MaxTotalTokens, MaxOutputBytes: config.MaxOutputBytes,
		MaxCostMicrousd: config.MaxCostMicrousd, ProviderTimeoutMS: config.ProviderTimeout.Milliseconds(),
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
	if !stableIDPattern.MatchString(config.AppID) || !modelPattern.MatchString(config.Model) ||
		len(config.SystemPrompt) == 0 || strings.TrimSpace(config.SystemPrompt) == "" || len(config.SystemPrompt) > 16<<10 ||
		!utf8.ValidString(config.SystemPrompt) || strings.ContainsRune(config.SystemPrompt, '\x00') ||
		len(config.Timezone) == 0 || len(config.Timezone) > 128 ||
		config.MaxSteps < 1 || config.MaxSteps > 64 ||
		config.MaxToolCalls < 1 || config.MaxToolCalls > 128 ||
		config.MaxInputTokens < 1 || config.MaxInputTokens > 10_000_000 ||
		config.MaxOutputTokens < 1 || config.MaxOutputTokens > 1_000_000 ||
		config.MaxTotalTokens < config.MaxInputTokens || config.MaxTotalTokens > 11_000_000 ||
		config.MaxOutputBytes < 1 || config.MaxOutputBytes > 256<<10 ||
		config.MaxCostMicrousd > 1_000_000_000_000 ||
		config.ProviderTimeout < 100*time.Millisecond || config.ProviderTimeout > 5*time.Minute ||
		len(config.EnabledCapabilities) > 256 || len(config.PermissionScope) > 256 ||
		!validValues(config.EnabledCapabilities, capabilityPattern) ||
		!validValues(config.PermissionScope, permissionPattern) {
		return ErrInvalid
	}
	for channel, prompt := range config.ChannelPrompts {
		if !channelPattern.MatchString(channel) || len(channel) > 64 ||
			strings.TrimSpace(prompt) == "" || len(prompt) > 8<<10 || !utf8.ValidString(prompt) ||
			strings.ContainsRune(prompt, '\x00') {
			return ErrInvalid
		}
	}
	if _, err := time.LoadLocation(config.Timezone); err != nil {
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

// canonicalChannelPrompts 复制渠道提示映射，防止调用方后续修改影响已归一化
// 的配置；JSON 序列化对 map 按键排序，摘要哈希因此是确定的。
func canonicalChannelPrompts(prompts map[string]string) map[string]string {
	if len(prompts) == 0 {
		return nil
	}
	result := make(map[string]string, len(prompts))
	for channel, prompt := range prompts {
		result[channel] = prompt
	}
	return result
}

func validValues(values []string, pattern *regexp.Regexp) bool {
	for index, value := range values {
		if !pattern.MatchString(value) || len(value) > 128 || (index > 0 && values[index-1] >= value) {
			return false
		}
	}
	return true
}
