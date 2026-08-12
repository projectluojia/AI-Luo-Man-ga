package task

import "time"

// EventType 是后台任务生命周期事件类型，与任务状态迁移一一对应。
// 事件由权威持久状态派生，供将来的 Outbox 投递（通知/审计/推送）消费。
type EventType string

const (
	EventCreated        EventType = "task.created"         // 任务创建（queued）
	EventClaimed        EventType = "task.claimed"         // 任务被领取（running）
	EventSucceeded      EventType = "task.succeeded"       // 执行成功（succeeded）
	EventFailed         EventType = "task.failed"          // 执行失败终态（failed）
	EventRetryScheduled EventType = "task.retry_scheduled" // 已安排退避重试（retry_scheduled）
	EventCancelled      EventType = "task.cancelled"       // 被取消（cancelled）
	EventRecovered      EventType = "task.recovered"       // 死亡任务被恢复（retry_scheduled 或 failed）
)

// Event 描述一次后台任务状态变迁。只携带稳定标识与闭式状态，
// 不携带参数正文等敏感内容；权威来源是持久任务状态，可随时重放。
type Event struct {
	AppID          string     `json:"app_id"`
	TaskID         string     `json:"task_id"`
	Type           EventType  `json:"type"`
	Status         string     `json:"status"`
	Attempt        uint32     `json:"attempt"`
	TaskType       string     `json:"task_type"`
	ErrorClass     ErrorClass `json:"error_class,omitempty"`
	IdempotencyKey string     `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
}
