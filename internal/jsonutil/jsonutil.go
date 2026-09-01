// Package jsonutil 提供严格 JSON 解码与规范化摘要的共享原语：拒绝未知字段与
// 尾随数据、单 JSON 对象 EOF 校验、canonical 摘要。供内核各信任边界（平台
// 接入、Executor 能力参数、安装清单、维护命令、参数摘要）统一使用，避免每个
// 边界各自实现一份。
package jsonutil

import (
	"bytes"
	"crypto/sha256"
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

// CanonicalJSON 把 JSON 载荷反序列化后重新序列化（键序稳定、空白归一）；
// 空载荷等价于空对象 {}。载荷变更必然产生不同输出。
func CanonicalJSON(payload []byte) ([]byte, error) {
	var decoded any
	if len(payload) == 0 {
		decoded = map[string]any{}
	} else if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// CanonicalDigest 对 CanonicalJSON 的输出取 sha256。
func CanonicalDigest(payload []byte) ([32]byte, error) {
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}
