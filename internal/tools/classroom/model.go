package classroom

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	// AcademicTimezone 是教务日期与节次边界使用的 IANA 时区。
	// 学术日历日（YYYY-MM-DD）按此时区解释；库内时间戳一律以 UTC 存储。
	AcademicTimezone   = "Asia/Shanghai"
	AcademicDateLayout = "2006-01-02"

	DataStateAuthoritativeFresh = "authoritative_fresh"

	MinPeriod    = 1
	MaxPeriod    = 13
	DefaultLimit = 50
	MaxLimit     = 100

	ScheduleStatusScheduled = "scheduled"
	ScheduleStatusCancelled = "cancelled"

	DemoSource = "demo-fixture-not-zhihui-luojia"
)

var (
	ErrInvalidRequest   = errors.New("classroom request is invalid")
	ErrInvalidDate      = errors.New("academic date must be YYYY-MM-DD in Asia/Shanghai")
	ErrInvalidPeriod    = errors.New("period must be between 1 and 13")
	ErrInvalidLimit     = errors.New("limit must be between 1 and 100")
	ErrCampusRequired   = errors.New("campus_id is required")
	ErrRoomRequired     = errors.New("room_id is required")
	ErrUserRequired     = errors.New("user_id is required")
	ErrNotFound         = errors.New("classroom resource is not found")
	ErrConflict         = errors.New("classroom resource already exists")
	ErrIllegalState     = errors.New("illegal classroom schedule state transition")
	ErrStoreUnavailable = errors.New("classroom store is unavailable")
)

// SnapshotMetadata 描述一次教室快照的来源与新鲜度；Govern 在使用前强制权威/完整/未过期。
type SnapshotMetadata struct {
	Revision      string    `json:"source_revision"`
	Source        string    `json:"source"`
	Authoritative bool      `json:"authoritative"`
	Complete      bool      `json:"complete"`
	ImportedAt    time.Time `json:"imported_at"`
	ValidUntil    time.Time `json:"valid_until"`
}

// DataStatus 是查询结果的治理状态：State 加快照元数据（嵌入展开为扁平 JSON）。
type DataStatus struct {
	State string `json:"state"`
	SnapshotMetadata
}

func (m SnapshotMetadata) Govern(now time.Time) (DataStatus, error) {
	if m.Revision == "" || m.Source == "" || !m.Complete || m.ImportedAt.IsZero() || m.ValidUntil.IsZero() ||
		!m.ValidUntil.After(m.ImportedAt) || now.Before(m.ImportedAt) {
		return DataStatus{}, contracts.ErrDataIncomplete
	}
	if !m.Authoritative {
		return DataStatus{}, contracts.ErrDataUntrusted
	}
	if !now.Before(m.ValidUntil) {
		return DataStatus{}, contracts.ErrDataExpired
	}
	return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMetadata: m}, nil
}

type Campus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SourceRevision string `json:"source_revision"`
}

type Building struct {
	ID             string `json:"id"`
	CampusID       string `json:"campus_id"`
	Name           string `json:"name"`
	SourceRevision string `json:"source_revision"`
}

