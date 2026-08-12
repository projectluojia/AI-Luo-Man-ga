//go:build wasip1

// campus 是校园服务的 hosted 包形态：复用 internal/campus/bus 的业务逻辑与治理，
// 数据访问经宿主函数 ailuo.bus.query 投影到 Go 托管的权威存储（App 隔离在宿主侧强制）。
// 仅参与 wasm32-wasi 交叉编译（build.ps1 / build.sh）；宿主编译时该目录被忽略。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

//go:wasmimport ailuo.bus query
func busQuery(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32

const (
	// hostFunctionError 是宿主函数调用失败的长度标记（-1 的无符号表示）。
	hostFunctionError = 0xFFFFFFFF
	// maxHostResponse 是宿主函数响应缓冲上限（与内核消息上限一致）。
	maxHostResponse = 512 << 10
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
	// 日志写入 stderr（宿主丢弃），避免污染 stdout 结果信封通道。
	if _, err := observe.Configure(observe.Config{
		Service: "ailuo-campus-hosted", Environment: "production", Format: "json", Writer: os.Stderr,
	}); err != nil {
		writeEnvelope(os.Stdout, hostedEnvelope{OK: false, Code: "internal", Message: "observe configure failed"})
		return
	}
	run(os.Stdin, os.Stdout)
}

// run 读取调用信封、按工具分发到 bus 逻辑、写回结果信封。
func run(input io.Reader, output io.Writer) {
	inputBytes, err := io.ReadAll(input)
	if err != nil {
		writeEnvelope(output, hostedEnvelope{OK: false, Code: "internal", Message: "read stdin failed"})
		return
	}
	var request hostedRequest
	if err := json.Unmarshal(inputBytes, &request); err != nil {
		writeEnvelope(output, hostedEnvelope{OK: false, Code: "invalid_argument", Message: "request envelope is malformed"})
		return
	}
	handlers := bus.ToolHandlers(hostStore{})
	handler, exists := handlers[request.ToolID]
	if !exists {
		writeEnvelope(output, hostedEnvelope{OK: false, Code: "invalid_argument", Message: "unknown tool: " + request.ToolID})
		return
	}
	result, err := handler(context.Background(), contracts.RequestContext{}, request.Payload)
	if err != nil {
		writeEnvelope(output, hostedEnvelope{OK: false, Code: codeFor(err), Message: err.Error()})
		return
	}
	writeEnvelope(output, hostedEnvelope{OK: true, Result: result})
}

// hostStore 是 bus.Store 的宿主函数适配：查询经线性内存 ABI 投影到 Go 内核。
// App 隔离由宿主侧治理上下文强制，本适配忽略传入的 appID。
type hostStore struct{}

func (hostStore) SearchStops(_ context.Context, _ string, request bus.StopSearchRequest) (bus.StopSnapshot, error) {
	return query[bus.StopSnapshot]("search_stops", request)
}

func (hostStore) ListRoutes(_ context.Context, _ string, request bus.RouteListRequest) (bus.RouteSnapshot, error) {
	return query[bus.RouteSnapshot]("list_routes", request)
}

func (hostStore) SearchJourneys(_ context.Context, _ string, request bus.SearchRequest) (bus.JourneySnapshot, error) {
	return query[bus.JourneySnapshot]("search_journeys", request)
}

// query 调用宿主函数并把响应反序列化为目标快照类型。
func query[T any](op string, args any) (T, error) {
	var result T
	if err := callBusQuery(op, args, &result); err != nil {
		return result, err
	}
	return result, nil
}

// busHostResponse 与宿主约定：宿主函数响应信封。
type busHostResponse struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

// callBusQuery 以线性内存 ABI 调用宿主函数并反序列化查询结果。
// 宿主函数失败信封按闭式错误码映射回数据治理错误，供业务治理链识别。
func callBusQuery(op string, args any, result any) error {
	request, err := json.Marshal(map[string]any{"op": op, "args": args})
	if err != nil {
		return err
	}
	response := make([]byte, maxHostResponse)
	length := busQuery(unsafe.Pointer(&request[0]), uint32(len(request)), unsafe.Pointer(&response[0]), uint32(len(response)))
	if length == hostFunctionError {
		return errors.New("bus host function unavailable")
	}
	var envelope busHostResponse
	if err := json.Unmarshal(response[:length], &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		switch envelope.Code {
		case "data_unavailable":
			return errors.Join(contracts.ErrDataUnavailable, errors.New("bus host function: data unavailable"))
		case "data_incomplete":
			return errors.Join(contracts.ErrDataIncomplete, errors.New("bus host function: data incomplete"))
		case "data_untrusted":
			return errors.Join(contracts.ErrDataUntrusted, errors.New("bus host function: data untrusted"))
		case "data_expired":
			return errors.Join(contracts.ErrDataExpired, errors.New("bus host function: data expired"))
		default:
			return fmt.Errorf("bus host function failed: %s", envelope.Code)
		}
	}
	return json.Unmarshal(envelope.Result, result)
}

// codeFor 把 bus 业务错误映射为信封闭式错误码（与宿主侧稳定错误映射对应）。
func codeFor(err error) string {
	switch {
	case errors.Is(err, contracts.ErrDataUnavailable):
		return "data_unavailable"
	case errors.Is(err, contracts.ErrDataIncomplete):
		return "data_incomplete"
	case errors.Is(err, contracts.ErrDataUntrusted):
		return "data_untrusted"
	case errors.Is(err, contracts.ErrDataExpired):
		return "data_expired"
	case isInvalidArgument(err):
		return "invalid_argument"
	default:
		return "internal"
	}
}

// isInvalidArgument 判定参数类错误（bus 校验错误与 JSON 解码错误）。
func isInvalidArgument(err error) bool {
	if errors.Is(err, bus.ErrOriginRequired) || errors.Is(err, bus.ErrDestinationRequired) ||
		errors.Is(err, bus.ErrSameStop) || errors.Is(err, bus.ErrInvalidLimit) ||
		errors.Is(err, bus.ErrQueryRequired) {
		return true
	}
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	return errors.As(err, &syntaxError) || errors.As(err, &typeError)
}

// writeEnvelope 序列化并写出结果信封。
func writeEnvelope(output io.Writer, envelope hostedEnvelope) {
	data, err := json.Marshal(envelope)
	if err != nil {
		output.Write([]byte(`{"ok":false,"code":"internal","message":"envelope marshal failed"}`))
		return
	}
	output.Write(data)
}
