// eventhub.go 提供按 App+Echo 键控的 SSE 事件订阅分发：Web 与平台适配器
// （QQ 等）共享同一个实例，内核 Run 事件经 Publish 广播给所有订阅者，
// Finish 在 Run 结束时关闭对应 Echo 的全部订阅。
package access

import (
	"sync"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

type eventHubKey struct {
	appID  string
	echoID string
}

// EventHub 是 Echo 事件的进程内发布/订阅中心（不持久化；持久化事件由
// 存储层承担，断线重放走 GetEcho）。
type EventHub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[eventHubKey]map[uint64]chan kernelecho.Event
}

// NewEventHub 构造事件订阅中心。
func NewEventHub() *EventHub {
	return &EventHub{subscribers: make(map[eventHubKey]map[uint64]chan kernelecho.Event)}
}

// Subscribe 订阅指定 Echo 的实时事件；返回的 channel 在 Finish 时关闭，
// 调用方必须调用返回的取消函数释放订阅。
func (h *EventHub) Subscribe(appID, echoID string) (<-chan kernelecho.Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := eventHubKey{appID: appID, echoID: echoID}
	h.nextID++
	id := h.nextID
	channel := make(chan kernelecho.Event, 64)
	if h.subscribers[key] == nil {
		h.subscribers[key] = make(map[uint64]chan kernelecho.Event)
	}
	h.subscribers[key][id] = channel
	return channel, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if subscribers := h.subscribers[key]; subscribers != nil {
			if current, ok := subscribers[id]; ok {
				delete(subscribers, id)
				close(current)
			}
			if len(subscribers) == 0 {
				delete(h.subscribers, key)
			}
		}
	}
}

// Publish 广播一个 Echo 事件；慢订阅者（缓冲已满）被直接断开，不阻塞发布。
func (h *EventHub) Publish(event kernelecho.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := eventHubKey{appID: event.AppID, echoID: event.EchoID}
	for id, channel := range h.subscribers[key] {
		select {
		case channel <- event:
		default:
			close(channel)
			delete(h.subscribers[key], id)
		}
	}
}

// Finish 关闭指定 Echo 的全部订阅，标记运行结束。
func (h *EventHub) Finish(appID, echoID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := eventHubKey{appID: appID, echoID: echoID}
	for id, channel := range h.subscribers[key] {
		close(channel)
		delete(h.subscribers[key], id)
	}
	delete(h.subscribers, key)
}
