package schemaextract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnalyzeGoExtractsCapabilities(t *testing.T) {
	source := []byte(`package main

// hello 说你好。
func hello(args HelloArgs) {}
func ping(args PingArgs)      {}
func hidden()                  {}

type HelloArgs struct {
	Name string ` + "`json:\"name\"`" + `
	Count int32 ` + "`json:\"count,omitempty\"`" + `
}

type PingArgs struct {
	Message string ` + "`json:\"message\"`" + `
}
`)
	capabilities, err := AnalyzeGo(source, "my.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 2 {
		t.Fatalf("capabilities = %d, want 2", len(capabilities))
	}
	if capabilities[0].ID != "my.pkg.hello" || capabilities[0].Description != "hello 说你好。" {
		t.Fatalf("capability[0] = %+v", capabilities[0])
	}
	if capabilities[1].ID != "my.pkg.ping" {
		t.Fatalf("capability[1] = %+v", capabilities[1])
	}
}

func TestAnalyzeGoProducesValidSchema(t *testing.T) {
	source := []byte(`package main

func hello(args HelloArgs) {}

type HelloArgs struct {
	Name string ` + "`json:\"name\"`" + `
	Count int32 ` + "`json:\"count,omitempty\"`" + `
	At    string ` + "`json:\"at,omitempty\"`" + `  // time.Time 需 import 测试见下
}
`)
	capabilities, err := AnalyzeGo(source, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %d", len(capabilities))
	}
	var schema map[string]any
	if err := json.Unmarshal(capabilities[0].InputSchema, &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema = %+v", schema)
	}
	props := schema["properties"].(map[string]any)
	if len(props) != 3 {
		t.Fatalf("properties = %+v", props)
	}
	nameProp := props["name"].(map[string]any)
	if nameProp["type"] != "string" {
		t.Fatalf("name type = %v", nameProp["type"])
	}
	required := schema["required"].([]any)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("required = %v", required)
	}
}

func TestAnalyzeGoRejectsMultipleParameters(t *testing.T) {
	source := []byte(`package main
func hello(args Hello, token string) {}
type Hello struct { Name string ` + "`json:\"name\"`" + ` }
`)
	capabilities, err := AnalyzeGo(source, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 0 {
		t.Fatalf("capabilities = %+v, want no multi-parameter capability", capabilities)
	}
}

func TestAnalyzeGoRejectsGroupedMultipleParameters(t *testing.T) {
	source := []byte(`package main
func hello(first, second Hello) {}
type Hello struct { Name string ` + "`json:\"name\"`" + ` }
`)
	capabilities, err := AnalyzeGo(source, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 0 {
		t.Fatalf("capabilities = %+v, want no grouped multi-parameter capability", capabilities)
	}
}

func TestAnalyzeGoMapsByteSlicesToBase64(t *testing.T) {
	source := []byte(`package main
func upload(args UploadArgs) {}
type UploadArgs struct { Data []byte ` + "`json:\"data\"`" + ` }
`)
	capabilities, err := AnalyzeGo(source, "x")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(capabilities[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	data := schema["properties"].(map[string]any)["data"].(map[string]any)
	if data["type"] != "string" || data["contentEncoding"] != "base64" {
		t.Fatalf("data schema = %+v", data)
	}
}

func TestAnalyzeGoRejectsFixedByteArrays(t *testing.T) {
	source := []byte(`package main
func upload(args UploadArgs) {}
type UploadArgs struct { Data [32]byte ` + "`json:\"data\"`" + ` }
`)
	if _, err := AnalyzeGo(source, "x"); err == nil {
		t.Fatal("期望拒绝不受支持的定长字节数组")
	}
}

func TestAnalyzeGoRejectsPlatformDependentIntegers(t *testing.T) {
	source := []byte("package main\nfunc hello(args Hello) {}\ntype Hello struct { Count int `json:\"count\"` }\n")
	if _, err := AnalyzeGo(source, "x"); err == nil || !strings.Contains(err.Error(), "平台相关整数") {
		t.Fatalf("error = %v, want platform-dependent integer rejection", err)
	}
}

func TestAnalyzeGoRejectsUntaggedField(t *testing.T) {
	source := []byte(`package main
func hello(args Hello) {}
type Hello struct { Name string }
`)
	if _, err := AnalyzeGo(source, "x"); err == nil {
		t.Fatal("期望拒绝无 tag 字段")
	}
}

func TestAnalyzeGoRejectsUnsupportedJSONTagOptions(t *testing.T) {
	for _, tag := range []string{"json:\"count,string\"", "json:\"count,omitempty,string\"", "json:\"count,omitzero\""} {
		source := []byte("package main\nfunc hello(args Hello) {}\ntype Hello struct { Count int `" + tag + "` }\n")
		if _, err := AnalyzeGo(source, "x"); err == nil || !strings.Contains(err.Error(), "json tag") {
			t.Errorf("tag %s error = %v, want unsupported json tag option", tag, err)
		}
	}
}

func TestAnalyzeGoRejectsUnknownType(t *testing.T) {
	source := []byte(`package main
func hello(args Hello) {}
type Hello struct {
	Data json.RawMessage ` + "`json:\"data\"`" + `
}
`)
	if _, err := AnalyzeGo(source, "x"); err == nil {
		t.Fatal("期望拒绝未知类型")
	}
}

func TestAnalyzeGoWithTime(t *testing.T) {
	source := []byte(`package main
import "time"
func hello(args Hello) {}
type Hello struct {
	At time.Time ` + "`json:\"at\"`" + `
}
`)
	capabilities, err := AnalyzeGo(source, "x")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	json.Unmarshal(capabilities[0].InputSchema, &schema)
	props := schema["properties"].(map[string]any)
	atProp := props["at"].(map[string]any)
	if atProp["type"] != "string" || atProp["format"] != "date-time" {
		t.Fatalf("at = %+v", atProp)
	}
}

func TestAnalyzeGoWithPointerArg(t *testing.T) {
	source := []byte(`package main
func hello(args *Hello) {}
type Hello struct { Name string ` + "`json:\"name\"`" + ` }
`)
	capabilities, err := AnalyzeGo(source, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %d", len(capabilities))
	}
}

func TestAnalyzeGoResolvesNestedStructs(t *testing.T) {
	source := []byte(`package main
func hello(args HelloArgs) {}
type HelloArgs struct {
	Address Address ` + "`json:\"address\"`" + `
}
type Address struct {
	City string ` + "`json:\"city\"`" + `
}
`)
	capabilities, err := AnalyzeGo(source, "x")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(capabilities[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	address := schema["properties"].(map[string]any)["address"].(map[string]any)
	if address["type"] != "object" {
		t.Fatalf("address schema = %+v", address)
	}
	if address["properties"].(map[string]any)["city"].(map[string]any)["type"] != "string" {
		t.Fatalf("city schema = %+v", address)
	}
}

func TestAnalyzeGoRejectsRecursiveStructs(t *testing.T) {
	source := []byte(`package main
func hello(args Node) {}
type Node struct {
	Next *Node ` + "`json:\"next,omitempty\"`" + `
}
`)
	if _, err := AnalyzeGo(source, "x"); err == nil || !strings.Contains(err.Error(), "递归") {
		t.Fatalf("recursive struct error = %v, want explicit rejection", err)
	}
}
