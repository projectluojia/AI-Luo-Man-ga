// Package sports 提供运动场馆预约的原子 Tool 模型与存储端口。
// 官方智慧珞珈接口未授权：本包只消费 Go 统一存储中的快照，不发起校方 HTTP。
package sports

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

const (
	StatusConfirmed               = "confirmed"
	StatusCancelled               = "cancelled"
	StatusExpired                 = "expired"
	StatusRejectedOverQuota       = "rejected_over_quota"
	DataStateAuthoritativeFresh   = "authoritative_fresh"
	DataStateDemoNonAuthoritative = "demo_non_authoritative"
	MaxCount                      = 16
	MaxTextChars                  = 256
	MaxHeaderPurposeChars         = 256
	MaxUserAgentChars             = 256
	shanghaiLocationName          = "Asia/Shanghai"
)

var (
	ErrInvalid               = errors.New("invalid sports reservation input")
	ErrNotFound              = errors.New("sports reservation resource not found")
	ErrConflict              = errors.New("sports reservation already exists")
	ErrUserRequired          = errors.New("sports reservation user is required")
	ErrOverQuota             = contracts.ErrQuotaExceeded
	ErrDelegatedAuthRequired = contracts.ErrDelegatedAuthRequired
	ErrSlotExpired           = errors.New("sports slot has already ended")
	ErrNotCancellable        = errors.New("sports reservation cannot be cancelled")
)

// SnapshotMetadata 是场馆快照治理元数据，字段与校巴快照对齐。
type SnapshotMetadata struct {
	Revision      string    `json:"source_revision"`
	Source        string    `json:"source"`
	Authoritative bool      `json:"authoritative"`
	Complete      bool      `json:"complete"`
	ImportedAt    time.Time `json:"imported_at"`
	ValidUntil    time.Time `json:"valid_until"`
}

// DataStatus 是工具结果的治理状态：State 加快照元数据（嵌入展开为扁平 JSON）。
type DataStatus struct {
	State string `json:"state"`
	SnapshotMetadata
}

func (m SnapshotMetadata) incomplete(now time.Time) bool {
	return m.Revision == "" || m.Source == "" || !m.Complete || m.ImportedAt.IsZero() || m.ValidUntil.IsZero() ||
		!m.ValidUntil.After(m.ImportedAt) || now.Before(m.ImportedAt)
}

// Govern 按校巴同一规则 fail-closed：不完整、非权威或过期数据不得当作当前事实。
func (m SnapshotMetadata) Govern(now time.Time) (DataStatus, error) {
	if m.incomplete(now) {
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

// DemoStatus 允许返回显式非权威的演示状态：完整性/有效期仍 fail-closed。
func (m SnapshotMetadata) DemoStatus(now time.Time) (DataStatus, error) {
	if m.incomplete(now) {
		return DataStatus{}, contracts.ErrDataIncomplete
	}
	if !now.Before(m.ValidUntil) {
		return DataStatus{}, contracts.ErrDataExpired
	}
	if m.Authoritative {
		return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMetadata: m}, nil
	}
	return DataStatus{State: DataStateDemoNonAuthoritative, SnapshotMetadata: m}, nil
}

type Venue struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Campus         string `json:"campus,omitempty"`
	Address        string `json:"address,omitempty"`
	SourceRevision string `json:"source_revision"`
}

type Project struct {
	ID             string `json:"id"`
	VenueID        string `json:"venue_id"`
	Name           string `json:"name"`
	SourceRevision string `json:"source_revision"`
}

type Slot struct {
	ID             string    `json:"id"`
	VenueID        string    `json:"venue_id"`
	ProjectID      string    `json:"project_id"`
	Date           string    `json:"date"`
	StartAt        time.Time `json:"start_at"`
	EndAt          time.Time `json:"end_at"`
	Capacity       int       `json:"capacity"`
	RemainingQuota int       `json:"remaining_quota"`
	SourceRevision string    `json:"source_revision"`
}

type RequiredHeader struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

type WebViewDescriptor struct {
	EntryURL              string           `json:"entry_url"`
	RequiredUserAgent     string           `json:"required_user_agent"`
	RequiredHeaders       []RequiredHeader `json:"required_headers"`
	RequiresDelegatedAuth bool             `json:"requires_delegated_auth"`
	SourceRevision        string           `json:"source_revision,omitempty"`
}

