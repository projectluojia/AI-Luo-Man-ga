// Package config 提供 Deployment 级本机配置控制面服务。
package config

import (
	"errors"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq"
	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/promptcatalog"
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
	Model                   string                  `json:"model"`
	ExecutorTimeoutSeconds  float64                 `json:"executor_timeout_seconds"`
	QQEnabled               bool                    `json:"qq_enabled"`
	QQWSURL                 string                  `json:"qq_ws_url"`
	QQBotID                 string                  `json:"qq_bot_id"`
	QQAllowedGroupIDs       []string                `json:"qq_allowed_group_ids"`
	QQAllowedPrivateUserIDs []string                `json:"qq_allowed_private_user_ids"`
	QQQuickReplies          []QQQuickReply          `json:"qq_quick_replies"`
	QQPokeReplies           []string                `json:"qq_poke_replies"`
	PromptCatalog           promptcatalog.Catalog   `json:"prompt_catalog"`
	BaseSystemPrompt        string                  `json:"base_system_prompt"`
	ChannelPrompts          map[string]string       `json:"channel_prompts"`
	AgentRun                AgentRunSettings        `json:"agent_run"`
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
	Model                   string                  `json:"model"`
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
	PromptCatalog           promptcatalog.Catalog   `json:"prompt_catalog"`
	BaseSystemPrompt        string                  `json:"base_system_prompt"`
	ChannelPrompts          map[string]string       `json:"channel_prompts"`
	AgentRun                AgentRunSettings        `json:"agent_run"`
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

// Snapshot 是管理 API 返回值，只有秘密是否已配置，不返回秘密正文。
type Snapshot struct {
	Settings            Settings     `json:"settings"`
	QQWSTokenConfigured bool         `json:"qq_ws_token_configured"`
	Runtime             RuntimeState `json:"runtime"`
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
		QQAllowedGroupIDs:      []string{}, QQAllowedPrivateUserIDs: []string{},
		QQQuickReplies: []QQQuickReply{}, QQPokeReplies: qq.DefaultPokeReplies(),
		PromptCatalog:    promptcatalog.Default(),
		BaseSystemPrompt: promptcatalog.DefaultBaseSystemPrompt,
		ChannelPrompts:   promptcatalog.DefaultChannelPrompts(),
		AgentRun:         defaultAgentRunSettings(),
		Orchestration:    defaultOrchestrationSettings(),
		ContextAssembly:  defaultContextAssemblySettings(),
		Scheduler:        defaultSchedulerSettings(),
		QQConnection:     defaultQQConnectionSettings(),
		RuntimeProcess:   defaultRuntimeProcessSettings(),
		Governance:       defaultGovernanceSettings(),
	}
}

func normalize(input SaveInput) (Settings, error) {
	settings := Settings{
		Revision: input.Revision, AppID: strings.TrimSpace(input.AppID), Model: strings.TrimSpace(input.Model),
		ExecutorTimeoutSeconds: input.ExecutorTimeoutSeconds, QQEnabled: input.QQEnabled, QQWSURL: strings.TrimSpace(input.QQWSURL),
		QQBotID: strings.TrimSpace(input.QQBotID), QQAllowedGroupIDs: input.QQAllowedGroupIDs,
		QQAllowedPrivateUserIDs: input.QQAllowedPrivateUserIDs, QQQuickReplies: input.QQQuickReplies,
		QQPokeReplies: input.QQPokeReplies, PromptCatalog: input.PromptCatalog,
		BaseSystemPrompt: input.BaseSystemPrompt,
		ChannelPrompts:   input.ChannelPrompts,
		AgentRun:         input.AgentRun,
		Orchestration:    input.Orchestration,
		ContextAssembly:  input.ContextAssembly,
		Scheduler:        input.Scheduler,
		QQConnection:     input.QQConnection,
		RuntimeProcess:   input.RuntimeProcess,
		Governance:       input.Governance,
	}
	promptCatalog, err := promptcatalog.Normalize(input.PromptCatalog)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	baseSystemPrompt, err := promptcatalog.NormalizeBaseSystemPrompt(input.BaseSystemPrompt)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	channelPrompts, err := promptcatalog.NormalizeChannelPrompts(input.ChannelPrompts)
	if err != nil {
		return Settings{}, ErrInvalid
	}
	agentRun, err := normalizeAgentRun(input.AgentRun)
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
	if identity.ValidateAppID(settings.AppID) != nil || len(settings.Model) == 0 || len(settings.Model) > 256 ||
		settings.ExecutorTimeoutSeconds < 0.1 || settings.ExecutorTimeoutSeconds > 120 {
		return Settings{}, ErrInvalid
	}
	settings.PromptCatalog = promptCatalog
	settings.BaseSystemPrompt = baseSystemPrompt
	settings.ChannelPrompts = channelPrompts
	settings.AgentRun = agentRun
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
		Revision: settings.Revision, AppID: settings.AppID, Model: settings.Model,
		ExecutorTimeoutSeconds: settings.ExecutorTimeoutSeconds, QQEnabled: settings.QQEnabled, QQWSURL: settings.QQWSURL,
		QQBotID: settings.QQBotID, QQAllowedGroupIDs: settings.QQAllowedGroupIDs,
		QQAllowedPrivateUserIDs: settings.QQAllowedPrivateUserIDs, QQQuickReplies: settings.QQQuickReplies,
		QQPokeReplies: settings.QQPokeReplies, PromptCatalog: settings.PromptCatalog,
		BaseSystemPrompt: settings.BaseSystemPrompt,
		ChannelPrompts:   settings.ChannelPrompts,
		AgentRun:         settings.AgentRun,
		Orchestration:    settings.Orchestration,
		ContextAssembly:  settings.ContextAssembly,
		Scheduler:        settings.Scheduler,
		QQConnection:     settings.QQConnection,
		RuntimeProcess:   settings.RuntimeProcess,
		Governance:       settings.Governance,
	})
}
