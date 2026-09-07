package echo

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
)

const (
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
	Result       Output     `json:"result,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

type RunRecord struct {
	ID         string `json:"run_id"`
	RunGroupID string `json:"run_group_id"`
	AppID      string `json:"app_id"`
	EchoID     string `json:"echo_id"`
	// ParentRunID 和 OriginCallID 只记录通用执行因果关系；Core 不为某种
	// Executor 定义子任务语义。
	ParentRunID          string             `json:"parent_run_id,omitempty"`
	OriginCallID         string             `json:"origin_call_id,omitempty"`
	SessionID            string             `json:"session_id,omitempty"`
	UserID               string             `json:"user_id,omitempty"`
	MessageID            string             `json:"message_id,omitempty"`
	Channel              string             `json:"channel,omitempty"` // 平台渠道，恢复重装配时传入执行上下文
	Attempt              uint32             `json:"attempt"`
	Status               string             `json:"status"`
	ExecutorID           string             `json:"executor_id"`
	ConfigRevision       string             `json:"config_revision"`
	ProtocolVersion      string             `json:"protocol_version"`
	ExecutorConfig       json.RawMessage    `json:"-"`
	InputPayload         []byte             `json:"-"`
	InputContentType     string             `json:"-"`
	ContextDigest        string             `json:"context_digest,omitempty"`
	ContextSources       json.RawMessage    `json:"-"`
	MaxSteps             uint32             `json:"max_steps"`
	MaxCapabilityCalls   uint32             `json:"max_capability_calls"`
	MaxExecutionUnits    uint64             `json:"max_execution_units"`
	MaxOutputBytes       uint64             `json:"max_output_bytes"`
	MaxCostMicrousd      uint64             `json:"max_cost_microusd"`
	ExecutionTimeoutMS   uint32             `json:"execution_timeout_ms"`
	UsedExecutionUnits   uint64             `json:"used_execution_units"`
	UsedCostMicrousd     uint64             `json:"used_cost_microusd"`
	UsedRetries          uint32             `json:"used_retries"`
	Deadline             time.Time          `json:"deadline"`
	AvailableAt          time.Time          `json:"available_at"`
	LeaseToken           string             `json:"-"`
	LeaseExpiresAt       *time.Time         `json:"lease_expires_at,omitempty"`
	LastExecutorSequence uint64             `json:"last_executor_sequence"`
	CapabilityGrants     []capability.Grant `json:"-"`
	RecoverableState     json.RawMessage    `json:"-"`
	Result               Output             `json:"-"`
	ErrorCode            string             `json:"error_code,omitempty"`
	ErrorMessage         string             `json:"error_message,omitempty"`
	CreatedAt            time.Time          `json:"created_at"`
	StartedAt            *time.Time         `json:"started_at,omitempty"`
	CompletedAt          *time.Time         `json:"completed_at,omitempty"`
}

type RunWork struct {
	Run RunRecord `json:"run"`
}

// Output 是 Executor 返回的最终或中间输出。Core 只保存并转发 payload，
// 由具体 Access 决定是否把某种 content type 渲染为文本。
type Output struct {
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
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

// RunRequest 是一次 Echo 创建请求。会话上下文（SessionID/UserID/MessageID/
// Channel）只能来自受治理的平台接入入口，不进入 HTTP 契约（json:"-"），
// 客户端无法伪造或指定；由 Web/平台适配器在 Intake 成功后填充。
type RunRequest struct {
	Message        string `json:"message"`
	IdempotencyKey string `json:"-"`
	SessionID      string `json:"-"`
	UserID         string `json:"-"`
	MessageID      string `json:"-"`
	Channel        string `json:"-"` // 平台渠道（web/qq 群聊/qq 私聊），用于执行上下文
}

type PublicRun struct {
	RunRecord
	Result string `json:"result,omitempty"`
}

func PublicRunRecord(run RunRecord) PublicRun {
	result := ""
	contentType, _, err := mime.ParseMediaType(run.Result.ContentType)
	if err == nil && contentType == "text/plain" && utf8.Valid(run.Result.Data) {
		result = string(run.Result.Data)
	}
	return PublicRun{RunRecord: run, Result: result}
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

// EchoCreationStore 是 Echo 与首个 queued Run 原子创建的端口。
type EchoCreationStore interface {
	CreateEchoRunIdempotentLimited(ctx context.Context, key, fingerprint string, echo Record, run RunRecord, maxPending int) (string, bool, error)
}

// RunExecutionStore 是 Run 领取、执行进度和终态转换的端口。
type RunExecutionStore interface {
	GetRun(ctx context.Context, appID, runID string) (RunRecord, error)
	ClaimRun(ctx context.Context, appID, echoID, runID, leaseToken string, startedAt, leaseExpiresAt time.Time) (RunRecord, error)
	RenewRunLease(ctx context.Context, run RunRecord, renewedAt, leaseExpiresAt time.Time) error
	AdvanceRunExecutorSequence(ctx context.Context, run RunRecord, sequence uint64) error
	AdvanceRunExecutorSequenceWithUsage(ctx context.Context, run RunRecord, sequence, executionUnits, costMicrousd uint64, retries uint32) error
	// SetRunContext 固化 Run 的上下文摘要与来源版本（每次执行只可设置一次）。
	SetRunContext(ctx context.Context, run RunRecord, digest string, sources json.RawMessage) error
	RetryRun(ctx context.Context, current, next RunRecord, failure publicerror.Error, completedAt time.Time) error
	CompleteRun(ctx context.Context, run RunRecord, runStatus, echoStatus string, output Output, failure publicerror.Error, completedAt time.Time) error
}

// RunRecoveryStore 是启动恢复和持久队列读取的端口。
type RunRecoveryStore interface {
	ListQueuedRuns(ctx context.Context, appID string, limit int) ([]RunWork, error)
	ListRunnableRuns(ctx context.Context, appID string, now time.Time, limit int) ([]RunWork, error)
	FailAbandonedRuns(ctx context.Context, appID string, now time.Time) (int64, error)
}

// RunCancellationStore 是取消 queued Run 的端口。
type RunCancellationStore interface {
	CancelQueuedRun(ctx context.Context, appID, echoID string, completedAt time.Time) (bool, error)
	CancelQueuedRuns(ctx context.Context, appID string, completedAt time.Time) error
}

// EchoEventStore 是 Echo 事件追加与重放的端口。
type EchoEventStore interface {
	AppendEchoEvent(ctx context.Context, event Event) (Event, error)
	GetEcho(ctx context.Context, appID, echoID string) (Record, []Event, error)
}

// CapabilityAuditStore 是 Capability 调用审计持久化的端口。
type CapabilityAuditStore interface {
	RecordCapabilityCall(ctx context.Context, callID, runID, echoID, appID, capabilityID string, payload []byte, success bool, failure publicerror.Error, duration time.Duration) error
}

// StorePorts 是 Orchestrator 所需的最小领域端口集合。SQLite 只是组合根提供的
// 一个实现，Run 编排不依赖具体数据库类型。
type StorePorts struct {
	Idempotency  idempotency.Store
	Creation     EchoCreationStore
	Execution    RunExecutionStore
	Recovery     RunRecoveryStore
	Cancellation RunCancellationStore
	Events       EchoEventStore
	Audit        CapabilityAuditStore
}
