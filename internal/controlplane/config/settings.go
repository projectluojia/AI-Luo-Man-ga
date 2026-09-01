// Package config 提供 Deployment 级本机配置控制面服务。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq"
	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

const (
	MaxQQTokenBytes = 4096
)

var (
	ErrInvalid  = errors.New("invalid local configuration")
	ErrConflict = errors.New("local configuration revision conflict")
)

// Settings 是不含秘密的本机运行配置。
type Settings struct {
	Revision                uint64                  `json:"revision"`
	AppID                   string                  `json:"app_id"`
	ExecutorID              string                  `json:"executor_id"`
	ExecutorConfig          json.RawMessage         `json:"executor_config"`
	ExecutorTimeoutSeconds  float64                 `json:"executor_timeout_seconds"`
	QQEnabled               bool                    `json:"qq_enabled"`
	QQWSURL                 string                  `json:"qq_ws_url"`
	QQBotID                 string                  `json:"qq_bot_id"`
	QQAllowedGroupIDs       []string                `json:"qq_allowed_group_ids"`
	QQAllowedPrivateUserIDs []string                `json:"qq_allowed_private_user_ids"`
	QQQuickReplies          []QQQuickReply          `json:"qq_quick_replies"`
	QQPokeReplies           []string                `json:"qq_poke_replies"`
	Execution               ExecutionSettings       `json:"execution"`
	Orchestration           OrchestrationSettings   `json:"orchestration"`
	ContextAssembly         ContextAssemblySettings `json:"context_assembly"`
	Scheduler               SchedulerSettings       `json:"scheduler"`
	QQConnection            QQConnectionSettings    `json:"qq_connection"`
	RuntimeProcess          RuntimeProcessSettings  `json:"runtime_process"`
	Governance              GovernanceSettings      `json:"governance"`
	UpdatedAt               time.Time               `json:"updated_at"`
}

// SaveInput 是 WebUI 写入契约。秘密为空表示保留已有值。
type SaveInput struct {
	Revision                uint64                  `json:"revision"`
	AppID                   string                  `json:"app_id"`
	ExecutorID              string                  `json:"executor_id"`
	ExecutorConfig          json.RawMessage         `json:"executor_config"`
	ExecutorTimeoutSeconds  float64                 `json:"executor_timeout_seconds"`
	QQEnabled               bool                    `json:"qq_enabled"`
	QQWSURL                 string                  `json:"qq_ws_url"`
	QQWSToken               string                  `json:"qq_ws_token"`
	ClearQQWSToken          bool                    `json:"clear_qq_ws_token"`
	QQBotID                 string                  `json:"qq_bot_id"`
	QQAllowedGroupIDs       []string                `json:"qq_allowed_group_ids"`
	QQAllowedPrivateUserIDs []string                `json:"qq_allowed_private_user_ids"`
	QQQuickReplies          []QQQuickReply          `json:"qq_quick_replies"`
	QQPokeReplies           []string                `json:"qq_poke_replies"`
	Execution               ExecutionSettings       `json:"execution"`
	Orchestration           OrchestrationSettings   `json:"orchestration"`
	ContextAssembly         ContextAssemblySettings `json:"context_assembly"`
	Scheduler               SchedulerSettings       `json:"scheduler"`
	QQConnection            QQConnectionSettings    `json:"qq_connection"`
	RuntimeProcess          RuntimeProcessSettings  `json:"runtime_process"`
	Governance              GovernanceSettings      `json:"governance"`
}

// QQQuickReply 是本机控制面使用的 QQ 精确快速回复配置。
type QQQuickReply struct {
	Trigger string `json:"trigger"`
	Reply   string `json:"reply"`
}

// Snapshot 是配置服务的完整内部快照，供监督器读取；不能直接序列化到公共 API。
type Snapshot struct {
	Settings            Settings     `json:"settings"`
	QQWSTokenConfigured bool         `json:"qq_ws_token_configured"`
	Runtime             RuntimeState `json:"runtime"`
}

