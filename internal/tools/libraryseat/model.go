// Package libraryseat 提供图书馆座位预约的原子 Tool：空间/时段查询与预约状态机。
// 目录快照时间按 UTC 存储；预约日期与时段按 Asia/Shanghai 解释。
package libraryseat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "time/tzdata"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

const (
	// TimezoneName 是图书馆日期/时段的解释时区。持久化时间戳一律为 UTC。
	TimezoneName = "Asia/Shanghai"
	// DemoSource 是非权威演示目录的显式来源标记，不得伪装为智慧珞珈。
	DemoSource = "demo-fixture-not-zhihui-luojia"

	DataStateAuthoritativeFresh   = "authoritative_fresh"
	DataStateDemoNonAuthoritative = "demo_non_authoritative"
	SeatStatusAvailable           = "available"
	SeatStatusReserved            = "reserved"
	ReservationPendingConfirm     = "pending_confirm"
	ReservationConfirmed          = "confirmed"
	ReservationCancelled          = "cancelled"
	ReservationExpired            = "expired"
	ReservationCompleted          = "completed"
	// MaxActiveReservationsPerUser 是同一 App 内每位用户同时持有的 confirmed 预约上限。
	MaxActiveReservationsPerUser = 2
	MaxSpacesLimit               = 50
	MaxSeatsLimit                = 200
	MaxReservationsLimit         = 50
	MaxIDLength                  = 128
	MaxNameLength                = 256
)

var (
	ErrInvalid             = errors.New("library seat request is invalid")
	ErrUserRequired        = errors.New("library seat user_id is required")
	ErrNotFound            = errors.New("library seat resource is not found")
	ErrNotOwner            = errors.New("library seat reservation is not owned by the caller")
	ErrConflict            = errors.New("library seat is already reserved")
	ErrQuotaExceeded       = errors.New("library seat reservation quota exceeded")
	ErrIllegalTransition   = errors.New("library seat reservation state transition is illegal")
	ErrIdempotencyConflict = errors.New("library seat idempotency key conflicts with a different request")
	ErrSpaceRequired       = errors.New("space_id is required")
	ErrDateRequired        = errors.New("date is required")
	ErrSeatRequired        = errors.New("seat_id is required")
	ErrSlotRequired        = errors.New("slot_id is required")
	ErrReservationRequired = errors.New("reservation_id is required")
	ErrInvalidLimit        = errors.New("limit is out of range")
	ErrSlotInPast          = errors.New("library seat slot has already ended")
)

var (
	locationOnce sync.Once
	location     *time.Location
	locationErr  error
)

// TimeLocation 返回图书馆日期/时段使用的 Asia/Shanghai 时区（嵌入 tzdata）。
func TimeLocation() (*time.Location, error) {
	locationOnce.Do(func() {
		location, locationErr = time.LoadLocation(TimezoneName)
	})
	return location, locationErr
}

type SnapshotMetadata struct {
	Revision      string    `json:"source_revision"`
	Source        string    `json:"source"`
	Authoritative bool      `json:"authoritative"`
	Complete      bool      `json:"complete"`
	ImportedAt    time.Time `json:"imported_at"`
	ValidUntil    time.Time `json:"valid_until"`
}

// DataStatus 是工具结果的治理状态：State 加快照元数据。
type DataStatus struct {
	State string `json:"state"`
	SnapshotMetadata
}

// Govern 校验目录快照是否可被业务使用。
// 权威且未过期返回 authoritative_fresh；显式演示来源允许使用但标记为
// demo_non_authoritative；其余非权威来源 fail-closed 为 ErrDataUntrusted。
func (m SnapshotMetadata) Govern(now time.Time) (DataStatus, error) {
	if m.Revision == "" || m.Source == "" || !m.Complete || m.ImportedAt.IsZero() || m.ValidUntil.IsZero() ||
		!m.ValidUntil.After(m.ImportedAt) || now.Before(m.ImportedAt) {
		return DataStatus{}, contracts.ErrDataIncomplete
	}
	if !now.Before(m.ValidUntil) {
		return DataStatus{}, contracts.ErrDataExpired
	}
	if m.Source == DemoSource {
		if m.Authoritative {
			return DataStatus{}, contracts.ErrDataUntrusted
		}
		return DataStatus{State: DataStateDemoNonAuthoritative, SnapshotMetadata: m}, nil
	}
	if m.Authoritative {
		return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMetadata: m}, nil
	}
	return DataStatus{}, contracts.ErrDataUntrusted
}

