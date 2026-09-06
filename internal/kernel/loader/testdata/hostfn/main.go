// hostfn 是 hosted 宿主函数投影的测试工件：调用 ailuo.host.echo 宿主函数，
// 验证线性内存 ABI 与治理上下文绑定。只用于 internal/kernel/loader 测试，非产品代码。
package main

import (
	"encoding/json"
	"io"
	"os"
	"unsafe"
)

//go:wasmimport ailuo.host echo
func hostEcho(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32

// hostedRequest 与宿主约定：stdin 调用信封。
type hostedRequest struct {
	CapabilityID string          `json:"capability_id"`
	Payload      json.RawMessage `json:"payload"`
}

// hostedEnvelope 与宿主约定：stdout 结果信封。
type hostedEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

func main() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeEnvelope(hostedEnvelope{OK: false, Code: "internal", Message: "read stdin failed"})
		return
	}
	var request hostedRequest
	if err := json.Unmarshal(input, &request); err != nil {
		writeEnvelope(hostedEnvelope{OK: false, Code: "invalid_argument", Message: "request envelope is malformed"})
		return
	}
	// 调用宿主函数回显载荷，验证宿主函数投影链路。
	response, err := callHostEcho(request.Payload)
	if err != nil {
		writeEnvelope(hostedEnvelope{OK: false, Code: "host_function_error", Message: err.Error()})
		return
	}
	writeEnvelope(hostedEnvelope{OK: true, Result: response})
}

// callHostEcho 以线性内存 ABI 调用宿主函数：请求与响应都位于 guest 线性内存。
func callHostEcho(request []byte) ([]byte, error) {
	if len(request) == 0 {
		request = []byte("null")
	}
	response := make([]byte, 4096)
	length := hostEcho(unsafe.Pointer(&request[0]), uint32(len(request)), unsafe.Pointer(&response[0]), uint32(len(response)))
	if length == 0xFFFFFFFF {
		return nil, os.ErrInvalid
	}
	return response[:length], nil
}

// writeEnvelope 序列化并写出结果信封。
func writeEnvelope(envelope hostedEnvelope) {
	data, err := json.Marshal(envelope)
	if err != nil {
		os.Stdout.Write([]byte(`{"ok":false,"code":"internal","message":"envelope marshal failed"}`))
		return
	}
	os.Stdout.Write(data)
}
