package echo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
)

const (
	SubagentCapabilityID = "agent.run"

	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	RunStatusQueued    = "queued"
	RunStatusRunning   = "running"
	RunStatusSucceeded = "succeeded"
	RunStatusFailed    = "failed"
	RunStatusCancelled = "cancelled"
	RunStatusTimedOut  = "timed_out"
)

var (
	ErrEchoNotFound       = errors.New("echo not found")
	ErrRunNotFound        = errors.New("run not found")
	ErrQueueFull          = errors.New("run queue is full")
	ErrChildRunLimit      = errors.New("child run limit reached")
	ErrRunRetryScheduled  = errors.New("run retry was durably scheduled")
	ErrInvalidTransition  = errors.New("invalid state transition")
	ErrInvalidEchoRecord  = errors.New("invalid echo record")
	ErrInvalidRunRecord   = errors.New("invalid run record")
	ErrInvalidEchoEvent   = errors.New("invalid echo event")
	ErrInvalidAuditRecord = errors.New("invalid capability audit record")
)

type Record struct {
	ID           string     `json:"echo_id"`
	AppID        string     `json:"app_id"`
	InputMessage string     `json:"input_message"`
	Status       string     `json:"status"`
	FinalMessage string     `json:"final_message,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

type RunRecord struct {
	ID                  string          `json:"run_id"`
	RunGroupID          string          `json:"run_group_id"`
	AppID               string          `json:"app_id"`
	EchoID              string          `json:"echo_id"`
	ParentRunID         string          `json:"parent_run_id,omitempty"`
	OriginCallID        string          `json:"origin_call_id,omitempty"`
	Attempt             uint32          `json:"attempt"`
	Status              string          `json:"status"`
	Model               string          `json:"model"`
	ModelConfigVersion  string          `json:"model_config_version"`
	ProtocolVersion     string          `json:"protocol_version"`
	MaxSteps            uint32          `json:"max_steps"`
	MaxToolCalls        uint32          `json:"max_tool_calls"`
	MaxInputTokens      uint64          `json:"max_input_tokens"`
	MaxOutputTokens     uint64          `json:"max_output_tokens"`
	MaxTotalTokens      uint64          `json:"max_total_tokens"`
	MaxOutputBytes      uint64          `json:"max_output_bytes"`
	MaxCostMicrousd     uint64          `json:"max_cost_microusd"`
	ProviderTimeoutMS   uint32          `json:"provider_timeout_ms"`
	UsedInputTokens     uint64          `json:"used_input_tokens"`
	UsedOutputTokens    uint64          `json:"used_output_tokens"`
	UsedTotalTokens     uint64          `json:"used_total_tokens"`
	UsedCostMicrousd    uint64          `json:"used_cost_microusd"`
	UsedProviderRetries uint32          `json:"used_provider_retries"`
	Deadline            time.Time       `json:"deadline"`
	AvailableAt         time.Time       `json:"available_at"`
	LeaseToken          string          `json:"-"`
	LeaseExpiresAt      *time.Time      `json:"lease_expires_at,omitempty"`
	LastAgentSequence   uint64          `json:"last_agent_sequence"`
	CapabilityScope     []string        `json:"capability_scope,omitempty"`
	PermissionScope     []string        `json:"-"`
	RecoverableState    json.RawMessage `json:"-"`
	ResultMessage       string          `json:"-"`
	ErrorCode           string          `json:"error_code,omitempty"`
	ErrorMessage        string          `json:"error_message,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	StartedAt           *time.Time      `json:"started_at,omitempty"`
	CompletedAt         *time.Time      `json:"completed_at,omitempty"`
}

type RunWork struct {
	Run          RunRecord `json:"run"`
	InputMessage string    `json:"input_message"`
}

type Event struct {
	AppID     string          `json:"app_id"`
	EchoID    string          `json:"echo_id"`
	RunID     string          `json:"run_id,omitempty"`
	Sequence  uint64          `json:"sequence"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type RunRequest struct {
	Message        string `json:"message"`
	IdempotencyKey string `json:"-"`
}

type ChildRunRequest struct {
	ParentRunID     string
	OriginCallID    string
	Task            string
	CapabilityScope []string
}

type ChildRunResult struct {
	RunID  string `json:"run_id"`
	Result string `json:"result"`
}

type CapabilityAuditRecord struct {
	AppID        string          `json:"app_id"`
	RunID        string          `json:"run_id"`
	CallID       string          `json:"call_id"`
	EchoID       string          `json:"echo_id"`
	CapabilityID string          `json:"capability_id"`
	Payload      json.RawMessage `json:"payload"`
	Success      bool            `json:"success"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	DurationMS   int64           `json:"duration_ms"`
	CreatedAt    time.Time       `json:"created_at"`
}

type Store interface {
	idempotency.Store
	CreateEchoRun(ctx context.Context, echo Record, run RunRecord) error
	CreateEchoRunIdempotent(ctx context.Context, key, fingerprint string, echo Record, run RunRecord) (string, bool, error)
	CreateEchoRunIdempotentLimited(ctx context.Context, key, fingerprint string, echo Record, run RunRecord, maxPending int) (string, bool, error)
	CreateChildRun(ctx context.Context, parent, child RunRecord) error
	ClaimRun(ctx context.Context, appID, echoID, leaseToken string, startedAt, leaseExpiresAt time.Time) (RunRecord, error)
	ClaimChildRun(ctx context.Context, appID, echoID, runID, parentRunID, leaseToken string, startedAt, leaseExpiresAt time.Time) (RunRecord, error)
	FailQueuedChildRun(ctx context.Context, child RunRecord, failure publicerror.Error, completedAt time.Time) error
	RenewRunLease(ctx context.Context, run RunRecord, renewedAt, leaseExpiresAt time.Time) error
	AdvanceRunAgentSequence(ctx context.Context, run RunRecord, sequence uint64) error
	AdvanceRunAgentSequenceWithUsage(ctx context.Context, run RunRecord, sequence, inputTokens, outputTokens, totalTokens, costMicrousd uint64, providerRetries uint32) error
	CancelQueuedRun(ctx context.Context, appID, echoID string, completedAt time.Time) (bool, error)
	RetryRun(ctx context.Context, current, next RunRecord, failure publicerror.Error, completedAt time.Time) error
	CompleteRun(ctx context.Context, run RunRecord, runStatus, echoStatus, finalMessage string, failure publicerror.Error, completedAt time.Time) error
	CompleteChildRun(ctx context.Context, run RunRecord, runStatus, resultMessage string, failure publicerror.Error, completedAt time.Time) error
	AppendEchoEvent(ctx context.Context, event Event) (Event, error)
	GetEcho(ctx context.Context, appID, echoID string) (Record, []Event, error)
	GetRun(ctx context.Context, appID, runID string) (RunRecord, error)
	ListRuns(ctx context.Context, appID, echoID string) ([]RunRecord, error)
	ListQueuedRuns(ctx context.Context, appID string, limit int) ([]RunWork, error)
	ListRunnableRuns(ctx context.Context, appID string, now time.Time, limit int) ([]RunWork, error)
	FailAbandonedRuns(ctx context.Context, appID string, now time.Time) (int64, error)
	RecordCapabilityCall(ctx context.Context, callID, runID, echoID, appID, capabilityID string, payload []byte, success bool, failure publicerror.Error, duration time.Duration) error
	ListCapabilityCalls(ctx context.Context, appID, echoID string) ([]CapabilityAuditRecord, error)
}
