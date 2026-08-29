package timetable

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
)

var digitsPattern = regexp.MustCompile(`\d+`)

// ParseAcademic 解析武大教务公开课表契约：顶层 kbList 以及其中的 kcmc、xqj/
// xqjmc、jcs/jc/jcsm、zcd/zcmc/zc 等字段。未知的来源字段被忽略，来源字段
// 仍在 Go 信任边界经过课程模型校验。
func ParseAcademic(raw []byte) ([]Course, error) {
	if len(raw) == 0 || len(raw) > MaxImportBytes {
		return nil, ErrMalformedData
	}
	var envelope struct {
		KBList []map[string]json.RawMessage `json:"kbList"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.Join(ErrMalformedData, err)
	}
	result := make([]Course, 0, len(envelope.KBList))
	for _, item := range envelope.KBList {
		titleRaw := rawString(item["kcmc"])
		title := SanitizeDisplay(titleRaw)
		if title == "" {
			continue
		}
		weekday := parseWeekday(rawString(item["xqj"]), rawString(item["xqjmc"]))
		period := parseClassPeriod(firstRawString(item, "jcs", "jc", "jcsm"))
		weekMeta := strings.TrimSpace(rawString(item["zcd"]))
		weeks := parseWeeks(firstRawString(item, "zcd", "zcmc", "zc"))
		if weekday < 1 || period == nil || len(weeks) == 0 {
			continue
		}
		course := Course{
			Title: title, TitleRaw: titleRaw, Weekday: weekday,
			ClassFrom: period.from, ClassTo: period.to, Weeks: weeks,
			ExternalID:   strings.TrimSpace(rawString(item["jxbmc"])),
			CourseNature: SanitizeDisplay(firstRawString(item, "kcxz", "kcxzmc", "kclbmc", "kclb")),
			Instructor:   SanitizeDisplay(rawString(item["xm"])), InstructorRaw: rawString(item["xm"]),
			Location: SanitizeDisplay(rawString(item["cdmc"])), WeekMeta: weekMeta,
			StartText: firstRawString(item, "kssj", "sksj_kssj", "kssj_hhmm"),
			EndText:   firstRawString(item, "jssj", "sksj_jssj", "jssj_hhmm"),
		}
		result = append(result, course)
	}
	return result, nil
}

// ParseWuDa 是 ParseAcademic 的稳定中文来源别名。
func ParseWuDa(raw []byte) ([]Course, error) { return ParseAcademic(raw) }

// ParseWakeUpEnvelope 解析 {format,content,fileName} envelope。format 为
// legacy 时使用旧版 JSON 行布局，其余取值按公开脚本兼容为 CSV。
func ParseWakeUpEnvelope(raw []byte) (ImportData, error) {
	if len(raw) == 0 || len(raw) > MaxImportBytes {
		return ImportData{}, ErrMalformedData
	}
	var input struct {
		Format   string `json:"format"`
		Content  string `json:"content"`
		FileName string `json:"fileName"`
	}
	if err := jsonutil.DecodeStrict(raw, &input); err != nil {
		return ImportData{}, errors.Join(ErrMalformedData, err)
	}
	if len(input.Content) > MaxImportBytes {
		return ImportData{}, ErrMalformedData
	}
	if input.Format == "legacy" {
		return parseWakeUpLegacy(input.Content)
	}
	return parseWakeUpCSV(input.Content, input.FileName)
}

// ParseWakeUp 是 WakeUp envelope 解析的稳定入口别名。
func ParseWakeUp(raw []byte) (ImportData, error) { return ParseWakeUpEnvelope(raw) }

func parseWakeUpCSV(content, fileName string) (ImportData, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			filtered = append(filtered, line)
		}
	}
	if len(filtered) < 2 {
		return ImportData{}, ErrNoCourses
	}
	items := make([]Course, 0, len(filtered)-1)
	for _, line := range filtered[1:] {
		row, err := splitCSV(line)
		if err != nil || len(row) < 7 {
			return ImportData{}, ErrMalformedData
		}
		day, okDay := strictInt(row[1])
		from, okFrom := strictInt(row[2])
		to, okTo := strictInt(row[3])
		weeks := parseWeeks(row[6])
		if !okDay || !okFrom || !okTo || day < 1 || day > 7 || from < 1 || to < from || to > MaxClassPeriod || len(weeks) == 0 {
			return ImportData{}, ErrMalformedData
		}
		teacher := SanitizeDisplay(row[4])
		location := SanitizeDisplay(row[5])
		if teacher == "无" {
			teacher = ""
		}
		if location == "无" {
			location = ""
		}
		items = append(items, Course{
			Title: SanitizeDisplay(row[0]), Weekday: day, ClassFrom: from, ClassTo: to,
			Weeks: weeks, Instructor: teacher, Location: location, WeekMeta: SanitizeDisplay(row[6]),
		})
	}
	if len(items) == 0 {
		return ImportData{}, ErrNoCourses
	}
	return ImportData{Name: fileBase(fileName), Source: SourceWakeUp, Courses: items}, nil
}

func parseWakeUpLegacy(content string) (ImportData, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 5 {
		return ImportData{}, ErrMalformedData
	}
	var table struct {
		TableName string `json:"tableName"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &table); err != nil {
		return ImportData{}, ErrMalformedData
	}
	var base []struct {
		ID         json.RawMessage `json:"id"`
		CourseName string          `json:"courseName"`
	}
	var details []struct {
		ID        json.RawMessage `json:"id"`
		Day       json.RawMessage `json:"day"`
		StartNode json.RawMessage `json:"startNode"`
		Step      json.RawMessage `json:"step"`
		Type      json.RawMessage `json:"type"`
		Teacher   string          `json:"teacher"`
		Room      string          `json:"room"`
		StartWeek json.RawMessage `json:"startWeek"`
		EndWeek   json.RawMessage `json:"endWeek"`
	}
	if err := json.Unmarshal([]byte(lines[3]), &base); err != nil {
		return ImportData{}, ErrMalformedData
	}
	if err := json.Unmarshal([]byte(lines[4]), &details); err != nil {
		return ImportData{}, ErrMalformedData
	}
	names := make(map[string]string, len(base))
	for _, item := range base {
		name := SanitizeDisplay(item.CourseName)
		if name == "" {
			name = "未命名课程"
		}
		names[jsonKey(item.ID)] = name
	}
	items := make([]Course, 0, len(details))
	for _, detail := range details {
		day, okDay := integerRaw(detail.Day)
		start, okStart := integerRaw(detail.StartNode)
		if !okDay || !okStart || day < 1 || day > 7 || start < 1 {
			// 与 CSV 分支一致采用全有或全无：单行非法即整体拒绝，
			// 不允许静默丢行让用户拿到不完整的课表。
			return ImportData{}, ErrMalformedData
		}
		step := 1
		if value, ok := integerRaw(detail.Step); ok {
			step = value
		}
		if step < 1 || start+step-1 > MaxClassPeriod {
			return ImportData{}, ErrMalformedData
		}
		typeValue, _ := integerRaw(detail.Type)
		suffix := ""
		switch typeValue {
		case 1:
			suffix = "单"
		case 2:
			suffix = "双"
		}
		startWeek := 1
		if value, ok := integerRaw(detail.StartWeek); ok && value > 0 {
			startWeek = value
		}
		endWeek := 30
		if value, ok := integerRaw(detail.EndWeek); ok && value > 0 {
			endWeek = value
		}
		weeks := parseWeeks(fmt.Sprintf("%d-%d%s", startWeek, endWeek, suffix))
		if len(weeks) == 0 {
			return ImportData{}, ErrMalformedData
		}
		title := names[jsonKey(detail.ID)]
		if title == "" {
			title = "未命名课程"
		}
		items = append(items, Course{
			Title: title, Weekday: day, ClassFrom: start,
			ClassTo: start + step - 1, Weeks: weeks, Instructor: SanitizeDisplay(detail.Teacher),
			Location: SanitizeDisplay(detail.Room), WeekMeta: fmt.Sprintf("%d-%d%s", startWeek, endWeek, suffix),
		})
	}
	if len(items) == 0 {
		return ImportData{}, ErrNoCourses
	}
	name := SanitizeDisplay(table.TableName)
	if name == "" {
		name = "WakeUp 课程表"
	}
	return ImportData{Name: name, Source: SourceWakeUp, Courses: items}, nil
}

