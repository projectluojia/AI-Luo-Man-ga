// Package classroom 是空闲教室包的领域模型：快照治理元数据、领域文档与
// 请求校验。数据与校巴同构——权威快照由可信集成方导入，个人日程落在
// 个人作用域文档。
package classroom

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 领域常量（亚洲/上海学时制：日期为本地日期字符串，节次 1-13）。
const (
	AcademicTimezone    = "Asia/Shanghai"
	AcademicDateLayout  = "2006-01-02"
	MinPeriod           = 1
	MaxPeriod           = 13
	DefaultListLimit    = 50
	MaxListLimit        = 100
	StatusScheduled     = "scheduled"
	StatusCancelled     = "cancelled"
	MaxTitleLength      = 256
	MaxQueryLength      = 128
	MaxSchedulesPerUser = 200
)

// 快照集合名（namespace classroom/rooms 下的集合）。
const (
	campusesCollection  = "campuses"
	buildingsCollection = "buildings"
	roomsCollection     = "rooms"
	occupancyCollection = "occupancy"
)

// 个人日程集合名（namespace classroom/schedules，个人作用域）。
const SchedulesCollection = "schedules"

// 稳定错误码是 guestkit 闭式集合的子集；载荷校验错误统一 invalid_argument。
var (
	errInvalidArgument = errors.New("invalid argument")
)

// Campus 是校区文档。
type Campus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SourceRevision string `json:"source_revision"`
}

// Building 是教学楼文档。
type Building struct {
	ID             string `json:"id"`
	CampusID       string `json:"campus_id"`
	Name           string `json:"name"`
	SourceRevision string `json:"source_revision"`
}

// Room 是教室文档。
type Room struct {
	ID             string `json:"id"`
	CampusID       string `json:"campus_id"`
	BuildingID     string `json:"building_id"`
	Name           string `json:"name"`
	Type           string `json:"type,omitempty"`
	Capacity       int    `json:"capacity,omitempty"`
	Floor          string `json:"floor,omitempty"`
	SourceRevision string `json:"source_revision"`
}

// Occupancy 是节次占用文档（room 在指定日期第 N 节被占用）。
type Occupancy struct {
	RoomID         string `json:"room_id"`
	AcademicDate   string `json:"academic_date"`
	Period         int    `json:"period"`
	SourceRevision string `json:"source_revision"`
}

// ScheduleItem 是个人教室日程文档（个人作用域）。
type ScheduleItem struct {
	ScheduleID  string `json:"schedule_id"`
	RoomID      string `json:"room_id"`
	CampusID    string `json:"campus_id"`
	BuildingID  string `json:"building_id"`
	RoomName    string `json:"room_name"`
	Date        string `json:"date"`
	Period      int    `json:"period"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CancelledAt string `json:"cancelled_at,omitempty"`
}

// ---- 快照查询载荷 ----

// RoomsSearchRequest 按学术日期、校区、可选教学楼与节次查询空闲教室。
type RoomsSearchRequest struct {
	Date       string `json:"date"`
	CampusID   string `json:"campus_id"`
	BuildingID string `json:"building_id"`
	Period     int    `json:"period"`
	Limit      int    `json:"limit"`
}

func (r *RoomsSearchRequest) NormalizeAndValidate() error {
	r.CampusID = strings.TrimSpace(r.CampusID)
	r.BuildingID = strings.TrimSpace(r.BuildingID)
	if r.Limit == 0 {
		r.Limit = DefaultListLimit
	}
	if _, err := ParseAcademicDate(r.Date); err != nil {
		return errInvalidArgument
	}
	if r.CampusID == "" || r.Period < MinPeriod || r.Period > MaxPeriod ||
		r.Limit < 1 || r.Limit > MaxListLimit || len(r.CampusID) > 128 || len(r.BuildingID) > 128 {
		return errInvalidArgument
	}
	return nil
}

// CampusListRequest / BuildingListRequest / ScheduleListRequest 的载荷。
type CampusListRequest struct {
	Limit int `json:"limit"`
}

func (r *CampusListRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = DefaultListLimit
	}
	if r.Limit < 1 || r.Limit > MaxListLimit {
		return errInvalidArgument
	}
	return nil
}

type BuildingListRequest struct {
	CampusID string `json:"campus_id"`
	Limit    int    `json:"limit"`
}

func (r *BuildingListRequest) NormalizeAndValidate() error {
	r.CampusID = strings.TrimSpace(r.CampusID)
	if r.Limit == 0 {
		r.Limit = DefaultListLimit
	}
	if r.CampusID == "" || len(r.CampusID) > 128 || r.Limit < 1 || r.Limit > MaxListLimit {
		return errInvalidArgument
	}
	return nil
}

// ScheduleCreateRequest 从教室、学术日期与节次创建个人日程。
type ScheduleCreateRequest struct {
	RoomID string `json:"room_id"`
	Date   string `json:"date"`
	Period int    `json:"period"`
	Title  string `json:"title"`
}

func (r *ScheduleCreateRequest) NormalizeAndValidate() error {
	r.RoomID = strings.TrimSpace(r.RoomID)
	r.Title = strings.TrimSpace(r.Title)
	if r.Title == "" {
		r.Title = DefaultTitle(r.RoomID, r.Date, r.Period)
	}
	if _, err := ParseAcademicDate(r.Date); err != nil {
		return errInvalidArgument
	}
	if r.RoomID == "" || len(r.RoomID) > 128 || r.Period < MinPeriod || r.Period > MaxPeriod ||
		len(r.Title) > MaxTitleLength {
		return errInvalidArgument
	}
	return nil
}

// DefaultTitle 是未提供标题时的日程默认标题。
func DefaultTitle(roomID, date string, period int) string {
	return fmt.Sprintf("自习 %s %s 第%d节", roomID, date, period)
}

type ScheduleListRequest struct {
	Limit int `json:"limit"`
}

func (r *ScheduleListRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = DefaultListLimit
	}
	if r.Limit < 1 || r.Limit > MaxListLimit {
		return errInvalidArgument
	}
	return nil
}

type ScheduleCancelRequest struct {
	ScheduleID string `json:"schedule_id"`
}

func (r *ScheduleCancelRequest) NormalizeAndValidate() error {
	r.ScheduleID = strings.TrimSpace(r.ScheduleID)
	if r.ScheduleID == "" || len(r.ScheduleID) > 128 {
		return errInvalidArgument
	}
	return nil
}

// ParseAcademicDate 解析学术日期（拒绝带时刻的滚动日期）。
func ParseAcademicDate(value string) (time.Time, error) {
	return time.ParseInLocation(AcademicDateLayout, value, academicLocation())
}

func academicLocation() *time.Location {
	location, err := time.LoadLocation(AcademicTimezone)
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}
