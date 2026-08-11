// Package task 提供内核级后台任务模型与调度。
//
// 设计边界：
//   - 后台任务（Task）使用独立于 Agent Run 的持久状态机；Agent Run 继续由
//     internal/kernel/echo 调度，本模块只负责系统级后台任务。
//   - 持久存储是任务状态的唯一权威；调度器只负责轮询领取、租约续期、执行、
//     死亡任务恢复与优雅关闭，进程崩溃后任务可由持久状态确定性恢复。
//   - 任务类型必须通过 TypeRegistry 预先注册为封闭集合；调度器绝不执行未注册
//     类型的任务，参数在创建与执行时均按注册 Schema 重新校验。
//   - 错误分类默认失败关闭：只有显式标记为可重试的临时性错误才允许自动重试，
//     且重试带退避并有次数上限；含不安全副作用的类型默认不自动重试。
//   - 任务通过处理器执行受治理的业务动作；需要 Agent 推理时应由 Go 创建正式
//     的 Agent Run，任务不能伪装成用户 Echo。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
)

// 任务持久状态机状态。状态迁移必须经过 Store 端口的原子守卫。
const (
	StatusQueued         = "queued"          // 已创建，等待到期领取
	StatusRunning        = "running"         // 已领取，持有租约执行中
	StatusSucceeded      = "succeeded"       // 终态：执行成功
	StatusFailed         = "failed"          // 终态：不可重试失败或重试次数用尽
	StatusRetryScheduled = "retry_scheduled" // 失败但已按策略安排退避重试
	StatusCancelled      = "cancelled"       // 终态：被显式取消
)

// ErrorClass 是任务最近一次失败的持久化错误分类，决定是否允许自动重试。
type ErrorClass string

const (
	ErrorClassNone         ErrorClass = ""                  // 无错误（排队、运行、成功）
	ErrorClassRetryable    ErrorClass = "retryable"         // 显式标记的临时性错误，允许按类型策略自动重试
	ErrorClassNonRetryable ErrorClass = "non_retryable"     // 不可安全重试的错误
	ErrorClassDeadline     ErrorClass = "deadline_exceeded" // 执行超时，副作用未知，不可自动重试
	ErrorClassLeaseLost    ErrorClass = "lease_lost"        // 租约丢失（进程崩溃或续期失败）
	ErrorClassCancelled    ErrorClass = "cancelled"         // 被显式取消
)

var (
	ErrInvalidTask         = errors.New("invalid task")
	ErrInvalidTransition   = errors.New("invalid task state transition")
	ErrTaskNotFound        = errors.New("task not found")
	ErrTaskExists          = errors.New("task already exists")
	ErrTaskTypeUnknown     = errors.New("task type is not registered")
	ErrTaskTypeUnavailable = errors.New("task type is no longer registered")
	ErrInvalidParams       = errors.New("task parameters are invalid")
	ErrInvalidTypeSpec     = errors.New("invalid task type specification")
	ErrDuplicateType       = errors.New("task type is already registered")
	ErrLeaseLost           = errors.New("task execution lease was lost")
)

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

const (
	maxTaskIDBytes  = 128
	maxTypeIDBytes  = 128
	maxParamsBytes  = 64 << 10
	maxAttempts     = 32
	statusListValue = "queued,running,succeeded,failed,retry_scheduled,cancelled"
)

// Task 是持久化的后台任务记录，Go 内核对其状态机拥有权威解释权。
type Task struct {
	AppID          string          // App 隔离边界
	TaskID         string          // 稳定标识，不是显示名
	Type           string          // 封闭任务类型，必须预先注册
	Status         string          // 状态机当前状态
	Attempt        uint32          // 当前尝试序号（从 1 开始）
	MaxAttempts    uint32          // 执行/自动重试次数上限
	Deadline       time.Time       // 任务绝对截止时间
	AvailableAt    time.Time       // 最早可领取时间（定时/延迟执行）
	LeaseToken     string          // 租约令牌，领取时原子生成
	LeaseExpiresAt *time.Time      // 租约到期时间
	IdempotencyKey string          // 幂等键：处理器据此保证副作用可重放安全
	Params         json.RawMessage // 结构化参数（受类型 Schema 约束）
	ErrorClass     ErrorClass      // 最近一次失败的错误分类
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ValidateNewTask 校验一个新任务的结构（不涉及类型注册表；类型与参数 Schema
// 校验由 TypeRegistry 负责）。新任务只能以 queued 状态创建。
func ValidateNewTask(value Task) error {
	if err := appconfig.ValidateAppID(value.AppID); err != nil {
		return errors.Join(ErrInvalidTask, err)
	}
	if len(value.TaskID) == 0 || len(value.TaskID) > maxTaskIDBytes || !taskIDPattern.MatchString(value.TaskID) {
		return fmt.Errorf("%w: 非法 task_id", ErrInvalidTask)
	}
	if len(value.Type) == 0 || len(value.Type) > maxTypeIDBytes {
		return fmt.Errorf("%w: 非法任务类型标识", ErrInvalidTask)
	}
	if value.Status != StatusQueued {
		return fmt.Errorf("%w: 新任务状态必须是 queued", ErrInvalidTask)
	}
	if value.Attempt != 1 {
		return fmt.Errorf("%w: 新任务 attempt 必须是 1", ErrInvalidTask)
	}
	if value.MaxAttempts < 1 || value.MaxAttempts > maxAttempts {
		return fmt.Errorf("%w: max_attempts 必须在 1..%d", ErrInvalidTask, maxAttempts)
	}
	if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: 时间戳不能为零", ErrInvalidTask)
	}
	if value.Deadline.IsZero() || !value.Deadline.After(value.CreatedAt) {
		return fmt.Errorf("%w: deadline 必须晚于 created_at", ErrInvalidTask)
	}
	if value.AvailableAt.IsZero() || value.AvailableAt.Before(value.CreatedAt) {
		return fmt.Errorf("%w: available_at 不能早于 created_at", ErrInvalidTask)
	}
	if !value.Deadline.After(value.AvailableAt) {
		return fmt.Errorf("%w: available_at 必须早于 deadline", ErrInvalidTask)
	}
	if value.LeaseToken != "" || value.LeaseExpiresAt != nil {
		return fmt.Errorf("%w: 新任务不能持有租约", ErrInvalidTask)
	}
	if err := idempotency.ValidateKey(value.IdempotencyKey); err != nil {
		return errors.Join(ErrInvalidTask, err)
	}
	if len(value.Params) == 0 || len(value.Params) > maxParamsBytes || !json.Valid(value.Params) {
		return fmt.Errorf("%w: 参数必须是合法 JSON", ErrInvalidTask)
	}
	if value.ErrorClass != ErrorClassNone {
		return fmt.Errorf("%w: 新任务不能携带错误分类", ErrInvalidTask)
	}
	return nil
}

