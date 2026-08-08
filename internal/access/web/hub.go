package web

import (
	"sync"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

type eventHub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[echoKey]map[uint64]chan kernelecho.Event
}

type echoKey struct {
	appID  string
	echoID string
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: make(map[echoKey]map[uint64]chan kernelecho.Event)}
}

func (h *eventHub) subscribe(appID, echoID string) (<-chan kernelecho.Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := echoKey{appID: appID, echoID: echoID}
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

func (h *eventHub) publish(event kernelecho.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := echoKey{appID: event.AppID, echoID: event.EchoID}
	for id, channel := range h.subscribers[key] {
		select {
		case channel <- event:
		default:
			close(channel)
			delete(h.subscribers[key], id)
		}
	}
}

func (h *eventHub) finish(appID, echoID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := echoKey{appID: appID, echoID: echoID}
	for id, channel := range h.subscribers[key] {
		close(channel)
		delete(h.subscribers[key], id)
	}
	delete(h.subscribers, key)
}