type Reservation struct {
	AppID          string     `json:"-"`
	UserID         string     `json:"-"`
	ID             string     `json:"id"`
	VenueID        string     `json:"venue_id"`
	ProjectID      string     `json:"project_id"`
	SlotID         string     `json:"slot_id"`
	VenueName      string     `json:"venue_name,omitempty"`
	ProjectName    string     `json:"project_name,omitempty"`
	Count          int        `json:"count"`
	Status         string     `json:"status"`
	StartAt        time.Time  `json:"start_at"`
	EndAt          time.Time  `json:"end_at"`
	SourceRevision string     `json:"source_revision"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CancelledAt    *time.Time `json:"cancelled_at,omitempty"`
}

type ScheduleItem struct {
	AppID         string    `json:"-"`
	UserID        string    `json:"-"`
	ID            string    `json:"id"`
	ReservationID string    `json:"reservation_id"`
	Title         string    `json:"title"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at"`
	CreatedAt     time.Time `json:"created_at"`
}

type VenueListRequest struct{}

type VenueListResult struct {
	DataStatus DataStatus `json:"data_status"`
	Venues     []Venue    `json:"venues"`
}

type VenueSnapshot struct {
	Metadata SnapshotMetadata
	Venues   []Venue
}

type ProjectListRequest struct {
	VenueID string `json:"venue_id"`
}

func (r *ProjectListRequest) NormalizeAndValidate() error {
	r.VenueID = strings.TrimSpace(r.VenueID)
	if !validStableID(r.VenueID) {
		return ErrInvalid
	}
	return nil
}

type ProjectListResult struct {
	DataStatus DataStatus `json:"data_status"`
	Projects   []Project  `json:"projects"`
}

type ProjectSnapshot struct {
	Metadata SnapshotMetadata
	Projects []Project
}

type SlotSearchRequest struct {
	VenueID   string `json:"venue_id"`
	ProjectID string `json:"project_id"`
	Date      string `json:"date"`
}

func (r *SlotSearchRequest) NormalizeAndValidate() error {
	r.VenueID = strings.TrimSpace(r.VenueID)
	r.ProjectID = strings.TrimSpace(r.ProjectID)
	r.Date = strings.TrimSpace(r.Date)
	if !validStableID(r.VenueID) || !validStableID(r.ProjectID) || !validSlotDate(r.Date) {
		return ErrInvalid
	}
	return nil
}

type SlotSearchResult struct {
	DataStatus DataStatus `json:"data_status"`
	Slots      []Slot     `json:"slots"`
}

type SlotSnapshot struct {
	Metadata SnapshotMetadata
	Slots    []Slot
}

type CreateReservationRequest struct {
	VenueID   string `json:"venue_id"`
	ProjectID string `json:"project_id"`
	SlotID    string `json:"slot_id"`
	Count     int    `json:"count"`
}

func (r *CreateReservationRequest) NormalizeAndValidate() error {
	r.VenueID = strings.TrimSpace(r.VenueID)
	r.ProjectID = strings.TrimSpace(r.ProjectID)
	r.SlotID = strings.TrimSpace(r.SlotID)
	if r.Count == 0 {
		r.Count = 1
	}
	if !validStableID(r.VenueID) || !validStableID(r.ProjectID) || !validStableID(r.SlotID) ||
		r.Count < 1 || r.Count > MaxCount {
		return ErrInvalid
	}
	return nil
}

type CancelReservationRequest struct {
	ReservationID string `json:"reservation_id"`
}

func (r *CancelReservationRequest) NormalizeAndValidate() error {
	r.ReservationID = strings.TrimSpace(r.ReservationID)
	if !validStableID(r.ReservationID) {
		return ErrInvalid
	}
	return nil
}

type ReservationResult struct {
	DataStatus  DataStatus  `json:"data_status"`
	Reservation Reservation `json:"reservation"`
}

type ReservationListResult struct {
	DataStatus   DataStatus    `json:"data_status"`
	Reservations []Reservation `json:"reservations"`
}

type ReservationListSnapshot struct {
	Metadata     SnapshotMetadata
	Reservations []Reservation
}

type WebViewResult struct {
	DataStatus            DataStatus       `json:"data_status"`
	EntryURL              string           `json:"entry_url"`
	RequiredUserAgent     string           `json:"required_user_agent"`
	RequiredHeaders       []RequiredHeader `json:"required_headers"`
	RequiresDelegatedAuth bool             `json:"requires_delegated_auth"`
}

type WebViewSnapshot struct {
	Metadata   SnapshotMetadata
	Descriptor WebViewDescriptor
}

type AddScheduleRequest struct {
	ReservationID string `json:"reservation_id"`
}

func (r *AddScheduleRequest) NormalizeAndValidate() error {
	r.ReservationID = strings.TrimSpace(r.ReservationID)
	if !validStableID(r.ReservationID) {
		return ErrInvalid
	}
	return nil
}

type ScheduleResult struct {
	DataStatus DataStatus   `json:"data_status"`
	Schedule   ScheduleItem `json:"schedule"`
}

