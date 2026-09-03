// Package schemaextract 从 guest 源码自动提取 capability 契约，
// 使 ailuo pack 对纯计算包零声明：作者只写源码，清单自动生成。
// 支持的源码语言与提取方式：
//   - Go：go/ast 静态分析（标准库，不编译、不运行）
//   - TypeScript：调用 ts-json-schema-generator（node 生态成熟工具）
package schemaextract

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
)

// Capability 是提取出的 capability 契约（输入 schema 为 JSON Schema 文本）。
type Capability struct {
	ID          string
	Description string
	InputSchema json.RawMessage
}

// goArgsPattern 匹配 Go 导出处理函数（约定：包级小写函数名 = capability 名）。
var goArgsPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// AnalyzeGo 从 Go guest 源码提取 capabilities。
// 约定：包级小写函数 func hello(args HelloArgs) 导出 capability
// "包ID.hello"；参数 struct 的 json tag 与字段类型派生 JSON Schema。
func AnalyzeGo(source []byte, packageID string) ([]Capability, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", source, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	types := collectStructs(file)
	var capabilities []Capability
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Body == nil || function.Name == nil {
			continue
		}
		name := function.Name.Name
		if !goArgsPattern.MatchString(name) || len(function.Type.Params.List) != 1 {
			continue
		}
		structType, found := paramStruct(function.Type.Params.List[0], types)
		if !found {
			continue
		}
		schema, err := structSchemaWithTypes(structType, types, make(map[string]bool))
		if err != nil {
			return nil, err
		}
		capabilities = append(capabilities, Capability{
			ID:          packageID + "." + name,
			Description: functionDoc(function.Doc),
			InputSchema: schema,
		})
	}
	if len(capabilities) == 0 {
		return nil, nil
	}
	return capabilities, nil
}

// collectStructs 收集源码中所有具名 struct 类型（字段名 → 类型）。
func collectStructs(file *ast.File) map[string]*ast.StructType {
	types := make(map[string]*ast.StructType)
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name == nil {
				continue
			}
			if structType, ok := typeSpec.Type.(*ast.StructType); ok {
				types[typeSpec.Name.Name] = structType
			}
		}
	}
	return types
}

// paramStruct 解析函数第一个参数的类型为 struct（支持值或指针）。
func paramStruct(param *ast.Field, types map[string]*ast.StructType) (*ast.StructType, bool) {
	expr := param.Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return nil, false
	}
	structType, found := types[ident.Name]
	return structType, found
}

// functionDoc 返回函数注释首行（capability 描述）。
func functionDoc(group *ast.CommentGroup) string {
	if group == nil || len(group.List) == 0 {
		return ""
	}
	line := strings.TrimPrefix(group.List[0].Text, "//")
	return strings.TrimSpace(line)
}