type Space struct {
	ID             string `json:"space_id"`
	Name           string `json:"name"`
	Campus         string `json:"campus,omitempty"`
	Building       string `json:"building,omitempty"`
	Floor          string `json:"floor,omitempty"`
	SourceRevision string `json:"source_revision"`
}

type Seat struct {
	ID             string `json:"seat_id"`
	SpaceID        string `json:"space_id"`
	Label          string `json:"label"`
	Area           string `json:"area,omitempty"`
	SourceRevision string `json:"source_revision"`
}

type Slot struct {
	ID             string `json:"slot_id"`
	Name           string `json:"name"`
	StartMinute    int    `json:"start_minute"`
	EndMinute      int    `json:"end_minute"`
	SourceRevision string `json:"source_revision"`
}

type SeatAvailability struct {
	SeatID string `json:"seat_id"`
	Label  string `json:"label"`
	Area   string `json:"area,omitempty"`
	SlotID string `json:"slot_id"`
	Status string `json:"status"`
}

type SlotSeats struct {
	Slot  Slot               `json:"slot"`
	Seats []SeatAvailability `json:"seats"`
}

type Reservation struct {
	ID                   string     `json:"reservation_id"`
	AppID                string     `json:"-"`
	UserID               string     `json:"-"`
	SpaceID              string     `json:"space_id"`
	SpaceName            string     `json:"space_name,omitempty"`
	SeatID               string     `json:"seat_id"`
	SeatLabel            string     `json:"seat_label,omitempty"`
	SlotID               string     `json:"slot_id"`
	SlotName             string     `json:"slot_name,omitempty"`
	Date                 string     `json:"date"`
	StartsAt             time.Time  `json:"starts_at"`
	EndsAt               time.Time  `json:"ends_at"`
	Status               string     `json:"status"`
	CatalogRevision      string     `json:"catalog_revision"`
	CatalogSource        string     `json:"catalog_source"`
	CatalogAuthoritative bool       `json:"catalog_authoritative"`
	CreateIdempotencyKey string     `json:"-"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	CancelledAt          *time.Time `json:"cancelled_at,omitempty"`
}

type SpaceListRequest struct {
	Limit int `json:"limit,omitempty"`
}

func (r *SpaceListRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = MaxSpacesLimit
	}
	if r.Limit < 1 || r.Limit > MaxSpacesLimit {
		return ErrInvalidLimit
	}
	return nil
}

type SpaceListResult struct {
	DataStatus DataStatus `json:"data_status"`
	Spaces     []Space    `json:"spaces"`
}

type SpaceSnapshot struct {
	Metadata SnapshotMetadata
	Spaces   []Space
}

type SlotSearchRequest struct {
	SpaceID string `json:"space_id"`
	Date    string `json:"date"`
	SlotID  string `json:"slot_id,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (r *SlotSearchRequest) NormalizeAndValidate() error {
	r.SpaceID = strings.TrimSpace(r.SpaceID)
	r.Date = strings.TrimSpace(r.Date)
	r.SlotID = strings.TrimSpace(r.SlotID)
	if r.Limit == 0 {
		r.Limit = MaxSeatsLimit
	}
	switch {
	case r.SpaceID == "":
		return ErrSpaceRequired
	case !validStableID(r.SpaceID):
		return ErrInvalid
	case r.Date == "":
		return ErrDateRequired
	case r.SlotID != "" && !validStableID(r.SlotID):
		return ErrInvalid
	case r.Limit < 1 || r.Limit > MaxSeatsLimit:
		return ErrInvalidLimit
	}
	if _, err := ParseLibraryDate(r.Date); err != nil {
		return err
	}
	return nil
}

