// Package app 是学业校历包的协议层：单一 Capability 的载荷分发。读取走
// 系统作用域快照治理（guestkit.GovernedSnapshot），窗口过滤与排序在 guest
// 侧完成。本层不接触 wasmimport，可在原生 go test 下完整测试。
package app

import (
	"encoding/json"
	"sort"

	"github.com/projectluojia/calendar/calendar"
	"github.com/projectluojia/guestkit"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const EventsListCapabilityID = "calendar.events.list"

// NewDispatcher 构造分发器：以 guestkit 共享 Dispatcher 分发（唯一实现，
// 包内只声明能力分发表）。store 为 guestkit.StoreClient（wasip1 内）或测试替身。
func NewDispatcher(client guestkit.Store) *guestkit.Dispatcher {
	return guestkit.NewDispatcher(Handlers(client))
}

// dispatcher 持有存储端口；处理函数由 Handlers 注册到 guestkit.Dispatcher。
type dispatcher struct {
	store guestkit.Store
}

// Handlers 返回能力分发表（src/main.go 装配 guestkit.Run 用）。
func Handlers(client guestkit.Store) map[string]func(json.RawMessage) (any, error) {
	d := &dispatcher{store: client}
	return map[string]func(json.RawMessage) (any, error){
		EventsListCapabilityID: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listEvents) },
	}
}

type eventsListResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Events     []calendar.Event    `json:"events"`
}

// listEvents 治理快照后按窗口过滤事件，按开始时间排序。
func (d *dispatcher) listEvents(request calendar.QueryRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[calendar.Event](d.store, calendar.EventsCollection)
	if err != nil {
		return nil, err
	}
	events := make([]calendar.Event, 0, len(snapshot.Documents))
	for _, item := range snapshot.Documents {
		if item.Matches(request) {
			events = append(events, item)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].StartAt.Before(events[j].StartAt) })
	if len(events) > request.Limit {
		events = events[:request.Limit]
	}
	return eventsListResult{DataStatus: dataStatus, Events: events}, nil
}
