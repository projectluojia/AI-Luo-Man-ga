// Package guestkit 是 AI珞 hosted 包 guest ABI 1 的参考实现：stdin/stdout
// 调用信封循环、闭式错误码与 ailuo.store 宿主函数客户端。abi_version = "1"
// 的清单承诺本模块定义的调用协议；协议不兼容演进以新 ABI 版本号与 guestkit
// 新主版本承载，不做隐式兼容。
package guestkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Run 读取 stdin 调用信封、按能力分发表分发并写出结果信封：所有 guest
// 共用的 main 循环，分发表即 app 包的 Handlers。unknown capability 按
// 稳定 invalid_argument 应答。
func Run(input io.Reader, output io.Writer, handlers map[string]func(payload json.RawMessage) (any, error)) {
	RunDispatcher(input, output, NewDispatcher(handlers))
}

// RunDispatcher 是 Run 的分发器形态：已构造好的 Dispatcher 直接驱动
// stdin/stdout 循环。
func RunDispatcher(input io.Reader, output io.Writer, dispatch *Dispatcher) {
	inputBytes, err := io.ReadAll(input)
	if err != nil {
		writeEnvelope(output, ResultEnvelope{Code: CodeInternal, Message: "read stdin failed"})
		return
	}
	var request RequestEnvelope
	if err := json.Unmarshal(inputBytes, &request); err != nil {
		writeEnvelope(output, ResultEnvelope{Code: CodeInvalidArgument, Message: "request envelope is malformed"})
		return
	}
	writeEnvelope(output, dispatch.Dispatch(request.CapabilityID, request.Payload))
}

func writeEnvelope(output io.Writer, envelope ResultEnvelope) {
	data, err := json.Marshal(envelope)
	if err != nil {
		_, _ = output.Write([]byte(`{"ok":false,"code":"internal","message":"envelope marshal failed"}`))
		return
	}
	_, _ = output.Write(data)
}

// DecodePayload 严格解码调用载荷（len==0 视为空对象，供无参能力复用）。
// 格式错误按稳定 invalid_argument 应答。
func DecodePayload(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("payload is malformed")
	}
	// 第二个 JSON 值（无论合法与否）都违反单值契约：仅 EOF 表示干净结束。
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("payload is malformed")
	}
	return nil
}

// Govern 执行系统快照数据的新鲜度/权威性治理：失败返回稳定错误码
// （data_incomplete/data_untrusted/data_expired）。个人作用域数据不携带
// 快照元数据，不走本函数。
func Govern(meta SnapshotMeta) (SnapshotMeta, error) {
	if meta.SourceRevision == "" || meta.Source == "" || !meta.Complete ||
		meta.ImportedAt.IsZero() || meta.ValidUntil.IsZero() ||
		!meta.ValidUntil.After(meta.ImportedAt) {
		return SnapshotMeta{}, ErrDataIncomplete
	}
	if !meta.Authoritative {
		return SnapshotMeta{}, ErrDataUntrusted
	}
	if !clock().Before(meta.ValidUntil) {
		return SnapshotMeta{}, ErrDataExpired
	}
	return meta, nil
}

// DataStateAuthoritativeFresh 是治理通过后的唯一状态值。
const DataStateAuthoritativeFresh = "authoritative_fresh"

// DataStatus 是能力结果的治理状态：State 加快照元数据（嵌入展开为扁平 JSON）。
type DataStatus struct {
	State string `json:"state"`
	SnapshotMeta
}

// GovernStatus 治理快照元数据并展开为结果治理状态；失败返回稳定错误码错误。
func GovernStatus(meta SnapshotMeta) (DataStatus, error) {
	governed, err := Govern(meta)
	if err != nil {
		return DataStatus{}, err
	}
	return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMeta: governed}, nil
}

// clock 是当前时间读取点（测试可替换的变量而非固定依赖 time.Now 的闭包）。
var clock = time.Now

// ErrData* 是 Govern 失败的哨兵错误：Error() 即稳定错误码，调用方直接把
// 错误文本作为信封 code 应答。
var (
	ErrDataIncomplete = errors.New(CodeDataIncomplete)
	ErrDataUntrusted  = errors.New(CodeDataUntrusted)
	ErrDataExpired    = errors.New(CodeDataExpired)
)
