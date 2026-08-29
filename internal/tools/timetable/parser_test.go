package timetable

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseAcademicCompatibleFieldsAndAliases(t *testing.T) {
	raw := []byte(`{"kbList":[{"kcmc":" 数学\u200b  ","xqj":"","xqjmc":"星期三","jcs":"第 3—4 节","zcd":"1-4周, 6周,单周","jxbmc":"class-1","xm":"张\u200b老师","cdmc":"教一\u00a0101","kssj":"08:00","jssj":"09:45"}]}`)
	courses, err := ParseAcademic(raw)
	if err != nil || len(courses) != 1 {
		t.Fatalf("ParseAcademic courses=%#v err=%v", courses, err)
	}
	course := courses[0]
	if course.Title != "数学" || course.Weekday != 3 || course.ClassFrom != 3 || course.ClassTo != 4 ||
		course.ExternalID != "class-1" || course.Instructor != "张老师" || course.Location != "教一 101" {
		t.Fatalf("unexpected course=%#v", course)
	}
	want := []int{1, 2, 3, 4, 6}
	if len(course.Weeks) != len(want) {
		t.Fatalf("weeks=%v", course.Weeks)
	}
	for i, week := range want {
		if course.Weeks[i] != week {
			t.Fatalf("weeks=%v", course.Weeks)
		}
	}
}

func TestParseWakeUpCSVQuotedAndLegacy(t *testing.T) {
	csvEnvelope := []byte(`{"format":"csv","fileName":"我的,课表.csv","content":"name,day,startNode,endNode,teacher,location,weekMeta\n\"高等,数学\",2,1,2,\"张老师\",\"教一,101\",1-16周"}`)
	data, err := ParseWakeUpEnvelope(csvEnvelope)
	if err != nil || len(data.Courses) != 1 || data.Name != "我的,课表" {
		t.Fatalf("csv data=%#v err=%v", data, err)
	}
	if data.Courses[0].Title != "高等,数学" || data.Courses[0].Location != "教一,101" || data.Courses[0].ClassTo != 2 {
		t.Fatalf("csv course=%#v", data.Courses[0])
	}

	legacy := `header-0
header-1
{"tableName":"旧课表"}
[{"id":7,"courseName":"物理"}]
[{"id":7,"day":5,"startNode":6,"step":2,"type":2,"teacher":"李老师","room":"教二"}]`
	legacyData, err := ParseWakeUpEnvelope([]byte(`{"format":"legacy","content":` + quoteJSON(legacy) + `}`))
	if err != nil || len(legacyData.Courses) != 1 || legacyData.Courses[0].Title != "物理" || legacyData.Courses[0].Weeks[0] != 2 {
		t.Fatalf("legacy data=%#v err=%v", legacyData, err)
	}
}

func TestParseWakeUpLegacyAcceptsQuotedNumericFields(t *testing.T) {
	legacy := `header-0
header-1
{"tableName":"旧课表"}
[{"id":"7","courseName":"化学"}]
[{"id":"7","day":"4","startNode":"2","step":"3","type":"1","teacher":"王老师","room":"教三","startWeek":"3","endWeek":"18"}]`
	data, err := ParseWakeUpEnvelope([]byte(`{"format":"legacy","content":` + quoteJSON(legacy) + `}`))
	if err != nil || len(data.Courses) != 1 {
		t.Fatalf("legacy data=%#v err=%v", data, err)
	}
	course := data.Courses[0]
	if course.Title != "化学" || course.Weekday != 4 || course.ClassFrom != 2 || course.ClassTo != 4 {
		t.Fatalf("course=%#v", course)
	}
	if want := []int{3, 5, 7, 9, 11, 13, 15, 17}; !sameInts(course.Weeks, want) {
		t.Fatalf("weeks=%v", course.Weeks)
	}
}

func TestParseAcademicRejectsMalformedRow(t *testing.T) {
	raw := []byte(`{"kbList":[{"kcmc":"高数","zcd":"1-4周"}]}`)
	if _, err := ParseAcademic(raw); !errors.Is(err, ErrMalformedData) {
		t.Fatalf("academic missing weekday err=%v", err)
	}
}

func TestImportSizeErrorsAreDistinctFromMalformedData(t *testing.T) {
	huge := strings.Repeat("a", MaxImportBytes+1)
	if _, err := ParseAcademic([]byte(huge)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("academic size err=%v", err)
	}
	envelope := []byte(`{"format":"csv","content":"` + huge + `"}`)
	if _, err := ParseWakeUpEnvelope(envelope); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("envelope size err=%v", err)
	}
	data, err := parseCapabilityImport(importInput{Format: "csv", Content: huge})
	if !errors.Is(err, ErrTooLarge) || len(data.Courses) != 0 {
		t.Fatalf("capability size data=%#v err=%v", data, err)
	}
}

func TestParseWakeUpLegacyRejectsInvalidDetailRow(t *testing.T) {
	legacy := `header-0
header-1
{"tableName":"旧课表"}
[{"id":7,"courseName":"物理"}]
[{"id":7,"day":"9","startNode":2,"step":2,"type":2,"teacher":"李老师","room":"教二"}]`
	if _, err := ParseWakeUpEnvelope([]byte(`{"format":"legacy","content":` + quoteJSON(legacy) + `}`)); !errors.Is(err, ErrMalformedData) {
		t.Fatalf("legacy invalid row err=%v", err)
	}
}

func TestParseWakeUpCSVRejectsClassPeriodBeyondLimit(t *testing.T) {
	csvEnvelope := []byte(`{"format":"csv","content":"name,day,startNode,endNode,teacher,location,weekMeta\n高数,1,1,65,张老师,教一,1-16周"}`)
	if _, err := ParseWakeUpEnvelope(csvEnvelope); !errors.Is(err, ErrMalformedData) {
		t.Fatalf("csv err=%v", err)
	}
}

func TestSanitizeDisplayCollapsesLineBreaks(t *testing.T) {
	if got := SanitizeDisplay("高数\r\n第二节\t "); got != "高数 第二节" {
		t.Fatalf("sanitize=%q", got)
	}
}

func TestNormalizeCourseRejectsOverlongDisplayFields(t *testing.T) {
	course := Course{AppID: "app-a", UserID: "alice", TimetableID: "table-1", ID: "course-1",
		Title: "高数", Weekday: 1, ClassFrom: 1, ClassTo: 2, Weeks: []int{1},
		Instructor: strings.Repeat("张", MaxTextChars+1)}
	if _, err := NormalizeCourse(course); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overlong instructor err=%v", err)
	}
	course.Instructor = strings.Repeat("张", MaxTextChars)
	course.CourseNature = strings.Repeat("通", MaxTextChars)
	if _, err := NormalizeCourse(course); err != nil {
		t.Fatalf("boundary fields err=%v", err)
	}
}

func TestParseWeeksSupportsOddEvenAndRanges(t *testing.T) {
	if got := parseWeeks("1-6周,8周"); !sameInts(got, []int{1, 2, 3, 4, 5, 6, 8}) {
		t.Fatalf("range weeks=%v", got)
	}
	if got := parseWeeks("1-6单周"); !sameInts(got, []int{1, 3, 5}) {
		t.Fatalf("odd weeks=%v", got)
	}
	if got := parseWeeks("1-6双周"); !sameInts(got, []int{2, 4, 6}) {
		t.Fatalf("even weeks=%v", got)
	}
}

func sameInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
