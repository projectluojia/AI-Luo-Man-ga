// Package timetable 提供课表原子 Tool 的数据模型、导入解析与存储端口。
// 业务数据由 Go 托管存储实现；本包不持有数据库连接，也不提供外部网络接入。
package timetable

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
)

const (
	SourceLocal    = "local"
	SourceWUDA     = "wuda"
	SourceWuDa     = SourceWUDA
	SourceAcademic = SourceWUDA
	SourceWakeUp   = "wakeup"

	MaxTimetablesPerUser = 32
	MaxCoursesPerTable   = 512
	MaxWeeks             = 64
	// MaxClassPeriod 是单次连堂的节次上限；SQLite CHECK 约束使用同一数值。
	MaxClassPeriod      = 64
	MaxCourseTitleChars = 256
	MaxTextChars        = 512
	MaxImportBytes      = 64 << 10
)

var (
	ErrInvalid       = errors.New("invalid timetable input")
	ErrNotFound      = errors.New("timetable resource not found")
	ErrConflict      = errors.New("timetable resource already exists")
	ErrCapacity      = errors.New("timetable capacity exceeded")
	ErrUserRequired  = errors.New("timetable user is required")
	ErrNoCourses     = errors.New("timetable import contains no courses")
	ErrUnsupported   = errors.New("unsupported timetable import format")
	ErrMalformedData = errors.New("malformed timetable import data")
	// ErrTooLarge 表示导入内容超出 MaxImportBytes 字节预算；与格式错误的
	// ErrMalformedData 区分，避免把"内容过大"误报为"数据格式错误"。
	ErrTooLarge = errors.New("timetable import content exceeds size limit")
)

