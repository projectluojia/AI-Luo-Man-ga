// Package jsonutil 提供严格 JSON 解码的共享原语：拒绝未知字段与尾随数据。
// 供内核各信任边界（平台接入、Agent 能力参数、安装清单、维护命令）统一使用，
// 避免每个边界各自实现一份"单 JSON 对象 + EOF"校验。
package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ErrTrailingData 表示输入包含第二个 JSON 值，违反严格单对象契约。
var ErrTrailingData = errors.New("json payload must contain exactly one value")

// DecodeStrict 从字节序列严格解码单个 JSON 值：拒绝未知字段与尾随数据。
func DecodeStrict(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return EnsureEOF(decoder)
}

// EnsureEOF 校验解码器不再有第二个 JSON 值（面向流式请求体解码）。
func EnsureEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrTrailingData
		}
		return err
	}
	return nil
}
