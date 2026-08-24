package sdkgen

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaTypeScalars(t *testing.T) {
	tests := []struct {
		schema string
		want   TypeKind
	}{
		{`{"type":"string"}`, KindString},
		{`{"type":"integer"}`, KindInteger},
		{`{"type":"number"}`, KindNumber},
		{`{"type":"boolean"}`, KindBoolean},
		{`{"type":"string","format":"date-time"}`, KindDateTime},
	}
	for _, test := range tests {
		model, err := schemaType(json.RawMessage(test.schema), "Input")
		if err != nil {
			t.Fatalf("schemaType(%s): %v", test.schema, err)
		}
		if model.Kind != test.want {
			t.Fatalf("schemaType(%s) kind = %v, want %v", test.schema, model.Kind, test.want)
		}
	}
}

func TestSchemaTypeEnum(t *testing.T) {
	model, err := schemaType(json.RawMessage(`{"type":"string","enum":["active","inactive"]}`), "StatusEnum")
	if err != nil {
		t.Fatal(err)
	}
	if model.Kind != KindEnum || model.Base != KindString || model.Name != "StatusEnum" {
		t.Fatalf("enum model = %+v", model)
	}
	if len(model.Values) != 2 || model.Values[0] != "active" || model.Values[1] != "inactive" {
		t.Fatalf("enum values = %v", model.Values)
	}
}

func TestSchemaTypeArray(t *testing.T) {
	model, err := schemaType(json.RawMessage(`{"type":"array","items":{"type":"string"}}`), "Names")
	if err != nil {
		t.Fatal(err)
	}
	if model.Kind != KindArray || model.Elem.Kind != KindString {
		t.Fatalf("array model = %+v", model)
	}
}

func TestSchemaTypeObject(t *testing.T) {
	schema := `{"type":"object","properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`
	model, err := schemaType(json.RawMessage(schema), "SearchInput")
	if err != nil {
		t.Fatal(err)
	}
	if model.Kind != KindObject || model.Name != "SearchInput" || len(model.Fields) != 2 {
		t.Fatalf("object model = %+v", model)
	}
	query, limit := model.Fields[0], model.Fields[1]
	if query.Name != "limit" || limit.Name != "query" {
		t.Fatalf("字段排序错误: %+v", model.Fields)
	}
	if !limit.Required || limit.Type.Kind != KindString {
		t.Fatalf("query 字段 = %+v", limit)
	}
	if query.Required || query.Type.Kind != KindInteger {
		t.Fatalf("limit 字段 = %+v", query)
	}
}

func TestSchemaTypeRejects(t *testing.T) {
	tests := []string{
		`{"type":"custom"}`, // 未知类型
		`{"type":"array"}`,  // 缺 items
		`{"type":"object","properties":{"a":{"type":"string"}}}`, // 缺 additionalProperties
		`{"type":"object","additionalProperties":false}`,         // 缺 properties
		`{"type":"string","enum":[1]}`,                           // enum 值非字符串
		`{"type":"number","enum":["x"]}`,                         // number 不支持 enum
		`{"type":"string","format":"date-time","enum":["x"]}`,    // enum 与 format 冲突
	}
	for _, schema := range tests {
		if _, err := schemaType(json.RawMessage(schema), "Input"); err == nil {
			t.Errorf("schemaType(%s) 期望拒绝，实际通过", schema)
		}
	}
}

func TestExportName(t *testing.T) {
	if got := exportName("campus.bus.stops.search"); got != "CampusBusStopsSearch" {
		t.Fatalf("exportName = %q", got)
	}
	if got := exportName("stops"); got != "Stops" {
		t.Fatalf("exportName 单段 = %q", got)
	}
}

func TestStripPackagePrefix(t *testing.T) {
	if got := stripPackagePrefix("campus.bus.stops.search", "campus"); got != "bus.stops.search" {
		t.Fatalf("stripPackagePrefix = %q", got)
	}
	if got := stripPackagePrefix("other.bus", "campus"); got != "other.bus" {
		t.Fatalf("stripPackagePrefix 无前缀 = %q", got)
	}
}

func TestPythonMethodName(t *testing.T) {
	if got := pythonMethodName("campus.bus.journeys.search", "campus"); got != "bus_journeys_search" {
		t.Fatalf("pythonMethodName = %q", got)
	}
}

// TestGenerateRejectsBadSource 验证生成入口对畸形契约输入 fail-closed。
func TestGenerateRejectsBadSource(t *testing.T) {
	tests := []string{
		``,                                // 空
		`{"unknown":true}`,                // 未知字段
		`{"capabilities":[]}`,             // 无 capability
		`{"capabilities":[{"id":"a.b"}]}`, // 缺 schema
	}
	for _, source := range tests {
		_, err := Generate(json.RawMessage(source), Options{Language: LanguageGo, PackageID: "campus"})
		if err == nil {
			t.Errorf("Generate(%q) 期望拒绝，实际通过", source)
		}
		if !strings.Contains(err.Error(), "sdkgen:") {
			t.Errorf("Generate(%q) 错误未带 sdkgen 前缀: %v", source, err)
		}
	}
}
