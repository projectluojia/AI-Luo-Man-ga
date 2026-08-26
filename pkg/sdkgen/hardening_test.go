package sdkgen

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestGenerateRejectsReservedFieldNames 验证保留字字段名 fail-closed，
// 且错误信息含"保留字"提示（class 撞 Python/TS 共有保留字，from 只撞 Python）。
func TestGenerateRejectsReservedFieldNames(t *testing.T) {
	for _, name := range []string{"class", "from", "interface"} {
		schema := `{"type":"object","properties":{"` + name + `":{"type":"string"}},"additionalProperties":false}`
		source := `{"capabilities":[{"id":"x.y","input_schema_json":` + strconv.Quote(schema) + `}]}`
		_, err := Generate(json.RawMessage(source), Options{Language: LanguagePython, PackageID: "x"})
		if err == nil || !strings.Contains(err.Error(), "保留字") {
			t.Errorf("字段名 %q 未被拒为保留字: %v", name, err)
		}
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
		if err := validateCapabilityID(valid); err != nil {
			t.Errorf("validateCapabilityID(%q) 期望通过: %v", valid, err)
		}
	}
	for _, invalid := range []string{"UPPER", "has space", "1leading", "trailing."} {
		if err := validateCapabilityID(invalid); err == nil {
			t.Errorf("validateCapabilityID(%q) 期望拒绝", invalid)
		}
	}
}

// TestGenerateRejectsNonIdentifierFieldNames 验证 JSON key 合法但不能作标识符的
// 字段名 fail-closed：user-name 作 dataclass 属性/TS interface 属性都是语法错误。
func TestGenerateRejectsNonIdentifierFieldNames(t *testing.T) {
	for _, name := range []string{"user-name", "2fa", "a b", ""} {
		schema := `{"type":"object","properties":{"` + name + `":{"type":"string"}},"additionalProperties":false}`
		source := `{"capabilities":[{"id":"x.y","input_schema_json":` + strconv.Quote(schema) + `}]}`
		if _, err := Generate(json.RawMessage(source), Options{Language: LanguagePython, PackageID: "x"}); err == nil {
			t.Errorf("字段名 %q 期望拒绝，实际通过", name)
		}
	}
}

// TestGenerateRejectsDuplicateCapabilityID 验证重复 capability ID fail-closed：
// 重复 ID 会生成同名方法与同名输入类型，产物编译不过。
func TestGenerateRejectsDuplicateCapabilityID(t *testing.T) {
	capability := `{"id":"x.y","input_schema_json":"{\"type\":\"object\",\"properties\":{\"a\":{\"type\":\"string\"}},\"additionalProperties\":false}"}`
	source := `{"capabilities":[` + capability + `,` + capability + `]}`
	_, err := Generate(json.RawMessage(source), Options{Language: LanguageGo, PackageID: "x"})
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 capability ID 未被拒绝: %v", err)
	}
}

// TestGenerateScalarRootSchema 验证根 schema 为标量时签名类型正确：标量根没有
// 具名类型，直接打印 TypeModel.Name 会生成 `input ` 这样的语法错误（Go 侧
// format.Source 会直接失败）。
func TestGenerateScalarRootSchema(t *testing.T) {
	source := `{"capabilities":[{"id":"x.y","input_schema_json":"{\"type\":\"string\"}"}]}`
	wants := map[Language]string{
		LanguageGo:         "input string)",
		LanguagePython:     "input: str)",
		LanguageTypeScript: "input: string)",
	}
	for language, want := range wants {
		files, err := Generate(json.RawMessage(source), Options{Language: language, PackageID: "x"})
		if err != nil {
			t.Errorf("Generate(%s) 标量根 schema 失败: %v", language, err)
			continue
		}
		if !strings.Contains(string(files[0].Code), want) {
			t.Errorf("%s 生成代码缺少 %q\n---\n%s", language, want, files[0].Code)
		}
	}
}

// TestGeneratePythonInitExportsDefinedNames 验证 __init__.py 重新导出的函数名与
// client.py 定义的一致：方法名按完整包 ID 去前缀派生，pythonInit 若只拿包 ID
// 首段（campus.bus → campus）就会导出不存在的符号，import 即 ImportError。
func TestGeneratePythonInitExportsDefinedNames(t *testing.T) {
	source := `{"capabilities":[{"id":"campus.bus.stops.search","input_schema_json":"{\"type\":\"object\",\"properties\":{\"query\":{\"type\":\"string\"}},\"additionalProperties\":false}"}]}`
	files, err := Generate(json.RawMessage(source), Options{Language: LanguagePython, PackageID: "campus.bus"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("Python 产物文件数 = %d，want 2", len(files))
	}
	client, initFile := string(files[0].Code), string(files[1].Code)
	if !strings.Contains(client, "def stops_search(") {
		t.Fatalf("client.py 未定义 stops_search\n---\n%s", client)
	}
	if !strings.Contains(initFile, "from .client import stops_search\n") {
		t.Fatalf("__init__.py 未导出 stops_search\n---\n%s", initFile)
	}
}
