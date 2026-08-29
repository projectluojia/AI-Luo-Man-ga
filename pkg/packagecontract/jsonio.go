package packagecontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// 包格式层的 JSON 原语（仅标准库）：严格解码（重复键/未知字段/尾随数据拒绝）。

// DecodeStrictJSON 严格解码单个 JSON 值：拒绝重复键、未知字段与尾随数据。
func DecodeStrictJSON(payload []byte, target any) error {
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrInvalidFormat
	}
	return nil
}

// rejectDuplicateJSONKeys 递归拒绝同一对象内的重复键。
func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrInvalidFormat
				}
				if _, duplicate := keys[key]; duplicate {
					return ErrInvalidFormat
				}
				keys[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return ErrInvalidFormat
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return ErrInvalidFormat
			}
		default:
			return ErrInvalidFormat
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidFormat
	}
	return nil
}