type SlotSearchResult struct {
	DataStatus DataStatus  `json:"data_status"`
	Space      Space       `json:"space"`
	Date       string      `json:"date"`
	Slots      []SlotSeats `json:"slots"`
}

type SeatSnapshot struct {
	Metadata SnapshotMetadata
	Space    Space
	Slots    []Slot
	Seats    []Seat
	Occupied map[string]string
}

type CreateReservationRequest struct {
	SpaceID string `json:"space_id"`
	SeatID  string `json:"seat_id"`
	SlotID  string `json:"slot_id"`
	Date    string `json:"date"`
}

func (r *CreateReservationRequest) NormalizeAndValidate() error {
	r.SpaceID = strings.TrimSpace(r.SpaceID)
	r.SeatID = strings.TrimSpace(r.SeatID)
	r.SlotID = strings.TrimSpace(r.SlotID)
	r.Date = strings.TrimSpace(r.Date)
	switch {
	case r.SpaceID == "":
		return ErrSpaceRequired
	case r.SeatID == "":
		return ErrSeatRequired
	case r.SlotID == "":
		return ErrSlotRequired
	case r.Date == "":
		return ErrDateRequired
	case !validStableID(r.SpaceID) || !validStableID(r.SeatID) || !validStableID(r.SlotID):
		return ErrInvalid
	}
	if _, err := ParseLibraryDate(r.Date); err != nil {
		return err
	}
	return nil
}

type CreateReservationInput struct {
	AppID          string
	UserID         string
	SpaceID        string
	SeatID         string
	SlotID         string
	Date           string
	IdempotencyKey string
}

type CancelReservationInput struct {
	AppID         string
	UserID        string
	ReservationID string
}

type ReservationResult struct {
	DataStatus  DataStatus  `json:"data_status"`
	Reservation Reservation `json:"reservation"`
}

type MineRequest struct {
	Limit int `json:"limit,omitempty"`
}

func (r *MineRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = MaxReservationsLimit
	}
	if r.Limit < 1 || r.Limit > MaxReservationsLimit {
		return ErrInvalidLimit
	}
	return nil
}

type MineResult struct {
	DataStatus   DataStatus    `json:"data_status"`
	Reservations []Reservation `json:"reservations"`
}

// CatalogSnapshot 是一次原子替换的目录快照（空间/座位/时段模板）。
type CatalogSnapshot struct {
	AppID         string
	Revision      string
	Source        string
	Authoritative bool
	Complete      bool
	ImportedAt    time.Time
	ValidUntil    time.Time
	Spaces        []Space
	Seats         []Seat
	Slots         []Slot
}

// Store 是 Go 内核注入的存储端口，实现必须按 app_id 隔离并强制状态机约束。
type Store interface {
	ReplaceCatalog(context.Context, CatalogSnapshot) error
	ListSpaces(context.Context, string, SpaceListRequest) (SpaceSnapshot, error)
	SearchSeats(context.Context, string, SlotSearchRequest) (SeatSnapshot, error)
	CreateReservation(context.Context, CreateReservationInput, time.Time) (Reservation, DataStatus, error)
	CancelReservation(context.Context, CancelReservationInput, time.Time) (Reservation, error)
	ListMyReservations(context.Context, string, string, MineRequest, time.Time) ([]Reservation, SnapshotMetadata, error)
}

// ParseLibraryDate 把 YYYY-MM-DD 解析为 Asia/Shanghai 当日零点。
func ParseLibraryDate(value string) (time.Time, error) {
	location, err := TimeLocation()
	if err != nil {
		return time.Time{}, fmt.Errorf("load library timezone: %w", err)
	}
	if len(value) != 10 {
		return time.Time{}, fmt.Errorf("%w: date must be YYYY-MM-DD in Asia/Shanghai", ErrInvalid)
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, location)
	if err != nil || parsed.Format("2006-01-02") != value {
		return time.Time{}, fmt.Errorf("%w: date must be YYYY-MM-DD in Asia/Shanghai", ErrInvalid)
	}
	return parsed, nil
}

