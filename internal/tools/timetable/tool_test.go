package timetable

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCourseUpdateSchemaRequiresCourseID 锁定更新契约：course_id 必须出现在
// required 中，防止更新 Schema 与创建 Schema 漂移回共用状态。
func TestCourseUpdateSchemaRequiresCourseID(t *testing.T) {
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(CourseUpdateInputSchemaJSON), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	for _, field := range schema.Required {
		if field == "course_id" {
			return
		}
	}
	t.Fatalf("course update schema required=%v missing course_id", schema.Required)
}

// TestCourseCreateSchemaKeepsCourseIDOptional 锁定创建契约：创建不要求
// course_id，与更新 Schema 形成有意的差别。
func TestCourseCreateSchemaKeepsCourseIDOptional(t *testing.T) {
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(CourseInputSchemaJSON), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if strings.Join(schema.Required, ",") == "" {
		t.Fatalf("course create schema has empty required list")
	}
	for _, field := range schema.Required {
		if field == "course_id" {
			t.Fatalf("course create schema must not require course_id, required=%v", schema.Required)
		}
	}
}
