package schemaextract

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"reflect"
	"strings"
)

// structSchema 把 struct AST 编译为 JSON Schema（object + additionalProperties:false）。
func structSchema(structType *ast.StructType) (json.RawMessage, error) {
	return structSchemaWithTypes(structType, nil, make(map[string]bool))
}

func structSchemaWithTypes(structType *ast.StructType, types map[string]*ast.StructType, resolving map[string]bool) (json.RawMessage, error) {
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
	for _, field := range structType.Fields.List {
		name, optional, err := fieldJSONName(field)
		if err != nil {
			return nil, err
		}
		fieldSchema, err := fieldSchemaWithTypes(field.Type, types, resolving)
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
	if field.Tag == nil || len(field.Names) == 0 {
		return "", false, fmt.Errorf("schemaextract: 字段缺少 json tag")
	}
	tag := strings.Trim(field.Tag.Value, "`")
	value, ok := reflect.StructTag(tag).Lookup("json")
	if !ok {
		return "", false, fmt.Errorf("schemaextract: 字段 %q 缺少 json tag", field.Names[0].Name)
	}
	parts := strings.Split(value, ",")
	if parts[0] == "" || parts[0] == "-" {
		return "", false, fmt.Errorf("schemaextract: 字段 %q 的 json tag 无效", field.Names[0].Name)
	}
	optional = len(parts) > 1 && parts[1] == "omitempty"
	return parts[0], optional, nil
}

// fieldSchema 把字段类型 AST 编译为 JSON Schema 片段。
func fieldSchema(expr ast.Expr) (any, error) {
	return fieldSchemaWithTypes(expr, nil, make(map[string]bool))
}

func fieldSchemaWithTypes(expr ast.Expr, types map[string]*ast.StructType, resolving map[string]bool) (any, error) {
	switch t := expr.(type) {
	case *ast.Ident:
		if schema, err := scalarSchema(t.Name); err == nil {
			return schema, nil
		}
		nested, ok := types[t.Name]
		if !ok {
			return nil, fmt.Errorf("未知类型 %q", t.Name)
		}
		if resolving[t.Name] {
			return nil, fmt.Errorf("类型 %q 存在递归引用", t.Name)
		}
		resolving[t.Name] = true
		schema, err := structSchemaWithTypes(nested, types, resolving)
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
		return fieldSchemaWithTypes(t.X, types, resolving)
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok && pkg.Name == "time" && t.Sel.Name == "Time" {
			return map[string]any{"type": "string", "format": "date-time"}, nil
		}
		return nil, fmt.Errorf("不支持的标识类型 %s.%s", pkgName(t.X), t.Sel.Name)
	case *ast.ArrayType:
		items, err := fieldSchemaWithTypes(t.Elt, types, resolving)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case *ast.StructType:
		schema, err := structSchemaWithTypes(t, types, resolving)
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
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
		return map[string]any{"type": "integer"}, nil
	case "float32", "float64":
		return map[string]any{"type": "number"}, nil
	case "bool":
		return map[string]any{"type": "boolean"}, nil
	default:
		return nil, fmt.Errorf("未知类型 %q", name)
	}
}

// pkgName 取 SelectorExpr 的包名。
func pkgName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return "?"
}
