package config

import (
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contextasm"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

// AgentRunSettings 是 campus App Agent 的运行预算与时区。它们会写入 App 配置，
// 因此校验边界与 internal/kernel/appconfig 保持一致。
type AgentRunSettings struct {
	Timezone        string `json:"timezone"`
	MaxSteps        uint32 `json:"max_steps"`
	MaxToolCalls    uint32 `json:"max_tool_calls"`
	MaxInputTokens  uint64 `json:"max_input_tokens"`
	MaxOutputTokens uint64 `json:"max_output_tokens"`
	MaxTotalTokens  uint64 `json:"max_total_tokens"`
	MaxOutputBytes  uint64 `json:"max_output_bytes"`
	MaxChildRuns    uint32 `json:"max_child_runs"`
}

// OrchestrationSettings 是 Echo/Run 编排参数。
type OrchestrationSettings struct {
	RunTimeoutSeconds float64 `json:"run_timeout_seconds"`
	MaxRunAttempts    uint32  `json:"max_run_attempts"`
	QueueCapacity     int     `json:"queue_capacity"`
	MaxCallDepth      uint16  `json:"max_call_depth"`
}

// ContextAssemblySettings 是 contextasm 的历史与提示预算。
type ContextAssemblySettings struct {
	MaxMessages    int `json:"max_messages"`
	MaxCharsPerMsg int `json:"max_chars_per_msg"`
	MaxTotalChars  int `json:"max_total_chars"`
	MaxPromptBytes int `json:"max_prompt_bytes"`
}

// SchedulerSettings 是 Web Access 持久 Run 调度器参数。
type SchedulerSettings struct {
	Workers   int `json:"workers"`
	PollMs    int `json:"poll_ms"`
	BatchSize int `json:"batch_size"`
}

// QQConnectionSettings 是 QQ OneBot 适配器连接参数。
type QQConnectionSettings struct {
	DialTimeoutSeconds        float64 `json:"dial_timeout_seconds"`
	ReconnectDelaySeconds     float64 `json:"reconnect_delay_seconds"`
	RunTimeoutSeconds         float64 `json:"run_timeout_seconds"`
	ManagerStopTimeoutSeconds float64 `json:"manager_stop_timeout_seconds"`
}

// AgentProcessSettings 是 Python Agent 进程监督参数。
type AgentProcessSettings struct {
	DialTimeoutSeconds    float64 `json:"dial_timeout_seconds"`
	StopGraceSeconds      float64 `json:"stop_grace_seconds"`
	TerminateGraceSeconds float64 `json:"terminate_grace_seconds"`
}

// GovernanceSettings 是内核治理后台任务参数。
type GovernanceSettings struct {
	ConfirmationSweepSeconds float64 `json:"confirmation_sweep_seconds"`
}

func defaultAgentRunSettings() AgentRunSettings {
	return AgentRunSettings{
		Timezone: "Asia/Shanghai", MaxSteps: 8, MaxToolCalls: 8,
		MaxInputTokens: 32768, MaxOutputTokens: 8192, MaxTotalTokens: 40960,
		MaxOutputBytes: 65536, MaxChildRuns: kernelecho.DefaultMaxChildRunsPerRoot,
	}
}

func defaultOrchestrationSettings() OrchestrationSettings {
	return OrchestrationSettings{
		RunTimeoutSeconds: 90, MaxRunAttempts: 3, QueueCapacity: 128, MaxCallDepth: 16,
	}
}

func defaultContextAssemblySettings() ContextAssemblySettings {
	budget := contextasm.DefaultBudget()
	return ContextAssemblySettings{
		MaxMessages: budget.MaxMessages, MaxCharsPerMsg: budget.MaxCharsPerMsg,
		MaxTotalChars: budget.MaxTotalChars, MaxPromptBytes: budget.MaxPromptBytes,
	}
}

func defaultSchedulerSettings() SchedulerSettings {
	return SchedulerSettings{Workers: 4, PollMs: 250, BatchSize: 32}
}

func defaultQQConnectionSettings() QQConnectionSettings {
	return QQConnectionSettings{
		DialTimeoutSeconds: 10, ReconnectDelaySeconds: 5, RunTimeoutSeconds: 180,
		ManagerStopTimeoutSeconds: 5,
	}
}

func defaultAgentProcessSettings() AgentProcessSettings {
	return AgentProcessSettings{
		DialTimeoutSeconds: 15, StopGraceSeconds: 5, TerminateGraceSeconds: 2,
	}
}

func defaultGovernanceSettings() GovernanceSettings {
	return GovernanceSettings{ConfirmationSweepSeconds: 300}
}

func normalizeAgentRun(value AgentRunSettings) (AgentRunSettings, error) {
	if value.Timezone == "" {
		value.Timezone = defaultAgentRunSettings().Timezone
	}
	if _, err := time.LoadLocation(value.Timezone); err != nil {
		return AgentRunSettings{}, ErrInvalid
	}
	defaults := defaultAgentRunSettings()
	if value.MaxSteps == 0 {
		value.MaxSteps = defaults.MaxSteps
	}
	if value.MaxToolCalls == 0 {
		value.MaxToolCalls = defaults.MaxToolCalls
	}
	if value.MaxInputTokens == 0 {
		value.MaxInputTokens = defaults.MaxInputTokens
	}
	if value.MaxOutputTokens == 0 {
		value.MaxOutputTokens = defaults.MaxOutputTokens
	}
	if value.MaxTotalTokens == 0 {
		value.MaxTotalTokens = defaults.MaxTotalTokens
	}
	if value.MaxOutputBytes == 0 {
		value.MaxOutputBytes = defaults.MaxOutputBytes
	}
	if value.MaxChildRuns == 0 {
		value.MaxChildRuns = defaults.MaxChildRuns
	}
	if value.MaxSteps > 64 || value.MaxToolCalls > 128 ||
		value.MaxInputTokens > 10_000_000 || value.MaxOutputTokens > 1_000_000 ||
		value.MaxTotalTokens < value.MaxInputTokens || value.MaxTotalTokens > 11_000_000 ||
		value.MaxOutputBytes > 256<<10 || value.MaxChildRuns > kernelecho.MaxChildRunsPerRoot {
		return AgentRunSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeOrchestration(value OrchestrationSettings) (OrchestrationSettings, error) {
	defaults := defaultOrchestrationSettings()
	if value.RunTimeoutSeconds == 0 {
		value.RunTimeoutSeconds = defaults.RunTimeoutSeconds
	}
	if value.MaxRunAttempts == 0 {
		value.MaxRunAttempts = defaults.MaxRunAttempts
	}
	if value.QueueCapacity == 0 {
		value.QueueCapacity = defaults.QueueCapacity
	}
	if value.MaxCallDepth == 0 {
		value.MaxCallDepth = defaults.MaxCallDepth
	}
	if value.RunTimeoutSeconds < 1 || value.RunTimeoutSeconds > 600 ||
		value.MaxRunAttempts > 10 || value.QueueCapacity < 1 || value.QueueCapacity > 10_000 ||
		value.MaxCallDepth > 64 {
		return OrchestrationSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeContextAssembly(value ContextAssemblySettings) (ContextAssemblySettings, error) {
	defaults := defaultContextAssemblySettings()
	if value.MaxMessages == 0 {
		value.MaxMessages = defaults.MaxMessages
	}
	if value.MaxCharsPerMsg == 0 {
		value.MaxCharsPerMsg = defaults.MaxCharsPerMsg
	}
	if value.MaxTotalChars == 0 {
		value.MaxTotalChars = defaults.MaxTotalChars
	}
	if value.MaxPromptBytes == 0 {
		value.MaxPromptBytes = defaults.MaxPromptBytes
	}
	if value.MaxMessages < 1 || value.MaxMessages > 1000 ||
		value.MaxCharsPerMsg < 1 || value.MaxCharsPerMsg > 64<<10 ||
		value.MaxTotalChars < 1 || value.MaxTotalChars > 1<<20 ||
		value.MaxPromptBytes < 1 || value.MaxPromptBytes > 32<<10 {
		return ContextAssemblySettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeScheduler(value SchedulerSettings) (SchedulerSettings, error) {
	defaults := defaultSchedulerSettings()
	if value.Workers == 0 {
		value.Workers = defaults.Workers
	}
	if value.PollMs == 0 {
		value.PollMs = defaults.PollMs
	}
	if value.BatchSize == 0 {
		value.BatchSize = defaults.BatchSize
	}
	if value.Workers < 1 || value.Workers > 64 || value.PollMs < 10 || value.PollMs > 60_000 ||
		value.BatchSize < 1 || value.BatchSize > 1000 {
		return SchedulerSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeQQConnection(value QQConnectionSettings) (QQConnectionSettings, error) {
	defaults := defaultQQConnectionSettings()
	if value.DialTimeoutSeconds == 0 {
		value.DialTimeoutSeconds = defaults.DialTimeoutSeconds
	}
	if value.ReconnectDelaySeconds == 0 {
		value.ReconnectDelaySeconds = defaults.ReconnectDelaySeconds
	}
	if value.RunTimeoutSeconds == 0 {
		value.RunTimeoutSeconds = defaults.RunTimeoutSeconds
	}
	if value.ManagerStopTimeoutSeconds == 0 {
		value.ManagerStopTimeoutSeconds = defaults.ManagerStopTimeoutSeconds
	}
	if value.DialTimeoutSeconds < 0.1 || value.DialTimeoutSeconds > 120 ||
		value.ReconnectDelaySeconds < 0.1 || value.ReconnectDelaySeconds > 600 ||
		value.RunTimeoutSeconds < 1 || value.RunTimeoutSeconds > 3600 ||
		value.ManagerStopTimeoutSeconds < 1 || value.ManagerStopTimeoutSeconds > 120 {
		return QQConnectionSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeAgentProcess(value AgentProcessSettings) (AgentProcessSettings, error) {
	defaults := defaultAgentProcessSettings()
	if value.DialTimeoutSeconds == 0 {
		value.DialTimeoutSeconds = defaults.DialTimeoutSeconds
	}
	if value.StopGraceSeconds == 0 {
		value.StopGraceSeconds = defaults.StopGraceSeconds
	}
	if value.TerminateGraceSeconds == 0 {
		value.TerminateGraceSeconds = defaults.TerminateGraceSeconds
	}
	if value.DialTimeoutSeconds < 1 || value.DialTimeoutSeconds > 120 ||
		value.StopGraceSeconds < 1 || value.StopGraceSeconds > 120 ||
		value.TerminateGraceSeconds < 1 || value.TerminateGraceSeconds > 60 {
		return AgentProcessSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeGovernance(value GovernanceSettings) (GovernanceSettings, error) {
	if value.ConfirmationSweepSeconds == 0 {
		value.ConfirmationSweepSeconds = defaultGovernanceSettings().ConfirmationSweepSeconds
	}
	if value.ConfirmationSweepSeconds < 10 || value.ConfirmationSweepSeconds > 86400 {
		return GovernanceSettings{}, ErrInvalid
	}
	return value, nil
}
