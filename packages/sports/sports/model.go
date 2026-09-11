// Package sports 是运动场馆预约包的领域模型：8 个 Capability 的文档、请求
// 与校验。纯 stdlib，无内核依赖——包将独立 repo 化。
//
// 快照治理复用 guestkit（与 classroom/library 同一规则）：不完整、非权威或
// 过期的快照 fail-closed。预约与日程是个人作用域文档；跨用户占用与配额由
// 宿主侧快照（remaining_quota / occupancy）承载，导入方在预约生效时写入。
package sports

import (
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// errInvalidArgument 是请求校验失败的包内哨兵：app 层映射为稳定错误码。
var errInvalidArgument = errors.New("invalid argument")

// 状态与上限常量。
const (
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"

	MaxCount              = 16
	MaxIDLength           = 128
	MaxTextChars          = 256
	MaxURLChars           = 512
	MaxHeaderNameChars    = 64
	MaxHeaders            = 16
	MaxUserAgentChars     = 256
	MaxHeaderPurposeChars = 256

	AcademicTimezone         = "Asia/Shanghai"
	AcademicDateLayout       = "2006-01-02"
	DefaultVenuesLimit       = 50
	MaxVenuesLimit           = 50
	DefaultSlotsLimit        = 100
	MaxSlotsLimit            = 100
	DefaultReservationsLimit = 50
	MaxReservationsLimit     = 50
)

// 集合名（系统作用域快照 + 个人作用域文档）。
const (
	VenuesCollection       = "venues"
	ProjectsCollection     = "projects"
	SlotsCollection        = "slots"
	WebviewCollection      = "webview"
	ReservationsCollection = "reservations"
	ScheduleCollection     = "schedule"
)

// 文档：场馆 / 项目 / 时段（系统作用域快照）。
type (
	// Venue 是运动场馆。
	Venue struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		Campus         string `json:"campus,omitempty"`
		Address        string `json:"address,omitempty"`
		SourceRevision string `json:"source_revision"`
	}

	// Project 是场馆下的运动项目。
	Project struct {
		ID             string `json:"id"`
		VenueID        string `json:"venue_id"`
		Name           string `json:"name"`
		SourceRevision string `json:"source_revision"`
	}

	// Slot 是可预约时段：容量与剩余配额由导入方维护（预约生效时扣减）。
	Slot struct {
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

	// RequiredHeader 是 WebView 会话要求的一个请求头（名称 + 用途）。
	RequiredHeader struct {
		Name    string `json:"name"`
		Purpose string `json:"purpose"`
	}

	// WebViewDescriptor 是运动订单 WebView 会话描述符：不含任何凭据，
	// 只有入口 URL 与 UA/头要求。
	WebViewDescriptor struct {
		EntryURL              string           `json:"entry_url"`
		RequiredUserAgent     string           `json:"required_user_agent"`
		RequiredHeaders       []RequiredHeader `json:"required_headers"`
		RequiresDelegatedAuth bool             `json:"requires_delegated_auth"`
		SourceRevision        string           `json:"source_revision,omitempty"`
	}

	// Reservation 是用户预约（个人作用域文档）。
	Reservation struct {
		ReservationID string `json:"reservation_id"`
		VenueID       string `json:"venue_id"`
		ProjectID     string `json:"project_id"`
		SlotID        string `json:"slot_id"`
		VenueName     string `json:"venue_name,omitempty"`
		ProjectName   string `json:"project_name,omitempty"`
		Count         int    `json:"count"`
		Status        string `json:"status"`
		StartsAt      string `json:"starts_at"`
		EndsAt        string `json:"ends_at"`
		CreatedAt     string `json:"created_at"`
		UpdatedAt     string `json:"updated_at"`
		CancelledAt   string `json:"cancelled_at,omitempty"`
	}

	// ScheduleItem 是预约对应的本地日程（个人作用域文档，每预约一条）。
	ScheduleItem struct {
		ScheduleID    string `json:"schedule_id"`
		ReservationID string `json:"reservation_id"`
		Title         string `json:"title"`
		StartsAt      string `json:"starts_at"`
		EndsAt        string `json:"ends_at"`
		CreatedAt     string `json:"created_at"`
	}
)

// EffectiveStatus 推导有效状态：confirmed 且时段已过 → expired。
func (r Reservation) EffectiveStatus(now time.Time) string {
	if r.Status != StatusConfirmed {
		return r.Status
	}
	endsAt, err := time.Parse(time.RFC3339, r.EndsAt)
	if err != nil || !now.Before(endsAt) {
		return StatusExpired
	}
	return StatusConfirmed
}

// ---- 请求 ----

type (
	// VenuesListRequest 列出场馆。
	VenuesListRequest struct {
		Limit int `json:"limit"`
	}

	// ProjectsListRequest 按场馆列出项目。
	ProjectsListRequest struct {
		VenueID string `json:"venue_id"`
		Limit   int    `json:"limit"`
	}

	// SlotSearchRequest 按场馆/项目/日期搜时段。
	SlotSearchRequest struct {
		VenueID   string `json:"venue_id"`
		ProjectID string `json:"project_id"`
		Date      string `json:"date"`
		Limit     int    `json:"limit"`
	}

	// ReservationCreateRequest 创建预约。
	ReservationCreateRequest struct {
		VenueID   string `json:"venue_id"`
		ProjectID string `json:"project_id"`
		SlotID    string `json:"slot_id"`
		Count     int    `json:"count"`
	}

	// ReservationCancelRequest 取消本人预约。
	ReservationCancelRequest struct {
		ReservationID string `json:"reservation_id"`
	}

	// ReservationListRequest 列出本人预约。
	ReservationListRequest struct {
		Limit int `json:"limit"`
	}

	// ScheduleAddRequest 为预约建立本地日程。
	ScheduleAddRequest struct {
		ReservationID string `json:"reservation_id"`
	}
)

// NormalizeAndValidate 统一默认与边界（所有请求共用此约束集）。
func (r *VenuesListRequest) NormalizeAndValidate() error {
	limit, err := normalizeLimit(r.Limit, DefaultVenuesLimit, MaxVenuesLimit)
	if err != nil {
		return err
	}
	r.Limit = limit
	return nil
}

// NormalizeAndValidate 校验 venue_id 必填。
func (r *ProjectsListRequest) NormalizeAndValidate() error {
	r.VenueID = strings.TrimSpace(r.VenueID)
	limit, err := normalizeLimit(r.Limit, DefaultVenuesLimit, MaxVenuesLimit)
	if err != nil {
		return err
	}
	r.Limit = limit
	if !validStableID(r.VenueID) {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 校验 venue/project 必填与日期规范形。
func (r *SlotSearchRequest) NormalizeAndValidate() error {
	r.VenueID = strings.TrimSpace(r.VenueID)
	r.ProjectID = strings.TrimSpace(r.ProjectID)
	r.Date = strings.TrimSpace(r.Date)
	limit, err := normalizeLimit(r.Limit, DefaultSlotsLimit, MaxSlotsLimit)
	if err != nil {
		return err
	}
	r.Limit = limit
	if !validStableID(r.VenueID) || !validStableID(r.ProjectID) || !validSlotDate(r.Date) {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 默认 count=1 并约束 1..MaxCount。
func (r *ReservationCreateRequest) NormalizeAndValidate() error {
	r.VenueID = strings.TrimSpace(r.VenueID)
	r.ProjectID = strings.TrimSpace(r.ProjectID)
	r.SlotID = strings.TrimSpace(r.SlotID)
	if r.Count == 0 {
		r.Count = 1
	}
	if !validStableID(r.VenueID) || !validStableID(r.ProjectID) || !validStableID(r.SlotID) ||
		r.Count < 1 || r.Count > MaxCount {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 校验 reservation_id 必填。
func (r *ReservationCancelRequest) NormalizeAndValidate() error {
	r.ReservationID = strings.TrimSpace(r.ReservationID)
	if !validStableID(r.ReservationID) {
		return errInvalidArgument
	}
	return nil
}

// NormalizeAndValidate 统一默认与边界。
func (r *ReservationListRequest) NormalizeAndValidate() error {
	limit, err := normalizeLimit(r.Limit, DefaultReservationsLimit, MaxReservationsLimit)
	if err != nil {
		return err
	}
	r.Limit = limit
	return nil
}

// NormalizeAndValidate 校验 reservation_id 必填。
func (r *ScheduleAddRequest) NormalizeAndValidate() error {
	r.ReservationID = strings.TrimSpace(r.ReservationID)
	if !validStableID(r.ReservationID) {
		return errInvalidArgument
	}
	return nil
}

// ---- 校验辅助 ----

func validStableID(value string) bool {
	return value != "" && len(value) <= MaxIDLength && utf8.ValidString(value)
}

func validDisplayText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum &&
		!strings.ContainsAny(value, "\r\n\x00")
}

func validSlotDate(value string) bool {
	if len(value) != 10 {
		return false
	}
	parsed, err := time.ParseInLocation(AcademicDateLayout, value, time.UTC)
	return err == nil && parsed.Format(AcademicDateLayout) == value
}

func validHTTPSURL(value string) bool {
	if !validDisplayText(value, MaxURLChars) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return parsed.Fragment == ""
}

func validHeaderName(value string) bool {
	if value == "" || len(value) > MaxHeaderNameChars || strings.ContainsAny(value, "\r\n\x00:") {
		return false
	}
	for _, r := range value {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// NormalizeWebViewDescriptor 归一化并校验 WebView 描述符：仅 https 入口，
// 头名白名单字符集，禁止 cookie/authorization/token 类凭据头。
func NormalizeWebViewDescriptor(value WebViewDescriptor) (WebViewDescriptor, error) {
	value.EntryURL = strings.TrimSpace(value.EntryURL)
	value.RequiredUserAgent = strings.TrimSpace(value.RequiredUserAgent)
	value.SourceRevision = strings.TrimSpace(value.SourceRevision)
	if !validHTTPSURL(value.EntryURL) || !validDisplayText(value.RequiredUserAgent, MaxUserAgentChars) {
		return WebViewDescriptor{}, errInvalidArgument
	}
	if len(value.RequiredHeaders) > MaxHeaders {
		return WebViewDescriptor{}, errInvalidArgument
	}
	normalized := make([]RequiredHeader, 0, len(value.RequiredHeaders))
	seen := make(map[string]struct{}, len(value.RequiredHeaders))
	for _, header := range value.RequiredHeaders {
		header.Name = strings.TrimSpace(header.Name)
		header.Purpose = strings.TrimSpace(header.Purpose)
		lower := strings.ToLower(header.Name)
		if !validHeaderName(header.Name) || !validDisplayText(header.Purpose, MaxHeaderPurposeChars) {
			return WebViewDescriptor{}, errInvalidArgument
		}
		if lower == "cookie" || lower == "authorization" || strings.Contains(lower, "token") {
			return WebViewDescriptor{}, errInvalidArgument
		}
		if _, duplicate := seen[lower]; duplicate {
			return WebViewDescriptor{}, errInvalidArgument
		}
		seen[lower] = struct{}{}
		normalized = append(normalized, header)
	}
	value.RequiredHeaders = normalized
	return value, nil
}

// ParseAcademicDate 解析规范学术日期。
func ParseAcademicDate(value string) (time.Time, error) {
	return time.ParseInLocation(AcademicDateLayout, value, time.UTC)
}

// AcademicLocation 返回学术时区（tzdata 缺失时退化为固定东八区）。
func AcademicLocation() *time.Location {
	location, err := time.LoadLocation(AcademicTimezone)
	if err != nil {
		return time.FixedZone("CST", 8*60*60)
	}
	return location
}

// normalizeLimit 统一 limit 语义：0 视为未指定取默认值，越界按 invalid_argument 拒绝
// （与 library 同型：显式越界不是可静默纠正的输入）。
func normalizeLimit(limit, defaultValue, maximum int) (int, error) {
	if limit == 0 {
		return defaultValue, nil
	}
	if limit < 1 || limit > maximum {
		return 0, errInvalidArgument
	}
	return limit, nil
}
