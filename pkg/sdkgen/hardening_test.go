package sdkgen

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestGenerateRejectsReservedFieldNames 验证保留字字段名 fail-closed。
func TestGenerateRejectsReservedFieldNames(t *testing.T) {
	source := `{"capabilities":[{"id":"x.y","input_schema_json":"{\"type\":\"object\",\"properties\":{\"class\":{\"type\":\"string\"}},\"required\":[\"class\"],\"additionalProperties\":false}"}]}`
	if _, err := Generate(json.RawMessage(source), Options{Language: LanguagePython, PackageID: "x"}); err == nil {
		t.Fatal("期望拒绝 Python 保留字字段名")
	}
	if _, err := Generate(json.RawMessage(source), Options{Language: LanguageTypeScript, PackageID: "x"}); err == nil {
		t.Fatal("期望拒绝 TypeScript 保留字字段名")
	}
}

// TestGenerateRejectsCompositionSchema 验证组合/引用结构 fail-closed。
func TestGenerateRejectsCompositionSchema(t *testing.T) {
	tests := []string{
		`{"oneOf":[{"type":"string"},{"type":"integer"}]}`,
		`{"allOf":[{"type":"string"}]}`,
		`{"anyOf":[{"type":"string"}]}`,
		`{"not":{"type":"string"}}`,
		`{"$ref":"#/definitions/X"}`,
	}
	for _, schema := range tests {
		source := `{"capabilities":[{"id":"x.y","input_schema_json":` + strconv.Quote(schema) + `}]}`
		if _, err := Generate(json.RawMessage(source), Options{Language: LanguageGo, PackageID: "x"}); err == nil {
			t.Errorf("schema %s 期望拒绝", schema)
		}
	}
}

// TestGenerateRejectsInvalidCapabilityID 验证 capability ID 格式 fail-closed。
func TestGenerateRejectsInvalidCapabilityID(t *testing.T) {
	source := `{"capabilities":[{"id":"UPPER.Case","input_schema_json":"{\"type\":\"object\",\"properties\":{\"a\":{\"type\":\"string\"}},\"required\":[\"a\"],\"additionalProperties\":false}"}]}`
	if _, err := Generate(json.RawMessage(source), Options{Language: LanguageGo, PackageID: "x"}); err == nil {
		t.Fatal("期望拒绝非法 capability ID")
	}
}

func TestValidateCapabilityID(t *testing.T) {
	for _, valid := range []string{"campus.bus.stops.search", "hello", "a.b-c"} {
		if err := ValidateCapabilityID(valid); err != nil {
			t.Errorf("ValidateCapabilityID(%q) 期望通过: %v", valid, err)
		}
	}
	for _, invalid := range []string{"UPPER", "has space", "1leading", "trailing."} {
		if err := ValidateCapabilityID(invalid); err == nil {
			t.Errorf("ValidateCapabilityID(%q) 期望拒绝", invalid)
		}
	}
}

func TestValidateFieldNames(t *testing.T) {
	// 合法字段名通过（campus 真实契约）。
	source := `{"capabilities":[{"id":"x.y","input_schema_json":"{\"type\":\"object\",\"properties\":{\"origin_stop_id\":{\"type\":\"string\"}},\"required\":[\"origin_stop_id\"],\"additionalProperties\":false}"}]}`
	if _, err := Generate(json.RawMessage(source), Options{Language: LanguagePython, PackageID: "x"}); err != nil {
		t.Fatalf("合法字段名被拒绝: %v", err)
	}
}

// TestGenerateRejectsReservedError 验证错误信息可读（含保留字提示）。
func TestGenerateRejectsReservedError(t *testing.T) {
	source := `{"capabilities":[{"id":"x.y","input_schema_json":"{\"type\":\"object\",\"properties\":{\"from\":{\"type\":\"string\"}},\"required\":[\"from\"],\"additionalProperties\":false}"}]}`
	_, err := Generate(json.RawMessage(source), Options{Language: LanguagePython, PackageID: "x"})
	if err == nil || !strings.Contains(err.Error(), "保留字") {
		t.Fatalf("错误信息不含保留字提示: %v", err)
	}
}