// retryableError 是显式声明可自动重试的错误包装。
type retryableError struct {
	cause error
}

func (e *retryableError) Error() string { return e.cause.Error() }
func (e *retryableError) Unwrap() error { return e.cause }

// Retryable 将错误显式标记为可安全自动重试的临时性失败。
// 未包装的错误默认视为不可安全重试（失败关闭），避免不安全副作用被自动重复执行。
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryableError{cause: err}
}

// ClassifyFailure 将执行错误映射为持久化的错误分类。超时必须显式判断；
// 可重试性必须通过 Retryable 显式声明，其余错误一律按不可重试处理。
func ClassifyFailure(err error) ErrorClass {
	switch {
	case err == nil:
		return ErrorClassNone
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorClassDeadline
	default:
		var retryable *retryableError
		if errors.As(err, &retryable) {
			return ErrorClassRetryable
		}
		return ErrorClassNonRetryable
	}
}

// Store 是任务持久化端口。所有读写在 SQL 边界强制 App 隔离，
// 状态迁移以租约令牌与租约到期条件做原子守卫，返回稳定错误。
type Store interface {
	// CreateTask 持久化一个已校验的新任务（queued）。
	CreateTask(ctx context.Context, value Task) error
	// ClaimTask 以租约令牌原子领取一个到期任务（queued/retry_scheduled -> running）。
	ClaimTask(ctx context.Context, appID, taskID, leaseToken string, startedAt, leaseExpiresAt time.Time) (Task, error)
	// RenewTaskLease 在当前租约尚未到期时续期。
	RenewTaskLease(ctx context.Context, value Task, renewedAt, leaseExpiresAt time.Time) error
	// CompleteTask 以租约守卫持久化成功终态（running -> succeeded）。
	CompleteTask(ctx context.Context, value Task, completedAt time.Time) error
	// FailTask 以租约守卫持久化失败终态（running -> failed）。
	FailTask(ctx context.Context, value Task, errorClass ErrorClass, completedAt time.Time) error
	// RetryTask 以租约守卫安排退避重试（running -> retry_scheduled，attempt+1）。
	RetryTask(ctx context.Context, value Task, nextAvailableAt time.Time, completedAt time.Time) error
	// CancelQueuedTask 取消排队/等待重试的任务（queued/retry_scheduled -> cancelled）。
	CancelQueuedTask(ctx context.Context, appID, taskID string, completedAt time.Time) (Task, bool, error)
	// CancelRunningTask 由执行者以租约守卫持久化取消（running -> cancelled）。
	CancelRunningTask(ctx context.Context, value Task, completedAt time.Time) error
	// ListDueTasks 返回当前可领取的到期任务（按到期时间排序）。
	ListDueTasks(ctx context.Context, now time.Time, limit int) ([]Task, error)
	// ListDeadTasks 返回租约已到期但仍处于 running 的死亡任务。
	ListDeadTasks(ctx context.Context, now time.Time, limit int) ([]Task, error)
	// RetryDeadTask 将租约已到期的运行中任务安排重试（running+租约过期 -> retry_scheduled，attempt+1）。
	RetryDeadTask(ctx context.Context, value Task, nextAvailableAt time.Time, completedAt time.Time) error
	// FailDeadTask 将租约已到期的运行中任务确定性终结（running+租约过期 -> failed）。
	FailDeadTask(ctx context.Context, value Task, errorClass ErrorClass, completedAt time.Time) error
	// GetTask 读取单个任务。
	GetTask(ctx context.Context, appID, taskID string) (Task, error)
	// ListTasks 按 App 读取任务列表。
	ListTasks(ctx context.Context, appID string, limit int) ([]Task, error)
}
