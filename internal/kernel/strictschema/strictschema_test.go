package strictschema

import (
	"strings"
	"testing"
)

// TestValidateStrictJSON 表驱动验证严格 JSON 规则：重复键、根值后额外 Token、空输入。
func TestValidateStrictJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"空对象", `{}`, false},
		{"嵌套值", `{"a":1,"b":[true,null,"x"],"c":{"d":2}}`, false},
		{"空输入", ``, true},
		{"重复键", `{"a":1,"a":2}`, true},
		{"根值后额外 Token", `{"a":1} trailing`, true},
		{"非 JSON", `not json`, true},
		{"数组根", `[1,2]`, false},
		{"数组重复元素允许", `[1,1]`, false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := ValidateStrictJSON([]byte(test.input))
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateStrictJSON(%q) error=%v, wantErr=%v", test.input, err, test.wantErr)
			}
		})
	}
}

// TestCompileSchema 表驱动验证严格 Schema 编译：顶层 object、拒绝未知字段、禁止外部引用。
func TestCompileSchema(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr bool
	}{
		{"合法严格 Schema", `{"type":"object","properties":{"x":{"type":"integer"}},"additionalProperties":false}`, false},
		{"最小严格 Schema", `{"type":"object","additionalProperties":false}`, false},
		{"顶层非 object", `{"type":"array"}`, true},
		{"未拒绝未知字段", `{"type":"object"}`, true},
		{"重复键", `{"type":"object","type":"object","additionalProperties":false}`, true},
		{"外部引用", `{"type":"object","$ref":"https://evil.example/schema","additionalProperties":false}`, true},
		{"空 Schema", ``, true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			schema, err := CompileSchema("test", test.schema, "https://schema.invalid/ailuo/test/", 1<<20)
			if (err != nil) != test.wantErr {
				t.Fatalf("CompileSchema(%q) error=%v, wantErr=%v", test.schema, err, test.wantErr)
			}
			if err == nil && schema == nil {
				t.Fatal("成功编译却返回空 Schema")
			}
		})
	}
}

// TestValidatePayload 验证载荷校验：严格 JSON、大小上限、Schema 匹配。
func TestValidatePayload(t *testing.T) {
	schema, err := CompileSchema("test", `{"type":"object","properties":{"x":{"type":"integer"}},"additionalProperties":false}`, "https://schema.invalid/ailuo/test/", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"合法载荷", `{"x":1}`, false},
		{"未知字段被拒绝", `{"x":1,"y":2}`, true},
		{"重复键被拒绝", `{"x":1,"x":2}`, true},
		{"类型不匹配", `{"x":"one"}`, true},
		{"空载荷", ``, true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePayload(schema, []byte(test.payload), 1<<20)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidatePayload(%q) error=%v, wantErr=%v", test.payload, err, test.wantErr)
			}
		})
	}
	if err := ValidatePayload(schema, []byte(`{"x":1}`), 4); err == nil {
		t.Fatal("超过大小上限的载荷必须被拒绝")
	}
}

func FuzzValidateStrictJSON(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"a":1,"b":[true,null,"x"]}`,
		`{"a":1,"a":2}`,
		`{"a":1} trailing`,
		``,
		`not json`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// 任意输入不得 panic：要么合法，要么返回可读错误。
		_ = ValidateStrictJSON([]byte(raw))
	})
}

func FuzzCompileSchema(f *testing.F) {
	for _, seed := range []string{
		`{"type":"object","properties":{"x":{"type":"integer"}},"additionalProperties":false}`,
		`{"type":"object","additionalProperties":false}`,
		`{"type":"array"}`,
		`{"type":"object"}`,
		`{"type":"object","$ref":"https://evil.example/schema"}`,
		strings.Repeat(`{"type":"object",`, 10) + `"x":1}`,
		``,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// 任意 Schema 输入不得 panic；成功编译时不得返回空 Schema。
		schema, err := CompileSchema("fuzz", raw, "https://schema.invalid/ailuo/fuzz/", 1<<20)
		if err == nil && schema == nil {
			t.Fatal("编译成功却返回空 Schema")
		}
	})
}
