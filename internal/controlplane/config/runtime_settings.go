package config

import (
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contextasm"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

// AgentRunSettings 是当前 App 的执行预算与时区。它们会写入 App 配置，
// 因此校验边界与 internal/kernel/appconfig 保持一致。
type AgentRunSettings struct {
	Timezone           string `json:"timezone"`
	MaxSteps           uint32 `json:"max_steps"`
	MaxCapabilityCalls uint32 `json:"max_capability_calls"`
	MaxInputTokens     uint64 `json:"max_input_tokens"`
	MaxOutputTokens    uint64 `json:"max_output_tokens"`
	MaxTotalTokens     uint64 `json:"max_total_tokens"`
	MaxOutputBytes     uint64 `json:"max_output_bytes"`
	MaxChildRuns       uint32 `json:"max_child_runs"`
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

// RuntimeProcessSettings 是 isolated 运行时的进程监督参数。
type RuntimeProcessSettings struct {
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
		Timezone: "Asia/Shanghai", MaxSteps: 8, MaxCapabilityCalls: 8,
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

func defaultRuntimeProcessSettings() RuntimeProcessSettings {
	return RuntimeProcessSettings{
		DialTimeoutSeconds: 15, StopGraceSeconds: 5, TerminateGraceSeconds: 2,
	}
}

func defaultGovernanceSettings() GovernanceSettings {
	return GovernanceSettings{ConfirmationSweepSeconds: 300}
}

func normalizeAgentRun(value AgentRunSettings) (AgentRunSettings, error) {
	if value.Timezone == "" {
		return AgentRunSettings{}, ErrInvalid
	}
	if _, err := time.LoadLocation(value.Timezone); err != nil {
		return AgentRunSettings{}, ErrInvalid
	}
	if value.MaxSteps < 1 || value.MaxSteps > 64 || value.MaxCapabilityCalls < 1 || value.MaxCapabilityCalls > 128 ||
		value.MaxInputTokens < 1 || value.MaxInputTokens > 10_000_000 || value.MaxOutputTokens < 1 || value.MaxOutputTokens > 1_000_000 ||
		value.MaxTotalTokens < value.MaxInputTokens || value.MaxTotalTokens > 11_000_000 ||
		value.MaxOutputBytes < 1 || value.MaxOutputBytes > 256<<10 || value.MaxChildRuns < 1 || value.MaxChildRuns > kernelecho.MaxChildRunsPerRoot {
		return AgentRunSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeOrchestration(value OrchestrationSettings) (OrchestrationSettings, error) {
	if value.RunTimeoutSeconds < 1 || value.RunTimeoutSeconds > 600 ||
		value.MaxRunAttempts < 1 || value.MaxRunAttempts > 10 || value.QueueCapacity < 1 || value.QueueCapacity > 10_000 ||
		value.MaxCallDepth < 1 || value.MaxCallDepth > 64 {
		return OrchestrationSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeContextAssembly(value ContextAssemblySettings) (ContextAssemblySettings, error) {
	if value.MaxMessages < 1 || value.MaxMessages > 1000 ||
		value.MaxCharsPerMsg < 1 || value.MaxCharsPerMsg > 64<<10 ||
		value.MaxTotalChars < 1 || value.MaxTotalChars > 1<<20 ||
		value.MaxPromptBytes < 1 || value.MaxPromptBytes > 32<<10 {
		return ContextAssemblySettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeScheduler(value SchedulerSettings) (SchedulerSettings, error) {
	if value.Workers < 1 || value.Workers > 64 || value.PollMs < 10 || value.PollMs > 60_000 ||
		value.BatchSize < 1 || value.BatchSize > 1000 {
		return SchedulerSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeQQConnection(value QQConnectionSettings) (QQConnectionSettings, error) {
	if value.DialTimeoutSeconds < 0.1 || value.DialTimeoutSeconds > 120 ||
		value.ReconnectDelaySeconds < 0.1 || value.ReconnectDelaySeconds > 600 ||
		value.RunTimeoutSeconds < 1 || value.RunTimeoutSeconds > 3600 ||
		value.ManagerStopTimeoutSeconds < 1 || value.ManagerStopTimeoutSeconds > 120 {
		return QQConnectionSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeRuntimeProcess(value RuntimeProcessSettings) (RuntimeProcessSettings, error) {
	if value.DialTimeoutSeconds < 1 || value.DialTimeoutSeconds > 120 ||
		value.StopGraceSeconds < 1 || value.StopGraceSeconds > 120 ||
		value.TerminateGraceSeconds < 1 || value.TerminateGraceSeconds > 60 {
		return RuntimeProcessSettings{}, ErrInvalid
	}
	return value, nil
}

func normalizeGovernance(value GovernanceSettings) (GovernanceSettings, error) {
	if value.ConfirmationSweepSeconds < 10 || value.ConfirmationSweepSeconds > 86400 {
		return GovernanceSettings{}, ErrInvalid
	}
	return value, nil
}
