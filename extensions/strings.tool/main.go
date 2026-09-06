//go:build wasip1

// strings.tool 是 hosted 包参考实现：字符串工具集。
// 它演示 hosted 沙箱调用契约（stdin 调用信封 -> stdout 结果信封），只依赖标准库，
// 可交叉编译为 wasm32-wasi 工件，在任意平台由内核 WasmHost 沙箱执行。
// 仅参与 wasm32-wasi 交叉编译（ailuo pack 经 [build] 驱动）；宿主编译时该目录被忽略。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// hostedRequest 与宿主约定：stdin 调用信封。
type hostedRequest struct {
	ToolID  string          `json:"tool_id"`
	Payload json.RawMessage `json:"payload"`
}

// hostedEnvelope 与宿主约定：stdout 结果信封。
type hostedEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}

// run 读取调用信封、按工具分发、写回结果信封；任何解析失败都输出失败信封而非 panic。
func run(input io.Reader, output io.Writer) error {
	inputBytes, err := io.ReadAll(input)
	if err != nil {
		return writeEnvelope(output, hostedEnvelope{OK: false, Code: "internal", Message: "read stdin failed"})
	}
	var request hostedRequest
	if err := json.Unmarshal(inputBytes, &request); err != nil {
		return writeEnvelope(output, hostedEnvelope{OK: false, Code: "invalid_argument", Message: "request envelope is malformed"})
	}
	result, err := dispatch(request.ToolID, request.Payload)
	if err != nil {
		return writeEnvelope(output, hostedEnvelope{OK: false, Code: "invalid_argument", Message: err.Error()})
	}
	return writeEnvelope(output, hostedEnvelope{OK: true, Result: result})
}

// dispatch 按工具标识分发到具体实现。
func dispatch(toolID string, payload json.RawMessage) (json.RawMessage, error) {
	switch toolID {
	case "strings.len":
		return stringsLen(payload)
	case "strings.upper":
		return transform(payload, strings.ToUpper)
	case "strings.join":
		return stringsJoin(payload)
	default:
		return nil, errors.New("unknown tool: " + toolID)
	}
}

// stringsLen 返回字符串长度（UTF-8 字节数）。
func stringsLen(payload json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(payload, &args); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"length": len(args.Value)})
}

// transform 对字符串应用大小写转换。
func transform(payload json.RawMessage, apply func(string) string) (json.RawMessage, error) {
	var args struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(payload, &args); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"value": apply(args.Value)})
}

// stringsJoin 以分隔符拼接字符串切片。
func stringsJoin(payload json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Values    []string `json:"values"`
		Separator string   `json:"separator"`
	}
	if err := json.Unmarshal(payload, &args); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"value": strings.Join(args.Values, args.Separator)})
}

// writeEnvelope 序列化并写出结果信封。
func writeEnvelope(output io.Writer, envelope hostedEnvelope) error {
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = output.Write(data)
	return err
}
