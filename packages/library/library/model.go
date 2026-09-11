// Package library 是图书馆座位预约包的领域模型：快照集合、个人预约状态机
// 与请求校验。目录（空间/座位/时段/占用）是权威快照数据；预约落在个人
// 作用域文档，UserID 由宿主从治理上下文注入。
package library

import (
	"errors"
	"strings"
	"time"
)

// 领域常量（日期/时段按亚洲/上海解释；持久化时间戳一律 UTC RFC3339）。
const (
	AcademicTimezone   = "Asia/Shanghai"
	AcademicDateLayout = "2006-01-02"
	SeatAvailable      = "available"
	SeatReserved       = "reserved"
	// 预约状态机：confirmed → cancelled（本人取消）/ expired（时段结束，读取期推导）。
	ReservationConfirmed = "confirmed"
	ReservationCancelled = "cancelled"
	ReservationExpired   = "expired"
	// MaxActiveReservationsPerUser 是同一用户同时持有的 confirmed 预约上限。
	MaxActiveReservationsPerUser = 2
	DefaultSpacesLimit           = 50
	MaxSpacesLimit               = 50
	DefaultSeatsLimit            = 200
	MaxSeatsLimit                = 200
	DefaultReservationsLimit     = 50
	MaxReservationsLimit         = 50
	MaxIDLength                  = 128
	MaxNameLength                = 256
	MaxMinutes                   = 1440
)

// 快照集合名（namespace library/seats 下的系统作用域集合）。
const (
	SpacesCollection    = "spaces"
	SeatsCollection     = "seats"
	SlotsCollection     = "slots"
	OccupancyCollection = "occupancy"
)

// ReservationsCollection 是个人预约集合（个人作用域）。
const ReservationsCollection = "reservations"

var errInvalidArgument = errors.New("invalid argument")

// Space 是空间文档。
type Space struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Campus         string `json:"campus,omitempty"`
	Building       string `json:"building,omitempty"`
	Floor          string `json:"floor,omitempty"`
	SourceRevision string `json:"source_revision"`
}

// Seat 是座位文档。
type Seat struct {
	ID             string `json:"id"`
	SpaceID        string `json:"space_id"`
	Label          string `json:"label"`
	Area           string `json:"area,omitempty"`
	SourceRevision string `json:"source_revision"`
}

// Slot 是时段模板文档（分钟偏移按亚洲/上海日历日解释）。
type Slot struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	StartMinute    int    `json:"start_minute"`
	EndMinute      int    `json:"end_minute"`
	SourceRevision string `json:"source_revision"`
}

// Occupancy 是占用文档（seat 在指定日期的指定时段被占用）。
type Occupancy struct {
	SeatID         string `json:"seat_id"`
	SlotID         string `json:"slot_id"`
	Date           string `json:"date"`
	SourceRevision string `json:"source_revision"`
}