// Timetable 是用户在指定 App 内的一份课表。
type Timetable struct {
	AppID     string    `json:"-"`
	UserID    string    `json:"-"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Source    string    `json:"source"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Course 是课表内一个可显示的课程时段。Course.ID 是本地稳定主键，
// ExternalID 对应教务系统的教学班标识（如 jxbmc）。
type Course struct {
	AppID         string `json:"-"`
	UserID        string `json:"-"`
	TimetableID   string `json:"timetable_id"`
	ID            string `json:"id"`
	Title         string `json:"title"`
	TitleRaw      string `json:"title_raw,omitempty"`
	Weekday       int    `json:"weekday"`
	ClassFrom     int    `json:"class_from"`
	ClassTo       int    `json:"class_to"`
	Weeks         []int  `json:"weeks"`
	CourseNature  string `json:"course_nature,omitempty"`
	Instructor    string `json:"instructor,omitempty"`
	InstructorRaw string `json:"instructor_raw,omitempty"`
	Location      string `json:"location,omitempty"`
	WeekMeta      string `json:"week_meta,omitempty"`
	StartText     string `json:"start_text,omitempty"`
	EndText       string `json:"end_text,omitempty"`
	ExternalID    string `json:"external_id,omitempty"`
}

// ImportData 是解析器输出的、尚未绑定 App/User/课表 ID 的导入结果。
type ImportData struct {
	Name    string
	Source  string
	Courses []Course
}

// Store 是 Go 管理的课表存储端口。实现必须在每个方法的 SQL 边界同时约束
// app_id 与 user_id；业务 Service/Tool 不得绕过该端口访问数据库。
type Store interface {
	CreateTimetable(context.Context, Timetable) (Timetable, error)
	ListTimetables(context.Context, string, string) ([]Timetable, error)
	GetTimetable(context.Context, string, string, string) (Timetable, error)
	UpdateTimetable(context.Context, Timetable) (Timetable, error)
	SetTimetableActive(context.Context, string, string, string) (Timetable, error)
	DeleteTimetable(context.Context, string, string, string) error

	ListCourses(context.Context, string, string, string) ([]Course, error)
	GetCourse(context.Context, string, string, string, string) (Course, error)
	CreateCourse(context.Context, Course) (Course, error)
	UpdateCourse(context.Context, Course) (Course, error)
	DeleteCourse(context.Context, string, string, string, string) error

	ImportTimetable(context.Context, Timetable, []Course) (Timetable, []Course, error)
}

func normalizeTimetable(value Timetable, allowEmptyID bool) (Timetable, error) {
	value.AppID = strings.TrimSpace(value.AppID)
	value.UserID = strings.TrimSpace(value.UserID)
	value.ID = strings.TrimSpace(value.ID)
	value.Name = SanitizeDisplay(value.Name)
	value.Source = strings.TrimSpace(value.Source)
	if err := identity.ValidateAppID(value.AppID); err != nil {
		return Timetable{}, ErrInvalid
	}
	if err := identity.ValidateUserID(value.UserID); err != nil {
		return Timetable{}, ErrInvalid
	}
	if (!allowEmptyID && value.ID == "") || len(value.ID) > 128 || strings.ContainsAny(value.ID, "\r\n\x00") ||
		(value.ID != "" && !id.StableMixed.MatchString(value.ID)) ||
		value.Name == "" || utf8.RuneCountInString(value.Name) > MaxTextChars ||
		(value.Source != SourceLocal && value.Source != SourceWUDA && value.Source != SourceWakeUp) {
		return Timetable{}, ErrInvalid
	}
	return value, nil
}

// NormalizeTimetable 校验一个完整课表模型。
func NormalizeTimetable(value Timetable) (Timetable, error) {
	return normalizeTimetable(value, false)
}

// NormalizeNewTimetable 校验创建课表模型，允许由存储/Service 分配 ID。
func NormalizeNewTimetable(value Timetable) (Timetable, error) {
	return normalizeTimetable(value, true)
}

// NormalizeCourse 归一化课程显示文本、周次和槽位。
func NormalizeCourse(value Course) (Course, error) {
	value.AppID = strings.TrimSpace(value.AppID)
	value.UserID = strings.TrimSpace(value.UserID)
	value.TimetableID = strings.TrimSpace(value.TimetableID)
	value.ID = strings.TrimSpace(value.ID)
	value.TitleRaw = strings.TrimSpace(value.TitleRaw)
	value.Title = SanitizeDisplay(value.Title)
	value.CourseNature = SanitizeDisplay(value.CourseNature)
	value.InstructorRaw = strings.TrimSpace(value.InstructorRaw)
	value.Instructor = SanitizeDisplay(value.Instructor)
	value.Location = SanitizeDisplay(value.Location)
	value.WeekMeta = SanitizeDisplay(value.WeekMeta)
	value.StartText = SanitizeDisplay(value.StartText)
	value.EndText = SanitizeDisplay(value.EndText)
	value.ExternalID = strings.TrimSpace(value.ExternalID)
	if err := identity.ValidateAppID(value.AppID); err != nil {
		return Course{}, ErrInvalid
	}
	if err := identity.ValidateUserID(value.UserID); err != nil {
		return Course{}, ErrInvalid
	}
	if value.TimetableID == "" || value.ID == "" || value.Title == "" ||
		!id.StableMixed.MatchString(value.TimetableID) || !id.StableMixed.MatchString(value.ID) ||
		utf8.RuneCountInString(value.Title) > MaxCourseTitleChars ||
		value.Weekday < 1 || value.Weekday > 7 || value.ClassFrom < 1 ||
		value.ClassTo < value.ClassFrom || value.ClassTo > MaxClassPeriod ||
		utf8.RuneCountInString(value.TitleRaw) > MaxTextChars || utf8.RuneCountInString(value.InstructorRaw) > MaxTextChars ||
		utf8.RuneCountInString(value.ExternalID) > MaxTextChars ||
		utf8.RuneCountInString(value.CourseNature) > MaxTextChars || utf8.RuneCountInString(value.Instructor) > MaxTextChars ||
		utf8.RuneCountInString(value.Location) > MaxTextChars || utf8.RuneCountInString(value.WeekMeta) > MaxTextChars ||
		utf8.RuneCountInString(value.StartText) > MaxTextChars || utf8.RuneCountInString(value.EndText) > MaxTextChars {
		return Course{}, ErrInvalid
	}
	weeks, err := normalizeWeeks(value.Weeks)
	if err != nil {
		return Course{}, err
	}
	value.Weeks = weeks
	return value, nil
}

func normalizeWeeks(weeks []int) ([]int, error) {
	if len(weeks) == 0 || len(weeks) > MaxWeeks {
		return nil, ErrInvalid
	}
	result := append([]int(nil), weeks...)
	sort.Ints(result)
	for i, week := range result {
		if week < 1 || week > MaxWeeks || (i > 0 && result[i-1] == week) {
			return nil, ErrInvalid
		}
	}
	return result, nil
}

// SanitizeDisplay 去除控制/格式字符并折叠常见 Unicode 空白，保持与 Luotopia
// 热更新解析脚本相同的显示文本清洗边界。
func SanitizeDisplay(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r <= 0x08 || r == 0x0b || r == 0x0c || (r >= 0x0e && r <= 0x1f) ||
			r == 0x7f || r == 0x00ad || (r >= 0x200b && r <= 0x200f) ||
			(r >= 0x202a && r <= 0x202e) || r == 0x2060 ||
			(r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			continue
		case r == 0x00a0 || (r >= 0x2000 && r <= 0x200a) || r == 0x202f || r == 0x205f:
			builder.WriteByte(' ')
		default:
			builder.WriteRune(r)
		}
	}
	return strings.TrimSpace(collapseASCIIWhitespace(builder.String()))
}

// collapseASCIIWhitespace 把连续 ASCII 空白折叠为单个空格；CR/LF 属于展示文本
// 边界内的可折叠空白，不能原样保留在课程展示字段中。
func collapseASCIIWhitespace(value string) string {
	var builder strings.Builder
	space := false
	for _, r := range value {
		if r == ' ' || r == '\t' || r == '\f' || r == '\v' || r == '\r' || r == '\n' {
			space = true
			continue
		}
		if space && builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		space = false
		builder.WriteRune(r)
	}
	return builder.String()
}