// PublicSettings 是配置 API 的白名单响应，不包含 opaque ExecutorConfig。
type PublicSettings struct {
	Revision                uint64                  `json:"revision"`
	AppID                   string                  `json:"app_id"`
	ExecutorID              string                  `json:"executor_id"`
	ExecutorTimeoutSeconds  float64                 `json:"executor_timeout_seconds"`
	QQEnabled               bool                    `json:"qq_enabled"`
	QQWSURL                 string                  `json:"qq_ws_url"`
	QQBotID                 string                  `json:"qq_bot_id"`
	QQAllowedGroupIDs       []string                `json:"qq_allowed_group_ids"`
	QQAllowedPrivateUserIDs []string                `json:"qq_allowed_private_user_ids"`
	QQQuickReplies          []QQQuickReply          `json:"qq_quick_replies"`
	QQPokeReplies           []string                `json:"qq_poke_replies"`
	Execution               ExecutionSettings       `json:"execution"`
	Orchestration           OrchestrationSettings   `json:"orchestration"`
	ContextAssembly         ContextAssemblySettings `json:"context_assembly"`
	Scheduler               SchedulerSettings       `json:"scheduler"`
	QQConnection            QQConnectionSettings    `json:"qq_connection"`
	RuntimeProcess          RuntimeProcessSettings  `json:"runtime_process"`
	Governance              GovernanceSettings      `json:"governance"`
	UpdatedAt               time.Time               `json:"updated_at"`
}

// PublicSnapshot 是本机配置 API 的响应快照。
type PublicSnapshot struct {
	Settings            PublicSettings `json:"settings"`
	QQWSTokenConfigured bool           `json:"qq_ws_token_configured"`
	Runtime             RuntimeState   `json:"runtime"`
}

// Public 将完整内部快照投影为不含受保护配置正文的公共响应。
func (s Snapshot) Public() PublicSnapshot {
	settings := s.Settings
	return PublicSnapshot{
		Settings: PublicSettings{
			Revision: settings.Revision, AppID: settings.AppID, ExecutorID: settings.ExecutorID,
			ExecutorTimeoutSeconds: settings.ExecutorTimeoutSeconds, QQEnabled: settings.QQEnabled,
			QQWSURL: settings.QQWSURL, QQBotID: settings.QQBotID,
			QQAllowedGroupIDs: settings.QQAllowedGroupIDs, QQAllowedPrivateUserIDs: settings.QQAllowedPrivateUserIDs,
			QQQuickReplies: settings.QQQuickReplies, QQPokeReplies: settings.QQPokeReplies,
			Execution: settings.Execution, Orchestration: settings.Orchestration,
			ContextAssembly: settings.ContextAssembly, Scheduler: settings.Scheduler,
			QQConnection: settings.QQConnection, RuntimeProcess: settings.RuntimeProcess,
			Governance: settings.Governance, UpdatedAt: settings.UpdatedAt,
		},
		QQWSTokenConfigured: s.QQWSTokenConfigured, Runtime: s.Runtime,
	}
}

// RuntimeState 描述内核相对于当前配置修订的运行状态。
type RuntimeState struct {
	State    string `json:"state"`
	Message  string `json:"message"`
	Revision uint64 `json:"revision"`
}

// Resolved 是内核启动所需的配置及秘密文件路径。
type Resolved struct {
	Settings      Settings
	QQWSTokenFile string
}

func DefaultSettings() Settings {
	return Settings{
		ExecutorTimeoutSeconds: 30,
		ExecutorConfig:         json.RawMessage(`{}`),
		QQAllowedGroupIDs:      []string{}, QQAllowedPrivateUserIDs: []string{},
		QQQuickReplies: []QQQuickReply{}, QQPokeReplies: qq.DefaultPokeReplies(),
		Execution:       defaultExecutionSettings(),
		Orchestration:   defaultOrchestrationSettings(),
		ContextAssembly: defaultContextAssemblySettings(),
		Scheduler:       defaultSchedulerSettings(),
		QQConnection:    defaultQQConnectionSettings(),
		RuntimeProcess:  defaultRuntimeProcessSettings(),
		Governance:      defaultGovernanceSettings(),
	}
}

