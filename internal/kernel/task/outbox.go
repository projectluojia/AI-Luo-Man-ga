package task

import (
	"context"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// EventType 是后台任务生命周期事件类型，与任务状态迁移一一对应。
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

// Event 描述一次后台任务状态变迁。事件由权威持久状态派生，只携带
// 稳定标识与闭式状态，不携带参数正文等敏感内容。
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

// EventSink 是 Outbox 事件的投递目标（如 SSE 推送、审计或消息总线适配器）。
type EventSink interface {
	Publish(ctx context.Context, event Event) error
}

// publishEvent 将状态变迁事件放入 Outbox 队列异步投递。
// 投递失败或队列溢出只记录日志并丢弃事件，绝不影响任务状态机；
// 事件的权威来源是持久任务状态，消费方可通过 ListTasks 重放。
func (s *Scheduler) publishEvent(event Event) {
	s.mu.Lock()
	outbox := s.outbox
	s.mu.Unlock()
	if outbox == nil {
		return
	}
	event.CreatedAt = s.now().UTC()
	select {
	case outbox <- event:
	default:
		observe.Warn(context.Background(), "后台任务 Outbox 事件队列已满，事件已丢弃（可从持久任务状态重放）",
			observe.StringAttr("app_id", event.AppID),
			observe.StringAttr("task_id", event.TaskID),
			observe.StringAttr("event_type", string(event.Type)),
		)
	}
}

// outboxLoop 顺序投递 Outbox 队列中的事件；关闭时先有界排空剩余事件再退出。
func (s *Scheduler) outboxLoop() {
	defer s.workerWG.Done()
	for {
		select {
		case <-s.runCtx.Done():
			for {
				select {
				case event := <-s.outbox:
					s.deliverEvent(event)
				default:
					return
				}
			}
		case event := <-s.outbox:
			s.deliverEvent(event)
		}
	}
}

func (s *Scheduler) deliverEvent(event Event) {
	deliveryContext, cancel := context.WithTimeout(context.Background(), terminalWriteTimeout)
	defer cancel()
	if err := s.config.EventSink.Publish(deliveryContext, event); err != nil {
		observe.Error(deliveryContext, "投递后台任务 Outbox 事件失败", err,
			observe.StringAttr("app_id", event.AppID),
			observe.StringAttr("task_id", event.TaskID),
			observe.StringAttr("event_type", string(event.Type)),
		)
	}
}
