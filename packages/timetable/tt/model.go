// Package tt 是课表包的纯领域层：课表/课程模型、显示文本清洗、
// 课表导入解析（武大教务契约、WakeUp CSV、WakeUp legacy）与 doc 载荷编码。
// 仅依赖标准库，无 wasm 标签、无宿主函数引用——协议分发与 ailuo.store
// 调用全部在 packages/timetable/app 与 packages/timetable/src 完成。
package tt

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	SourceLocal  = "local"
	SourceWUDA   = "wuda"
	SourceWakeUp = "wakeup"

	MaxTimetablesPerUser = 32
	MaxCoursesPerTable   = 512
	MaxWeeks             = 64
	// MaxClassPeriod 是单次连堂的节次上限。
	MaxClassPeriod      = 64
	MaxCourseTitleChars = 256
	MaxTextChars        = 512
	MaxImportBytes      = 64 << 10
	// MaxDocIDLen 与宿主 ailuo.store 文档 ID 上限一致。
	MaxDocIDLen = 128
	// MaxTimetableIDLen 是课表 ID 上限：课程集合名由 "courses-" 前缀 +
	// 课表 ID 派生，必须整体落在集合名上限内（len("courses-")+len ≤ 128）。
	MaxTimetableIDLen = MaxDocIDLen - len("courses-")
)

var (
	ErrInvalid       = errors.New("invalid timetable input")
	ErrNotFound      = errors.New("timetable resource not found")
	ErrConflict      = errors.New("timetable resource already exists")
	ErrCapacity      = errors.New("timetable capacity exceeded")
	ErrNoCourses     = errors.New("timetable import contains no courses")
	ErrUnsupported   = errors.New("unsupported timetable import format")
	ErrMalformedData = errors.New("malformed timetable import data")
	// ErrTooLarge 表示导入内容超出 MaxImportBytes 字节预算；与格式错误的
	// ErrMalformedData 区分，避免把"内容过大"误报为"数据格式错误"。
	ErrTooLarge = errors.New("timetable import content exceeds size limit")
)

// Timetable 是用户的一份课表（个人作用域文档载荷）。
// 激活状态不属于文档：当前激活课表由 app 层 active 集合内的单文档指针
// 记录，读取时实时计算，避免双写两份文档的非原子窗口。
type Timetable struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Course 是课表内一个可显示的课程时段。ID 是本地稳定主键，
// ExternalID 对应教务系统的教学班标识（如 jxbmc）。
type Course struct {
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

// ImportData 是解析器输出的、尚未绑定课表 ID 的导入结果。
type ImportData struct {
	Name    string
	Source  string
	Courses []Course
}

// docIDPattern 是宿主 ailuo.store 文档 ID/集合名的闭式规则
// （capability.IsStableID：小写字母开头、[._-] 分隔且不收尾）。
// guest 侧先行校验避免调用必然失败的写入。
var docIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// ValidDocID 判断一个文档 ID 是否满足宿主 ailuo.store 的闭式规则。
func ValidDocID(id string) bool {
	return len(id) <= MaxDocIDLen && docIDPattern.MatchString(id)
}

// ValidTimetableID 在 ValidDocID 之上收紧长度：课程集合名 "courses-"+ID
// 不能超出集合名上限，所以课表 ID 必须预留前缀长度。
func ValidTimetableID(id string) bool {
	return len(id) <= MaxTimetableIDLen && docIDPattern.MatchString(id)
}

func normalizeTimetable(value Timetable, allowEmptyID bool) (Timetable, error) {
	value.ID = strings.TrimSpace(value.ID)
	value.Name = SanitizeDisplay(value.Name)
	value.Source = strings.TrimSpace(value.Source)
	if (!allowEmptyID && value.ID == "") || (value.ID != "" && !ValidDocID(value.ID)) ||
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

// NormalizeNewTimetable 校验创建课表模型，允许由调用方分配 ID。
func NormalizeNewTimetable(value Timetable) (Timetable, error) {
	return normalizeTimetable(value, true)
}

// NormalizeCourse 归一化课程显示文本、周次和槽位。
func NormalizeCourse(value Course) (Course, error) {
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
	if value.TimetableID == "" || len(value.TimetableID) > MaxDocIDLen || !ValidDocID(value.TimetableID) ||
		value.ID == "" || len(value.ID) > MaxDocIDLen || !ValidDocID(value.ID) || value.Title == "" ||
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