// SlotBounds 根据上海日历日与分钟偏移计算 UTC 起止时间。
func SlotBounds(date string, startMinute, endMinute int) (time.Time, time.Time, error) {
	day, err := ParseLibraryDate(date)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if startMinute < 0 || endMinute > 1440 || endMinute <= startMinute {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: slot minutes", ErrInvalid)
	}
	start := day.Add(time.Duration(startMinute) * time.Minute)
	end := day.Add(time.Duration(endMinute) * time.Minute)
	return start.UTC(), end.UTC(), nil
}

func RequireUser(userID string) error {
	if strings.TrimSpace(userID) == "" {
		return ErrUserRequired
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return ErrUserRequired
	}
	return nil
}

func RequireApp(appID string) error {
	if err := identity.ValidateAppID(appID); err != nil {
		return contracts.ErrDataUnavailable
	}
	return nil
}

func validStableID(value string) bool {
	return len(value) <= MaxIDLength && utf8.ValidString(value) && id.StableMixed.MatchString(value)
}

func validText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum
}

func OccupancyKey(seatID, slotID string) string {
	return seatID + "\x1f" + slotID
}

func ValidateCatalog(snapshot CatalogSnapshot) error {
	if err := identity.ValidateAppID(snapshot.AppID); err != nil {
		return fmt.Errorf("invalid library seat snapshot metadata")
	}
	if !validStableID(snapshot.Revision) || !validText(snapshot.Source, MaxNameLength) || !snapshot.Complete ||
		snapshot.ImportedAt.IsZero() || snapshot.ValidUntil.IsZero() || !snapshot.ValidUntil.After(snapshot.ImportedAt) ||
		len(snapshot.Spaces) > 10_000 || len(snapshot.Seats) > 100_000 || len(snapshot.Slots) > 1_000 ||
		(snapshot.Source == DemoSource && snapshot.Authoritative) {
		return fmt.Errorf("invalid library seat snapshot metadata")
	}
	spaceIDs := make(map[string]struct{}, len(snapshot.Spaces))
	for _, space := range snapshot.Spaces {
		if !validStableID(space.ID) {
			return fmt.Errorf("invalid library space id %q", space.ID)
		}
		if !validText(space.Name, MaxNameLength) || utf8.RuneCountInString(space.Campus) > MaxIDLength ||
			utf8.RuneCountInString(space.Building) > MaxIDLength || utf8.RuneCountInString(space.Floor) > 64 ||
			(space.SourceRevision != "" && space.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid library space %q", space.ID)
		}
		if _, duplicate := spaceIDs[space.ID]; duplicate {
			return fmt.Errorf("duplicate library space %q", space.ID)
		}
		spaceIDs[space.ID] = struct{}{}
	}
	seatIDs := make(map[string]struct{}, len(snapshot.Seats))
	for _, seat := range snapshot.Seats {
		key := seat.SpaceID + "/" + seat.ID
		if !validStableID(seat.ID) || !validStableID(seat.SpaceID) || !validText(seat.Label, MaxIDLength) ||
			utf8.RuneCountInString(seat.Area) > MaxIDLength ||
			(seat.SourceRevision != "" && seat.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid library seat %q", seat.ID)
		}
		if _, exists := spaceIDs[seat.SpaceID]; !exists {
			return fmt.Errorf("library seat %q references unknown space", seat.ID)
		}
		if _, duplicate := seatIDs[key]; duplicate {
			return fmt.Errorf("duplicate library seat %q", key)
		}
		seatIDs[key] = struct{}{}
	}
	slotIDs := make(map[string]struct{}, len(snapshot.Slots))
	for _, slot := range snapshot.Slots {
		if !validStableID(slot.ID) || !validText(slot.Name, MaxNameLength) ||
			slot.StartMinute < 0 || slot.EndMinute > 1440 || slot.EndMinute <= slot.StartMinute ||
			(slot.SourceRevision != "" && slot.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid library slot %q", slot.ID)
		}
		if _, duplicate := slotIDs[slot.ID]; duplicate {
			return fmt.Errorf("duplicate library slot %q", slot.ID)
		}
		slotIDs[slot.ID] = struct{}{}
	}
	return nil
}
