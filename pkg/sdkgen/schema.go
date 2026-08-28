package sdkgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/iancoleman/strcase"
)

// TypeKind 是 JSON Schema 类型在生成目标语言中的中性表示。
type TypeKind uint8

const (
	KindString TypeKind = iota
	KindInteger
	KindNumber
	KindBoolean
	KindDateTime // string + format=date-time
	KindEnum     // string + enum
	KindArray
	KindObject
)

// Field 是 object 类型的一个属性。
type Field struct {
	Name     string
	Type     *TypeModel
	Required bool
}

// TypeModel 是 JSON Schema 编译后的派生类型模型。
type TypeModel struct {
	Kind   TypeKind
	Name   string     // object/enum 的具名类型名；其余为空
	Elem   *TypeModel // KindArray 的元素类型
	Fields []Field    // KindObject 的属性
	Values []string   // KindEnum 的允许值
}

// schemaSpec 是生成 SDK 所支持的 JSON Schema 子集（严格解码）。
type schemaSpec struct {
	Type                 string                     `json:"type"`
	Format               string                     `json:"format"`
	Enum                 []json.RawMessage          `json:"enum"`
	Items                json.RawMessage            `json:"items"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
	// 组合/引用结构（改变类型语义，生成器无法正确表达 → 显式拒绝）：
	OneOf []json.RawMessage `json:"oneOf"`
	AllOf []json.RawMessage `json:"allOf"`
	AnyOf []json.RawMessage `json:"anyOf"`
	Not   json.RawMessage   `json:"not"`
	Ref   string            `json:"$ref"`
}

// schemaType 将 JSON Schema 编译为具名 TypeModel。name 是生成语言中的类型名。
// object 必须显式声明 additionalProperties:false 且给出 properties，与内核
// 严格解码契约一致；未知 type、缺 items 的 array、无 properties 的 object、
// 组合/引用结构（oneOf/allOf/anyOf/not/$ref）一律拒绝，不引入宽松回退。
// Schema 内约束关键字（minimum 等）不改变类型派生，按 JSON Schema 规范允许出现。
func schemaType(schema json.RawMessage, name string) (*TypeModel, error) {
	// 容忍合法但不参与类型派生的约束关键字（minimum 等），拒绝尾随内容：
	// decoder.More() 不覆盖尾随 `}`/`]`，故用二次 Decode 断言 io.EOF。
	decoder := json.NewDecoder(bytes.NewReader(schema))
	var spec schemaSpec
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("sdkgen: 解码 JSON Schema 失败: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("sdkgen: JSON Schema 包含多余内容")
	}
	if spec.OneOf != nil || spec.AllOf != nil || spec.AnyOf != nil || spec.Not != nil || spec.Ref != "" {
		return nil, fmt.Errorf("sdkgen: 不支持的组合/引用结构（oneOf/allOf/anyOf/not/$ref）")
	}
	// enum 只支持 string 基类型（见 stringOrEnum）：其余类型的 enum 无真实用例，
	// 半支持比拒绝更糟（Python 成员名 `1 = 1` 根本不是合法标识符），静默忽略更糟。
	if len(spec.Enum) > 0 && spec.Type != "string" {
		return nil, fmt.Errorf("sdkgen: 类型 %q 不支持 enum（enum 仅支持 string）", spec.Type)
	}
	switch spec.Type {
	case "string":
		return stringOrEnum(spec, name)
	case "integer":
		return &TypeModel{Kind: KindInteger}, nil
	case "number":
		return &TypeModel{Kind: KindNumber}, nil
	case "boolean":
		return &TypeModel{Kind: KindBoolean}, nil
	case "array":
		if len(spec.Items) == 0 {
			return nil, fmt.Errorf("sdkgen: array 类型缺少 items")
		}
		elem, err := schemaType(spec.Items, name+"Item")
		if err != nil {
			return nil, err
		}
		return &TypeModel{Kind: KindArray, Elem: elem}, nil
	case "object":
		return objectType(spec, name)
	default:
		return nil, fmt.Errorf("sdkgen: 不支持的 JSON Schema 类型 %q", spec.Type)
	}
}

// ValidateSchema 校验生成器可消费的严格 JSON Schema 子集。
// 源码契约提取器与消费方 SDK 生成器共用这一入口，保证两处接受同一契约。
func ValidateSchema(schema json.RawMessage) error {
	_, err := schemaType(schema, "Input")
	return err
}

// stringOrEnum 处理 string：带 enum 派生枚举类型，date-time 派生时间类型。
func stringOrEnum(spec schemaSpec, name string) (*TypeModel, error) {
	if len(spec.Enum) > 0 {
		if spec.Format != "" {
			return nil, fmt.Errorf("sdkgen: enum 与 format 不能同时声明")
		}
		values := make([]string, 0, len(spec.Enum))
		seenNames := make(map[string]struct{}, len(spec.Enum))
		for _, raw := range spec.Enum {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, fmt.Errorf("sdkgen: enum 值必须是 string: %w", err)
			}
			memberName := strcase.ToCamel(value)
			if !identifierPattern.MatchString(memberName) {
				return nil, fmt.Errorf("sdkgen: enum 值 %q 无法生成合法成员名", value)
			}
			if _, exists := seenNames[memberName]; exists {
				return nil, fmt.Errorf("sdkgen: enum 值 %q 规范化后成员名 %q 重复", value, memberName)
			}
			seenNames[memberName] = struct{}{}
			values = append(values, value)
		}
		return &TypeModel{Kind: KindEnum, Name: name, Values: values}, nil
	}
	if spec.Format == "date-time" {
		return &TypeModel{Kind: KindDateTime}, nil
	}
	return &TypeModel{Kind: KindString}, nil
}

// objectType 编译 object：要求显式 additionalProperties:false 与 properties。
func objectType(spec schemaSpec, name string) (*TypeModel, error) {
	if spec.AdditionalProperties == nil || *spec.AdditionalProperties {
		return nil, fmt.Errorf("sdkgen: object 类型必须显式声明 additionalProperties=false")
	}
	if len(spec.Properties) == 0 {
		return nil, fmt.Errorf("sdkgen: object 类型缺少 properties")
	}
	required := make(map[string]struct{}, len(spec.Required))
	for _, key := range spec.Required {
		required[key] = struct{}{}
	}
	keys := make([]string, 0, len(spec.Properties))
	for key := range spec.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]Field, 0, len(keys))
	for _, key := range keys {
		_, isRequired := required[key]
		fieldType, err := schemaType(spec.Properties[key], name+goFieldName(key))
		if err != nil {
			return nil, fmt.Errorf("sdkgen: 属性 %q: %w", key, err)
		}
		fields = append(fields, Field{Name: key, Type: fieldType, Required: isRequired})
	}
	return &TypeModel{Kind: KindObject, Name: name, Fields: fields}, nil
}
