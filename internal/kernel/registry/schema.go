package registry

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

const (
	maxSchemaBytes  = 64 << 10
	maxPayloadBytes = 64 << 10
)

func compileInputSchema(id, raw string) (*jsonschema.Schema, error) {
	if raw == "" || len(raw) > maxSchemaBytes {
		return nil, errors.New("JSON Schema 为空或超过大小限制")
	}
	if err := validateNoDuplicateJSONKeys([]byte(raw)); err != nil {
		return nil, fmt.Errorf("JSON Schema 不是严格 JSON：%w", err)
	}
	document, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := validateSchemaDocument(document); err != nil {
		return nil, err
	}
	resourceURL := "https://schema.invalid/ailuo/" + url.PathEscape(id)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resourceURL)
}

func validateSchemaDocument(document any) error {
	root, ok := document.(map[string]any)
	if !ok {
		return errors.New("Capability 输入 Schema 必须是对象")
	}
	if root["type"] != "object" {
		return errors.New("Capability 输入 Schema 顶层 type 必须是 object")
	}
	return walkSchema(root)
}

func walkSchema(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
			if ref, ok := current[keyword].(string); ok && !strings.HasPrefix(ref, "#") {
				return errors.New("JSON Schema 禁止外部引用")
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

func validatePayload(schema *jsonschema.Schema, payload json.RawMessage) error {
	if len(payload) == 0 || len(payload) > maxPayloadBytes {
		return ErrSchemaValidation
	}
	if err := validateNoDuplicateJSONKeys(payload); err != nil {
		return ErrSchemaValidation
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return ErrSchemaValidation
	}
	if err := schema.Validate(instance); err != nil {
		return ErrSchemaValidation
	}
	return nil
}

func validateNoDuplicateJSONKeys(payload []byte) error {
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