func splitCSV(line string) ([]string, error) {
	reader := csv.NewReader(bytes.NewBufferString(line))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = false
	row, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, ErrMalformedData
	}
	return row, err
}

func parseWeekday(raw, label string) int {
	for _, valueText := range []string{raw, label} {
		if value, ok := strictInt(valueText); ok && value >= 1 && value <= 7 {
			return value
		}
	}
	for _, text := range []string{raw, label} {
		for index, marker := range []string{"一", "二", "三", "四", "五", "六"} {
			if strings.Contains(text, marker) {
				return index + 1
			}
		}
		if strings.Contains(text, "日") || strings.Contains(text, "天") || strings.Contains(text, "七") {
			return 7
		}
	}
	return -1
}

type classPeriod struct{ from, to int }

func parseClassPeriod(text string) *classPeriod {
	text = strings.NewReplacer("～", "-", "~", "-", "—", "-", "－", "-").Replace(text)
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, text)
	numbers := digitsPattern.FindAllString(text, -1)
	if len(numbers) == 0 {
		return nil
	}
	from, _ := strconv.Atoi(numbers[0])
	to := from
	if len(numbers) > 1 {
		to, _ = strconv.Atoi(numbers[1])
	}
	if from <= 0 || to < from || to > MaxClassPeriod {
		return nil
	}
	return &classPeriod{from: from, to: to}
}

