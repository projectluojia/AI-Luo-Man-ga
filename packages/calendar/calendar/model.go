// Package calendar 是学业校历包的领域模型：权威校历事件的查询载荷与
// 快照集合契约。数据与校巴同构——权威快照由可信集成方导入。
package calendar

import (
	"errors"
	"time"
)

// 快照集合名（namespace calendar/events 下的集合）。
const EventsCollection = "events"

// MaxEvents 是单次查询的事件数上限；MaxRangeDays 是查询窗口宽度上限。
const (
	MaxEvents    = 5000
	MaxRangeDays = 366
)

// DefaultLimit 是未指定 limit 时的返回数。
const DefaultLimit = 100

var errInvalidArgument = errors.New("invalid argument")

// Event 是校历事件文档。
type Event struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Type           string    `json:"type"`
	StartAt        time.Time `json:"start_at"`
	EndAt          time.Time `json:"end_at"`
	Description    string    `json:"description,omitempty"`
	SourceRevision string    `json:"source_revision"`
}

// QueryRequest 按时间窗口查询校历事件：窗口宽度 ≤366 天，limit 0 归一化为
// 100（上限 MaxEvents）。
type QueryRequest struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Limit int       `json:"limit"`
}

func (r *QueryRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = DefaultLimit
	}
	switch {
	case r.From.IsZero() || r.To.IsZero():
		return errInvalidArgument
	case !r.To.After(r.From):
		return errInvalidArgument
	case r.To.Sub(r.From) > MaxRangeDays*24*time.Hour:
		return errInvalidArgument
	case r.Limit < 1 || r.Limit > MaxEvents:
		return errInvalidArgument
	default:
		return nil
	}
}

// Matches 判断事件与查询窗口相交（半开区间）。
func (e Event) Matches(request QueryRequest) bool {
	return !e.EndAt.Before(request.From) && e.StartAt.Before(request.To)
}
