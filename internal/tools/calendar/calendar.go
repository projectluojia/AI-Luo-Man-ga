// Package calendar 是 academiccalendar Tool 的稳定短包名兼容层。
package calendar

import ac "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"

type SnapshotMetadata = ac.SnapshotMetadata
type DataStatus = ac.DataStatus
type Event = ac.Event
type QueryRequest = ac.QueryRequest
type Snapshot = ac.Snapshot
type Store = ac.Store
type SnapshotInput = ac.SnapshotInput
type SnapshotWriter = ac.SnapshotWriter

var (
	ErrInvalid        = ac.ErrInvalid
	ErrQueryRequired  = ac.ErrQueryRequired
	ErrDataIncomplete = ac.ErrDataIncomplete
	ErrDataUntrusted  = ac.ErrDataUntrusted
	ErrDataExpired    = ac.ErrDataExpired
)

const (
	EventsListToolID                  = ac.EventsListToolID
	EventsListCapabilityID            = ac.EventsListCapabilityID
	AcademicCalendarQueryCapabilityID = ac.AcademicCalendarQueryCapabilityID
	CalendarQueryCapabilityID         = ac.CalendarQueryCapabilityID
	InputSchemaJSON                   = ac.InputSchemaJSON
	DataStateAuthoritativeFresh       = ac.DataStateAuthoritativeFresh
)