func parseWeeks(value string) []int {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	seen := make(map[int]struct{})
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool {
		return strings.ContainsRune(",，;；、", r)
	}) {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		odd := strings.Contains(segment, "单")
		even := strings.Contains(segment, "双")
		numbers := digitsPattern.FindAllString(segment, -1)
		if len(numbers) == 0 {
			continue
		}
		start, _ := strconv.Atoi(numbers[0])
		end := start
		if len(numbers) > 1 {
			end, _ = strconv.Atoi(numbers[1])
		}
		if start < 1 || end < start || end > MaxWeeks {
			return nil
		}
		for week := start; week <= end; week++ {
			if odd && week%2 == 0 || even && week%2 == 1 {
				continue
			}
			seen[week] = struct{}{}
		}
	}
	result := make([]int, 0, len(seen))
	for week := range seen {
		result = append(result, week)
	}
	// NormalizeCourse 还会再次检查排序和重复；这里排序使解析结果本身稳定。
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j] < result[j-1]; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result
}

func firstRawString(item map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(rawString(item[key])); value != "" {
			return value
		}
	}
	return ""
}

func rawString(value json.RawMessage) string {
	if len(value) == 0 || string(value) == "null" {
		return ""
	}
	var stringValue string
	if json.Unmarshal(value, &stringValue) == nil {
		return stringValue
	}
	return strings.TrimSpace(string(value))
}

func strictInt(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return -1, false
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

// integerRaw 读取 legacy JSON 的数字字段。旧版 WakeUp 导出在数字与字符串两种
// 编码间混排（如 "day":3 与 "day":"3"），带引号的合法数字需要先解码再校验，
// 否则整行课程会被静默丢弃。
func integerRaw(value json.RawMessage) (int, bool) {
	if len(value) == 0 {
		return -1, false
	}
	trimmed := strings.TrimSpace(string(value))
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if json.Unmarshal(value, &text) != nil {
			return -1, false
		}
		return strictInt(text)
	}
	return strictInt(trimmed)
}

func jsonKey(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	if value[0] == '"' {
		return rawString(value)
	}
	return strings.TrimSpace(string(value))
}

func fileBase(name string) string {
	name = SanitizeDisplay(name)
	if name == "" {
		return ""
	}
	name = strings.ReplaceAll(name, "\\", "/")
	base := filepath.Base(name)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}