func normalize(input SaveInput) (Settings, error) {
	settings := Settings{
		Revision: input.Revision, AppID: strings.TrimSpace(input.AppID), ExecutorID: strings.TrimSpace(input.ExecutorID),
		ExecutorConfig:         append(json.RawMessage(nil), input.ExecutorConfig...),
		ExecutorTimeoutSeconds: input.ExecutorTimeoutSeconds, QQEnabled: input.QQEnabled, QQWSURL: strings.TrimSpace(input.QQWSURL),
		QQBotID: strings.TrimSpace(input.QQBotID), QQAllowedGroupIDs: input.QQAllowedGroupIDs,
		QQAllowedPrivateUserIDs: input.QQAllowedPrivateUserIDs, QQQuickReplies: input.QQQuickReplies,
		QQPokeReplies:   input.QQPokeReplies,
		Execution:       input.Execution,
		Orchestration:   input.Orchestration,
		ContextAssembly: input.ContextAssembly,
		Scheduler:       input.Scheduler,
		QQConnection:    input.QQConnection,
		RuntimeProcess:  input.RuntimeProcess,
		Governance:      input.Governance,
	}
	execution, err := normalizeExecution(input.Execution)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	orchestration, err := normalizeOrchestration(input.Orchestration)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	contextAssembly, err := normalizeContextAssembly(input.ContextAssembly)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	scheduler, err := normalizeScheduler(input.Scheduler)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	qqConnection, err := normalizeQQConnection(input.QQConnection)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	runtimeProcess, err := normalizeRuntimeProcess(input.RuntimeProcess)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	governance, err := normalizeGovernance(input.Governance)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	if identity.ValidateAppID(settings.AppID) != nil || !id.AppID.MatchString(settings.ExecutorID) ||
		len(settings.ExecutorConfig) == 0 || len(settings.ExecutorConfig) > 64<<10 || !json.Valid(settings.ExecutorConfig) ||
		settings.ExecutorTimeoutSeconds < 0.1 || settings.ExecutorTimeoutSeconds > 120 {
		return Settings{}, ErrInvalid
	}
	settings.Execution = execution
	settings.Orchestration = orchestration
	settings.ContextAssembly = contextAssembly
	settings.Scheduler = scheduler
	settings.QQConnection = qqConnection
	settings.RuntimeProcess = runtimeProcess
	settings.Governance = governance
	qqValue, err := qqsettings.Normalize(qqsettings.Settings{
		AppID: settings.AppID, Enabled: settings.QQEnabled, WSURL: settings.QQWSURL, BotQQID: settings.QQBotID,
		AllowedGroupIDs: settings.QQAllowedGroupIDs, AllowedPrivateUserIDs: settings.QQAllowedPrivateUserIDs,
	})
	if err != nil {
		return Settings{}, ErrInvalid
	}
	settings.QQWSURL = qqValue.WSURL
	settings.QQBotID = qqValue.BotQQID
	settings.QQAllowedGroupIDs = qqValue.AllowedGroupIDs
	settings.QQAllowedPrivateUserIDs = qqValue.AllowedPrivateUserIDs
	quick := make([]qq.QuickReply, 0, len(settings.QQQuickReplies))
	if settings.QQPokeReplies == nil {
		return Settings{}, ErrInvalid
	}
	for _, rule := range settings.QQQuickReplies {
		quick = append(quick, qq.QuickReply{Trigger: rule.Trigger, Reply: rule.Reply})
	}
	quick, settings.QQPokeReplies, err = qq.NormalizeBehavior(quick, settings.QQPokeReplies)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	settings.QQQuickReplies = make([]QQQuickReply, 0, len(quick))
	for _, rule := range quick {
		settings.QQQuickReplies = append(settings.QQQuickReplies, QQQuickReply{Trigger: rule.Trigger, Reply: rule.Reply})
	}
	return settings, nil
}

func validateStored(settings Settings) (Settings, error) {
	return normalize(SaveInput{
		Revision: settings.Revision, AppID: settings.AppID, ExecutorID: settings.ExecutorID,
		ExecutorConfig:         settings.ExecutorConfig,
		ExecutorTimeoutSeconds: settings.ExecutorTimeoutSeconds, QQEnabled: settings.QQEnabled, QQWSURL: settings.QQWSURL,
		QQBotID: settings.QQBotID, QQAllowedGroupIDs: settings.QQAllowedGroupIDs,
		QQAllowedPrivateUserIDs: settings.QQAllowedPrivateUserIDs, QQQuickReplies: settings.QQQuickReplies,
		QQPokeReplies:   settings.QQPokeReplies,
		Execution:       settings.Execution,
		Orchestration:   settings.Orchestration,
		ContextAssembly: settings.ContextAssembly,
		Scheduler:       settings.Scheduler,
		QQConnection:    settings.QQConnection,
		RuntimeProcess:  settings.RuntimeProcess,
		Governance:      settings.Governance,
	})
}

// decodeStoredSettings 严格读取最终配置契约；旧模型、提示词和 Agent 字段不
// 再转换或回退到 ExecutorConfig，避免历史配置改变当前执行语义。
func decodeStoredSettings(data []byte) (Settings, error) {
	var settings Settings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Settings{}, errors.New("local configuration contains trailing data")
	}
	return settings, nil
}