// Reservation 是个人预约文档（个人作用域）。
type Reservation struct {
	ReservationID string `json:"reservation_id"`
	SpaceID       string `json:"space_id"`
	SpaceName     string `json:"space_name,omitempty"`
	SeatID        string `json:"seat_id"`
	SeatLabel     string `json:"seat_label,omitempty"`
	SlotID        string `json:"slot_id"`
	SlotName      string `json:"slot_name,omitempty"`
	Date          string `json:"date"`
	StartsAt      string `json:"starts_at"`
	EndsAt        string `json:"ends_at"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	CancelledAt   string `json:"cancelled_at,omitempty"`
}

// EffectiveStatus 读取期推导状态：confirmed 且时段已结束视为 expired。
func (r Reservation) EffectiveStatus(now time.Time) string {
	if r.Status != ReservationConfirmed {
		return r.Status
	}
	endsAt, err := time.Parse(time.RFC3339, r.EndsAt)
	if err != nil || now.After(endsAt) {
		return ReservationExpired
	}
	return r.Status
}

// ---- 快照查询载荷 ----

type SpacesListRequest struct {
	Limit int `json:"limit"`
}

func (r *SpacesListRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = DefaultSpacesLimit
	}
	if r.Limit < 1 || r.Limit > MaxSpacesLimit {
		return errInvalidArgument
	}
	return nil
}

// SlotSearchRequest 按空间、日期与可选时段查座位可用性。
type SlotSearchRequest struct {
	SpaceID string `json:"space_id"`
	Date    string `json:"date"`
	SlotID  string `json:"slot_id"`
	Limit   int    `json:"limit"`
}

func (r *SlotSearchRequest) NormalizeAndValidate() error {
	r.SpaceID = strings.TrimSpace(r.SpaceID)
	r.Date = strings.TrimSpace(r.Date)
	r.SlotID = strings.TrimSpace(r.SlotID)
	if r.Limit == 0 {
		r.Limit = DefaultSeatsLimit
	}
	if _, err := ParseAcademicDate(r.Date); err != nil {
		return errInvalidArgument
	}
	if r.SpaceID == "" || len(r.SpaceID) > MaxIDLength || len(r.SlotID) > MaxIDLength ||
		r.Limit < 1 || r.Limit > MaxSeatsLimit {
		return errInvalidArgument
	}
	return nil
}

// ---- 预约载荷 ----

type ReservationCreateRequest struct {
	SpaceID string `json:"space_id"`
	SeatID  string `json:"seat_id"`
	SlotID  string `json:"slot_id"`
	Date    string `json:"date"`
}

func (r *ReservationCreateRequest) NormalizeAndValidate() error {
	r.SpaceID = strings.TrimSpace(r.SpaceID)
	r.SeatID = strings.TrimSpace(r.SeatID)
	r.SlotID = strings.TrimSpace(r.SlotID)
	r.Date = strings.TrimSpace(r.Date)
	if _, err := ParseAcademicDate(r.Date); err != nil {
		return errInvalidArgument
	}
	if r.SpaceID == "" || r.SeatID == "" || r.SlotID == "" ||
		len(r.SpaceID) > MaxIDLength || len(r.SeatID) > MaxIDLength || len(r.SlotID) > MaxIDLength {
		return errInvalidArgument
	}
	return nil
}

type ReservationCancelRequest struct {
	ReservationID string `json:"reservation_id"`
}

func (r *ReservationCancelRequest) NormalizeAndValidate() error {
	r.ReservationID = strings.TrimSpace(r.ReservationID)
	if r.ReservationID == "" || len(r.ReservationID) > MaxIDLength {
		return errInvalidArgument
	}
	return nil
}

type ReservationListRequest struct {
	Limit int `json:"limit"`
}

func (r *ReservationListRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = DefaultReservationsLimit
	}
	if r.Limit < 1 || r.Limit > MaxReservationsLimit {
		return errInvalidArgument
	}
	return nil
}

// ParseAcademicDate 解析学术日期（拒绝带时刻的滚动日期）。
func ParseAcademicDate(value string) (time.Time, error) {
	if len(value) != 10 {
		return time.Time{}, errInvalidArgument
	}
	parsed, err := time.ParseInLocation(AcademicDateLayout, value, academicLocation())
	if err != nil || parsed.Format(AcademicDateLayout) != value {
		return time.Time{}, errInvalidArgument
	}
	return parsed, nil
}

// SlotBounds 计算指定日期时段的 UTC 起止时间（亚洲/上海日历日）。
func SlotBounds(date string, slot Slot) (time.Time, time.Time, error) {
	day, err := ParseAcademicDate(date)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if slot.StartMinute < 0 || slot.EndMinute > MaxMinutes || slot.EndMinute <= slot.StartMinute {
		return time.Time{}, time.Time{}, errInvalidArgument
	}
	return day.Add(time.Duration(slot.StartMinute) * time.Minute).UTC(),
		day.Add(time.Duration(slot.EndMinute) * time.Minute).UTC(), nil
}

func academicLocation() *time.Location {
	location, err := time.LoadLocation(AcademicTimezone)
	if err != nil {
		// 亚洲/上海无夏令时，固定东八区与 IANA 数据等价（wasm 内嵌 tzdata 体积代价高）。
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}