type CreateReservationInput struct {
	AppID            string
	UserID           string
	VenueID          string
	ProjectID        string
	SlotID           string
	Count            int
	Now              time.Time
	ExpectedRevision string
}

type CancelReservationInput struct {
	AppID         string
	UserID        string
	ReservationID string
	Now           time.Time
}

type AddScheduleInput struct {
	AppID         string
	UserID        string
	ReservationID string
	Now           time.Time
}

// CatalogSnapshot 是一次原子替换的场馆目录快照；配额初值来自时段 capacity。
type CatalogSnapshot struct {
	AppID         string
	Revision      string
	Source        string
	Authoritative bool
	Complete      bool
	ImportedAt    time.Time
	ValidUntil    time.Time
	Venues        []Venue
	Projects      []Project
	Slots         []Slot
	WebView       WebViewDescriptor
}

// Store 是 Go 内核注入的存储端口。实现必须在 SQL 边界强制 App 隔离。
type Store interface {
	ReplaceSportsSnapshot(context.Context, CatalogSnapshot) error
	ListVenues(context.Context, string) (VenueSnapshot, error)
	ListProjects(context.Context, string, ProjectListRequest) (ProjectSnapshot, error)
	SearchSlots(context.Context, string, SlotSearchRequest) (SlotSnapshot, error)
	CreateReservation(context.Context, CreateReservationInput) (Reservation, SnapshotMetadata, error)
	CancelReservation(context.Context, CancelReservationInput) (Reservation, SnapshotMetadata, error)
	ListMyReservations(context.Context, string, string, time.Time) (ReservationListSnapshot, error)
	GetWebViewDescriptor(context.Context, string) (WebViewSnapshot, error)
	AddScheduleItem(context.Context, AddScheduleInput) (ScheduleItem, SnapshotMetadata, error)
}

func validStableID(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && id.StableMixed.MatchString(value)
}

func validDisplayText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum &&
		!strings.ContainsAny(value, "\r\n\x00")
}

func validSlotDate(value string) bool {
	if len(value) != 10 {
		return false
	}
	_, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	return err == nil
}

func validHTTPSURL(value string) bool {
	if !validDisplayText(value, 512) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return parsed.Fragment == ""
}

func validHeaderName(value string) bool {
	if value == "" || len(value) > 64 || strings.ContainsAny(value, "\r\n\x00:") {
		return false
	}
	for _, r := range value {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func NormalizeWebViewDescriptor(value WebViewDescriptor) (WebViewDescriptor, error) {
	value.EntryURL = strings.TrimSpace(value.EntryURL)
	value.RequiredUserAgent = strings.TrimSpace(value.RequiredUserAgent)
	value.SourceRevision = strings.TrimSpace(value.SourceRevision)
	if !validHTTPSURL(value.EntryURL) || !validDisplayText(value.RequiredUserAgent, MaxUserAgentChars) {
		return WebViewDescriptor{}, ErrInvalid
	}
	if len(value.RequiredHeaders) > 16 {
		return WebViewDescriptor{}, ErrInvalid
	}
	normalized := make([]RequiredHeader, 0, len(value.RequiredHeaders))
	seen := make(map[string]struct{}, len(value.RequiredHeaders))
	for _, header := range value.RequiredHeaders {
		header.Name = strings.TrimSpace(header.Name)
		header.Purpose = strings.TrimSpace(header.Purpose)
		lower := strings.ToLower(header.Name)
		if !validHeaderName(header.Name) || !validDisplayText(header.Purpose, MaxHeaderPurposeChars) {
			return WebViewDescriptor{}, ErrInvalid
		}
		if lower == "cookie" || lower == "authorization" || strings.Contains(lower, "token") {
			return WebViewDescriptor{}, ErrInvalid
		}
		if _, duplicate := seen[lower]; duplicate {
			return WebViewDescriptor{}, ErrInvalid
		}
		seen[lower] = struct{}{}
		normalized = append(normalized, header)
	}
	if normalized == nil {
		normalized = []RequiredHeader{}
	}
	value.RequiredHeaders = normalized
	return value, nil
}

func RequireUser(request contracts.RequestContext) error {
	if request.UserID == "" {
		return ErrUserRequired
	}
	if err := identity.ValidateUserID(request.UserID); err != nil {
		return ErrUserRequired
	}
	return nil
}

func ShanghaiLocation() (*time.Location, error) {
	return time.LoadLocation(shanghaiLocationName)
}

func SlotDateInShanghai(value time.Time) (string, error) {
	location, err := ShanghaiLocation()
	if err != nil {
		return "", err
	}
	return value.In(location).Format("2006-01-02"), nil
}
