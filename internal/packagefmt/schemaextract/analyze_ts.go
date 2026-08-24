package schemaextract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// tsFunctionPattern 匹配 TS 导出处理函数：export function hello(args: X)。
var tsFunctionPattern = regexp.MustCompile(`(?m)^export\s+async\s+function\s+(\w+)\(|^export\s+function\s+(\w+)\(`)

// commandRunner 抽象外部命令执行（测试注入用）。
type commandRunner func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

// AnalyzeTS 从 TypeScript guest 源码提取 capabilities。
// 约定：export function hello(args: HelloArgs) 导出 capability "包ID.hello"，
// 参数类型名 = <CapabilityName>Args；schema 由 ts-json-schema-generator
// （node 生态成熟工具）从类型生成，避免手写 TS 解析器。
func AnalyzeTS(ctx context.Context, source []byte, packageID, workDir string) ([]Capability, error) {
	return analyzeTS(ctx, source, packageID, workDir, execCommand)
}

func analyzeTS(ctx context.Context, source []byte, packageID, workDir string, run commandRunner) ([]Capability, error) {
	names := extractTSFunctions(source)
	if len(names) == 0 {
		return nil, nil
	}
	sourcePath := filepath.Join(workDir, "main.ts")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		return nil, err
	}
	capabilities := make([]Capability, 0, len(names))
	for _, name := range names {
		// 类型名约定：<CapabilityName>Args（首字母大写，与 Go 参数 struct 一致）。
		typeName := upperFirst(name) + "Args"
		output, err := run(ctx, workDir, "npx", "--yes", "ts-json-schema-generator",
			"--path", "main.ts", "--type", typeName, "--no-top-ref")
		if err != nil {
			return nil, fmt.Errorf("schemaextract: ts-json-schema-generator 失败（%s）：%w：%s", name, err, strings.TrimSpace(string(output)))
		}
		var schema map[string]any
		if err := json.Unmarshal(output, &schema); err != nil {
			return nil, fmt.Errorf("schemaextract: schema 输出非法：%w", err)
		}
		// tjsg 默认输出 additionalProperties:false，无需后处理加。
		normalized, err := json.Marshal(schema)
		if err != nil {
			return nil, err
		}
		capabilities = append(capabilities, Capability{
			ID:          packageID + "." + name,
			Description: tsFunctionDoc(source, name),
			InputSchema: normalized,
		})
	}
	return capabilities, nil
}

// extractTSFunctions 用正则提取导出小写函数名（TS 语法约定，无需完整 AST；
// 与 Go 提取器一致：小写函数名 = capability）。
func extractTSFunctions(source []byte) []string {
	var names []string
	seen := make(map[string]struct{})
	for _, match := range tsFunctionPattern.FindAllSubmatch(source, -1) {
		name := string(match[1])
		if name == "" {
			name = string(match[2])
		}
		if name == "" || !goArgsPattern.MatchString(name) {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// tsFunctionDoc 提取函数前的 JSDoc 首行（capability 描述）。
func tsFunctionDoc(source []byte, name string) string {
	lines := strings.Split(string(source), "\n")
	for index, line := range lines {
		if strings.Contains(line, "function "+name+"(") {
			for previous := index - 1; previous >= 0; previous-- {
				trimmed := strings.TrimSpace(lines[previous])
				if strings.HasPrefix(trimmed, "//") {
					return strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
				}
				if trimmed != "" && !strings.HasPrefix(trimmed, "*") {
					break
				}
			}
		}
	}
	return ""
}

// upperFirst 将首字母大写（TS 参数类型名约定）。
func upperFirst(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// execCommand 执行外部命令并捕获输出。
func execCommand(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	return command.CombinedOutput()
}