type Room struct {
	ID             string `json:"id"`
	CampusID       string `json:"campus_id"`
	CampusName     string `json:"campus_name"`
	BuildingID     string `json:"building_id"`
	BuildingName   string `json:"building_name"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Capacity       int    `json:"capacity"`
	Floor          string `json:"floor,omitempty"`
	SourceRevision string `json:"source_revision"`
}

// Occupancy 标记某教室在指定学术日+节次被占用（非空闲）。
type Occupancy struct {
	RoomID         string
	AcademicDate   string
	Period         int
	SourceRevision string
}

type SearchRequest struct {
	Date       string `json:"date"`
	CampusID   string `json:"campus_id"`
	BuildingID string `json:"building_id,omitempty"`
	Period     int    `json:"period"`
	Limit      int    `json:"limit,omitempty"`
}

func (r *SearchRequest) NormalizeAndValidate() error {
	r.Date = strings.TrimSpace(r.Date)
	r.CampusID = strings.TrimSpace(r.CampusID)
	r.BuildingID = strings.TrimSpace(r.BuildingID)
	if r.Limit == 0 {
		r.Limit = DefaultLimit
	}
	if _, err := ParseAcademicDate(r.Date); err != nil {
		return err
	}
	if r.CampusID == "" {
		return ErrCampusRequired
	}
	if !validStableID(r.CampusID) {
		return ErrInvalidRequest
	}
	if r.BuildingID != "" && !validStableID(r.BuildingID) {
		return ErrInvalidRequest
	}
	if r.Period < MinPeriod || r.Period > MaxPeriod {
		return ErrInvalidPeriod
	}
	if r.Limit < 1 || r.Limit > MaxLimit {
		return ErrInvalidLimit
	}
	return nil
}

type SearchResult struct {
	DataStatus DataStatus `json:"data_status"`
	Rooms      []Room     `json:"rooms"`
}

type RoomSnapshot struct {
	Metadata SnapshotMetadata
	Rooms    []Room
}

type CampusListResult struct {
	DataStatus DataStatus `json:"data_status"`
	Campuses   []Campus   `json:"campuses"`
}

type CampusSnapshot struct {
	Metadata SnapshotMetadata
	Campuses []Campus
}

type BuildingListRequest struct {
	CampusID string `json:"campus_id"`
}

func (r *BuildingListRequest) NormalizeAndValidate() error {
	r.CampusID = strings.TrimSpace(r.CampusID)
	if r.CampusID == "" {
		return ErrCampusRequired
	}
	if !validStableID(r.CampusID) {
		return ErrInvalidRequest
	}
	return nil
}

type BuildingListResult struct {
	DataStatus DataStatus `json:"data_status"`
	Buildings  []Building `json:"buildings"`
}

type BuildingSnapshot struct {
	Metadata  SnapshotMetadata
	Buildings []Building
}

type ScheduleItem struct {
	AppID          string     `json:"app_id,omitempty"`
	UserID         string     `json:"user_id,omitempty"`
	ID             string     `json:"schedule_id"`
	RoomID         string     `json:"room_id"`
	CampusID       string     `json:"campus_id"`
	BuildingID     string     `json:"building_id"`
	RoomName       string     `json:"room_name"`
	AcademicDate   string     `json:"date"`
	Period         int        `json:"period"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	SourceRevision string     `json:"source_revision"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CancelledAt    *time.Time `json:"cancelled_at,omitempty"`
}

type ScheduleCreateRequest struct {
	ScheduleID string `json:"schedule_id,omitempty"`
	RoomID     string `json:"room_id"`
	Date       string `json:"date"`
	Period     int    `json:"period"`
	Title      string `json:"title,omitempty"`
}

func (r *ScheduleCreateRequest) NormalizeAndValidate() error {
	r.ScheduleID = strings.TrimSpace(r.ScheduleID)
	r.RoomID = strings.TrimSpace(r.RoomID)
	r.Date = strings.TrimSpace(r.Date)
	r.Title = strings.TrimSpace(r.Title)
	if r.RoomID == "" {
		return ErrRoomRequired
	}
	if !validStableID(r.RoomID) {
		return ErrInvalidRequest
	}
	if r.ScheduleID != "" && !validStableID(r.ScheduleID) {
		return ErrInvalidRequest
	}
	if _, err := ParseAcademicDate(r.Date); err != nil {
		return err
	}
	if r.Period < MinPeriod || r.Period > MaxPeriod {
		return ErrInvalidPeriod
	}
	if len(r.Title) > 256 {
		return ErrInvalidRequest
	}
	return nil
}

type ScheduleCreateResult struct {
	Schedule ScheduleItem `json:"schedule"`
}

type ScheduleListRequest struct {
	Status string `json:"status,omitempty"`
	Date   string `json:"date,omitempty"`
}

func (r *ScheduleListRequest) NormalizeAndValidate() error {
	r.Status = strings.TrimSpace(r.Status)
	r.Date = strings.TrimSpace(r.Date)
	if r.Status != "" && r.Status != ScheduleStatusScheduled && r.Status != ScheduleStatusCancelled {
		return ErrInvalidRequest
	}
	if r.Date != "" {
		if _, err := ParseAcademicDate(r.Date); err != nil {
			return err
		}
	}
	return nil
}

type ScheduleListResult struct {
	Schedules []ScheduleItem `json:"schedules"`
}

type ScheduleCancelRequest struct {
	ScheduleID string `json:"schedule_id"`
}

func (r *ScheduleCancelRequest) NormalizeAndValidate() error {
	r.ScheduleID = strings.TrimSpace(r.ScheduleID)
	if r.ScheduleID == "" || !validStableID(r.ScheduleID) {
		return ErrInvalidRequest
	}
	return nil
}

type ScheduleCancelResult struct {
	Schedule ScheduleItem `json:"schedule"`
}

// Store 是 Go 内核注入的教室存储端口。查询与日程写入都必须按 app_id 隔离。
type Store interface {
	SearchRooms(context.Context, string, SearchRequest) (RoomSnapshot, error)
	ListCampuses(context.Context, string) (CampusSnapshot, error)
	ListBuildings(context.Context, string, BuildingListRequest) (BuildingSnapshot, error)
	GetRoom(context.Context, string, string) (Room, SnapshotMetadata, error)
	CreateSchedule(context.Context, ScheduleItem) (ScheduleItem, error)
	ListSchedules(context.Context, string, string, ScheduleListRequest) ([]ScheduleItem, error)
	CancelSchedule(context.Context, string, string, string, time.Time) (ScheduleItem, error)
}

// ParseAcademicDate 把 YYYY-MM-DD 解析为 Asia/Shanghai 日历日的零点。
// 拒绝 Go time.Parse 会滚动的非法日期（如 2026-02-31）。
func ParseAcademicDate(value string) (time.Time, error) {
	location, err := time.LoadLocation(AcademicTimezone)
	if err != nil {
		return time.Time{}, err
	}
	parsed, err := time.ParseInLocation(AcademicDateLayout, value, location)
	if err != nil || parsed.Format(AcademicDateLayout) != value {
		return time.Time{}, ErrInvalidDate
	}
	return parsed, nil
}

func RequireUser(request contracts.RequestContext) error {
	if request.UserID == "" {
		return errors.Join(registry.ErrSchemaValidation, ErrUserRequired)
	}
	if err := identity.ValidateUserID(request.UserID); err != nil {
		return errors.Join(registry.ErrSchemaValidation, ErrUserRequired)
	}
	return nil
}

func validStableID(value string) bool {
	return value != "" && len(value) <= 128 && id.StableMixed.MatchString(value)
}
