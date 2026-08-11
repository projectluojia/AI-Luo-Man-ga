package task

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const maxSchemaBytes = 64 << 10

var typeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// Handler 执行一次任务。处理器必须遵守传入上下文中的 deadline 与取消；
// 副作用必须使用 task.IdempotencyKey 保证可重放安全。
type Handler func(ctx context.Context, value Task) error

// TypeSpec 描述一种封闭任务类型。任务类型必须在注册表预先注册，
// 调度器绝不执行未注册类型的任务。
type TypeSpec struct {
	TypeID       string          // 稳定类型标识（如 "bus.catalog.sync"）
	ParamsSchema json.RawMessage // 严格 JSON Schema：顶层 object、拒绝未知字段
	AllowRetry   bool            // 是否允许自动重试；含不安全副作用且非幂等的类型必须为 false
	Handler      Handler         // 处理器，不可为空
}

type compiledType struct {
	spec   TypeSpec
	schema *jsonschema.Schema
}

// TypeRegistry 是封闭任务类型注册表，注册时校验类型标识与参数 Schema。
type TypeRegistry struct {
	mu    sync.RWMutex
	types map[string]*compiledType
}

// NewTypeRegistry 创建空注册表。
func NewTypeRegistry() *TypeRegistry {
	return &TypeRegistry{types: make(map[string]*compiledType)}
}

// Register 原子注册一个封闭任务类型。重复注册、非法标识、缺失处理器或
// 非法 Schema 都会失败，保证注册表只包含可安全执行的类型。
func (r *TypeRegistry) Register(spec TypeSpec) error {
	if len(spec.TypeID) == 0 || len(spec.TypeID) > maxTypeIDBytes || !typeIDPattern.MatchString(spec.TypeID) {
		return fmt.Errorf("%w: 非法类型标识 %q", ErrInvalidTypeSpec, spec.TypeID)
	}
	if spec.Handler == nil {
		return fmt.Errorf("%w: %q 缺少处理器", ErrInvalidTypeSpec, spec.TypeID)
	}
	schema, err := compileParamsSchema(spec.TypeID, spec.ParamsSchema)
	if err != nil {
		return errors.Join(ErrInvalidTypeSpec, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.types[spec.TypeID]; exists {
		return ErrDuplicateType
	}
	r.types[spec.TypeID] = &compiledType{spec: spec, schema: schema}
	return nil
}

// Lookup 返回已注册类型的规格。
func (r *TypeRegistry) Lookup(typeID string) (TypeSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	compiled, ok := r.types[typeID]
	if !ok {
		return TypeSpec{}, false
	}
	return compiled.spec, true
}

// ValidateParams 校验参数是否符合已注册类型的 Schema。
// 调度器在创建与执行时都会调用，作为 Go 信任边界的双重校验。
func (r *TypeRegistry) ValidateParams(typeID string, params json.RawMessage) error {
	r.mu.RLock()
	compiled, ok := r.types[typeID]
	r.mu.RUnlock()
	if !ok {
		return ErrTaskTypeUnknown
	}
	if err := validateParamsAgainst(compiled.schema, params); err != nil {
		return err
	}
	return nil
}

// Len 返回已注册的任务类型数量。
func (r *TypeRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.types)
}

func compileParamsSchema(typeID string, raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 || len(raw) > maxSchemaBytes {
		return nil, errors.New("参数 Schema 为空或超过大小限制")
	}
	if err := validateNoDuplicateJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("参数 Schema 不是严格 JSON：%w", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := validateSchemaDocument(document); err != nil {
		return nil, err
	}
	resourceURL := "https://schema.invalid/ailuo/task/" + url.PathEscape(typeID)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resourceURL)
}

// validateSchemaDocument 校验任务参数 Schema 的结构约束：
//   - 顶层必须是显式 object；
//   - 对象 Schema 必须显式拒绝未知字段；
//   - 禁止任何外部 $ref（只允许文档内引用），防止依赖外部资源。
func validateSchemaDocument(document any) error {
	root, ok := document.(map[string]any)
	if !ok {
		return errors.New("任务参数 Schema 顶层必须是对象")
	}
	if root["type"] != "object" {
		return errors.New("任务参数 Schema 顶层 type 必须是 object")
	}
	return walkSchema(root)
}

func walkSchema(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
			if ref, ok := current[keyword].(string); ok && !strings.HasPrefix(ref, "#") {
				return errors.New("任务参数 Schema 禁止外部引用")
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

func validateParamsAgainst(schema *jsonschema.Schema, params json.RawMessage) error {
	if len(params) == 0 || len(params) > maxParamsBytes {
		return ErrInvalidParams
	}
	if err := validateNoDuplicateJSONKeys(params); err != nil {
		return ErrInvalidParams
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(params))
	if err != nil {
		return ErrInvalidParams
	}
	if err := schema.Validate(instance); err != nil {
		return ErrInvalidParams
	}
	return nil
}

// validateNoDuplicateJSONKeys 拒绝重复键等严格 JSON 违规，防止解析歧义。
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
