package schemaextract

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"reflect"
	"strconv"
	"strings"
)

// structSchema 把 struct AST 编译为 JSON Schema（object + additionalProperties:false）。
func structSchemaWithTypes(structType *ast.StructType, types map[string]*ast.StructType, imports map[string]string, resolving map[string]bool) (json.RawMessage, error) {
	if structType.Fields == nil || len(structType.Fields.List) == 0 {
		return nil, fmt.Errorf("schemaextract: 参数 struct 不能为空")
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"required":             []string{},
		"additionalProperties": false,
	}
	properties := schema["properties"].(map[string]any)
	required := schema["required"].([]string)
	seenNames := make(map[string]struct{}, len(structType.Fields.List))
	for _, field := range structType.Fields.List {
		if len(field.Names) == 1 && !ast.IsExported(field.Names[0].Name) {
			continue
		}
		name, optional, err := fieldJSONName(field)
		if err != nil {
			return nil, err
		}
		if _, exists := seenNames[name]; exists {
			return nil, fmt.Errorf("schemaextract: JSON 字段名 %q 重复", name)
		}
		seenNames[name] = struct{}{}
		fieldSchema, err := fieldSchemaWithTypes(field.Type, types, imports, resolving)
		if err != nil {
			return nil, fmt.Errorf("schemaextract: 字段 %q: %w", name, err)
		}
		properties[name] = fieldSchema
		if !optional {
			required = append(required, name)
		}
	}
	schema["required"] = required
	return json.Marshal(schema)
}

// fieldJSONName 解析字段 json tag：`json:"name"` 或 `json:"name,omitempty"`。
// 无 tag 拒绝（契约必须显式声明 JSON 名）。
func fieldJSONName(field *ast.Field) (name string, optional bool, err error) {
	if field.Tag == nil || len(field.Names) != 1 {
		return "", false, fmt.Errorf("schemaextract: 字段缺少 json tag")
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return "", false, fmt.Errorf("schemaextract: 字段 %q 的 json tag 无效", field.Names[0].Name)
	}
	value, ok := reflect.StructTag(tag).Lookup("json")
	if !ok {
		return "", false, fmt.Errorf("schemaextract: 字段 %q 缺少 json tag", field.Names[0].Name)
	}
	parts := strings.Split(value, ",")
	if parts[0] == "" || parts[0] == "-" {
		return "", false, fmt.Errorf("schemaextract: 字段 %q 的 json tag 无效", field.Names[0].Name)
	}
	switch len(parts) {
	case 1:
		return parts[0], false, nil
	case 2:
		if parts[1] != "omitempty" {
			return "", false, fmt.Errorf("schemaextract: 字段 %q 的 json tag 选项 %q 不支持", field.Names[0].Name, parts[1])
		}
		return parts[0], true, nil
	default:
		return "", false, fmt.Errorf("schemaextract: 字段 %q 的 json tag 选项不支持", field.Names[0].Name)
	}
}

// fieldSchema 把字段类型 AST 编译为 JSON Schema 片段。
func fieldSchemaWithTypes(expr ast.Expr, types map[string]*ast.StructType, imports map[string]string, resolving map[string]bool) (any, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		scalar, scalarErr := scalarSchema(t.Name)
		if scalarErr == nil {
			return scalar, nil
		}
		nested, ok := types[t.Name]
		if !ok {
			return nil, scalarErr
		}
		if resolving[t.Name] {
			return nil, fmt.Errorf("类型 %q 存在递归引用", t.Name)
		}
		resolving[t.Name] = true
		schema, err := structSchemaWithTypes(nested, types, imports, resolving)
		delete(resolving, t.Name)
		if err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(schema, &decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	case *ast.StarExpr:
		return fieldSchemaWithTypes(t.X, types, imports, resolving)
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok && imports[pkg.Name] == "time" && t.Sel.Name == "Time" {
			return map[string]any{"type": "string", "format": "date-time"}, nil
		}
		return nil, fmt.Errorf("不支持的标识类型 %s.%s", pkgName(t.X), t.Sel.Name)
	case *ast.ArrayType:
		if element, ok := t.Elt.(*ast.Ident); ok && (element.Name == "byte" || element.Name == "uint8") {
			// 只有 []byte 在 encoding/json 中是 base64 字符串；[N]byte 应继续按定长数组处理。
			if t.Len == nil {
				return map[string]any{"type": "string", "contentEncoding": "base64"}, nil
			}
		}
		items, err := fieldSchemaWithTypes(t.Elt, types, imports, resolving)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"type": "array", "items": items}
		if t.Len != nil {
			literal, ok := t.Len.(*ast.BasicLit)
			if !ok || literal.Kind.String() != "INT" {
				return nil, fmt.Errorf("schemaextract: 定长数组长度必须是整数常量")
			}
			length, err := strconv.Atoi(literal.Value)
			if err != nil || length < 0 {
				return nil, fmt.Errorf("schemaextract: 定长数组长度无效")
			}
			result["minItems"] = length
			result["maxItems"] = length
		}
		return result, nil
	case *ast.StructType:
		schema, err := structSchemaWithTypes(t, types, imports, resolving)
		if err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(schema, &decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("不支持的字段类型 %T", expr)
	}
}

// scalarSchema 映射内置标量类型；未知类型拒绝（fail-closed）。
func scalarSchema(name string) (any, error) {
	switch name {
	case "string":
		return map[string]any{"type": "string"}, nil
	case "int8":
		return boundedIntegerSchema(int64(-1<<7), int64(1<<7-1)), nil
	case "int16":
		return boundedIntegerSchema(int64(-1<<15), int64(1<<15-1)), nil
	case "int32":
		return boundedIntegerSchema(int64(-1<<31), int64(1<<31-1)), nil
	case "int64":
		return boundedIntegerSchema(int64(-1<<63), int64(1<<63-1)), nil
	case "uint8":
		return boundedIntegerSchema(uint64(0), uint64(1<<8-1)), nil
	case "uint16":
		return boundedIntegerSchema(uint64(0), uint64(1<<16-1)), nil
	case "uint32":
		return boundedIntegerSchema(uint64(0), uint64(1<<32-1)), nil
	case "uint64":
		return boundedIntegerSchema(uint64(0), ^uint64(0)), nil
	case "int", "uint":
		return nil, fmt.Errorf("不支持平台相关整数类型 %q，请使用定宽整数", name)
	case "float32", "float64":
		return map[string]any{"type": "number"}, nil
	case "bool":
		return map[string]any{"type": "boolean"}, nil
	default:
		return nil, fmt.Errorf("未知类型 %q", name)
	}
}

func boundedIntegerSchema(minimum, maximum any) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

// pkgName 取 SelectorExpr 的包名。
func pkgName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return "?"
}
