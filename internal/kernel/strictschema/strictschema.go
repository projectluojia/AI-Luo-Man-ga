// Package strictschema 提供严格的 JSON Schema 编译与载荷校验，
// 供 Registry 与后台任务类型注册表等内核模块共用，避免重复实现。
//
// 严格约定：
//   - 输入必须是严格 JSON（拒绝重复键、根值后的额外 Token 等歧义）；
//   - Schema 顶层必须是显式 object 并显式拒绝未知字段；
//   - 禁止任何外部 $ref，只允许文档内引用。
package strictschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateStrictJSON 校验输入为严格 JSON：拒绝重复键与根值后的额外 Token。
func ValidateStrictJSON(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("JSON 根值后存在额外 Token：%v", token)
	}
	return nil
}

// CompileSchema 编译严格 JSON Schema。id 用于生成资源定位符，maxBytes 为原始
// Schema 文本的大小上限；编译失败返回可读错误。
func CompileSchema(id, raw, resourcePrefix string, maxBytes int) (*jsonschema.Schema, error) {
	if raw == "" || len(raw) > maxBytes {
		return nil, errors.New("JSON Schema 为空或超过大小限制")
	}
	if err := ValidateStrictJSON([]byte(raw)); err != nil {
		return nil, fmt.Errorf("输入不是严格 JSON：%w", err)
	}
	document, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := validateSchemaDocument(document); err != nil {
		return nil, err
	}
	resourceURL := resourcePrefix + url.PathEscape(id)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resourceURL)
}

// ValidatePayload 校验载荷符合已编译 Schema（严格 JSON + 大小上限）。
func ValidatePayload(schema *jsonschema.Schema, payload json.RawMessage, maxBytes int) error {
	if len(payload) == 0 || len(payload) > maxBytes {
		return errors.New("载荷为空或超过大小限制")
	}
	if err := ValidateStrictJSON(payload); err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	return schema.Validate(instance)
}

// validateSchemaDocument 校验 Schema 文档的结构约束。
func validateSchemaDocument(document any) error {
	root, ok := document.(map[string]any)
	if !ok {
		return errors.New("输入 Schema 必须是对象")
	}
	if root["type"] != "object" {
		return errors.New("输入 Schema 顶层 type 必须是 object")
	}
	return walkSchema(root)
}

func walkSchema(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
			if ref, ok := current[keyword].(string); ok && !strings.HasPrefix(ref, "#") {
				return errors.New("输入 Schema 禁止外部引用")
			}
		}
		if usesObjectKeywords(current) {
			if current["type"] != "object" {
				return errors.New("对象 Schema 必须显式声明 type 为 object")
			}
			additional, exists := current["additionalProperties"]
			if !exists || additional != false {
				return errors.New("对象 Schema 必须显式拒绝未知字段")
			}
		}
		for _, child := range current {
			if err := walkSchema(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range current {
			if err := walkSchema(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func usesObjectKeywords(schema map[string]any) bool {
	if schema["type"] == "object" {
		return true
	}
	for _, keyword := range []string{
		"properties",
		"patternProperties",
		"additionalProperties",
		"propertyNames",
		"required",
		"dependentRequired",
		"dependentSchemas",
		"minProperties",
		"maxProperties",
	} {
		if _, exists := schema[keyword]; exists {
			return true
		}
	}
	return false
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON 对象键不是字符串")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("JSON 对象包含重复字段 %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("JSON 对象没有正确结束")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("JSON 数组没有正确结束")
		}
	default:
		return errors.New("JSON 包含无效分隔符")
	}
	return nil
}
