package sdkgen

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Language 是目标 SDK 语言。
type Language string

const (
	LanguageGo     Language = "go"
	LanguagePython Language = "python"
)

// Options 控制 SDK 生成。
type Options struct {
	Language  Language
	PackageID string // 包清单 ID（如 campus.bus），决定生成的包名与类型名前缀
}

// Generated 是单个文件的生成结果。
type Generated struct {
	Path string // 相对文件名（如 campus/client.go）
	Code []byte
}

// Generate 从 extensions 段生成指定语言的 SDK 源码。
// source 是 packmgr.Manifest.Extensions 的原始字节；生成是唯一 SDK 路径，
// 任何无法严格解码的输入都返回错误，不产生部分产物。
func Generate(source json.RawMessage, options Options) ([]Generated, error) {
	if options.PackageID == "" {
		return nil, fmt.Errorf("sdkgen: Options.PackageID 不能为空")
	}
	capabilities, err := decodeCapabilities(source)
	if err != nil {
		return nil, err
	}
	models := make(map[string]*TypeModel, len(capabilities))
	for index := range capabilities {
		capability := &capabilities[index]
		inputName := inputTypeName(capability.ID, options.PackageID)
		model, err := schemaType(json.RawMessage(capability.InputSchema), inputName)
		if err != nil {
			return nil, fmt.Errorf("sdkgen: capability %q: %w", capability.ID, err)
		}
		models[capability.ID] = model
	}
	switch options.Language {
	case LanguageGo:
		return emitGo(options.PackageID, capabilities, models), nil
	case LanguagePython:
		return emitPython(options.PackageID, capabilities, models), nil
	default:
		return nil, fmt.Errorf("sdkgen: 不支持的语言 %q", options.Language)
	}
}

// packageName 派生 Go 包名/Python 模块名：取包 ID 第一段（campus.bus → campus）。
func packageName(packageID string) string {
	return strings.Split(packageID, ".")[0]
}

// inputTypeName 派生 capability 输入类型的具名：去掉包前缀后各段 Title 拼接，
// 再追加 Input。campus.bus.stops.search（包 campus）→ BusStopsSearchInput。
func inputTypeName(capabilityID, packageID string) string {
	return exportName(stripPackagePrefix(capabilityID, packageID)) + "Input"
}

// methodName 派生 capability 的调用函数名：inputTypeName 去掉 Input 后缀。
func methodName(capabilityID, packageID string) string {
	return exportName(stripPackagePrefix(capabilityID, packageID))
}

// stripPackagePrefix 去掉 capability ID 的包前缀（"campus."），无前缀时原样保留。
func stripPackagePrefix(capabilityID, packageID string) string {
	prefix := packageID + "."
	if strings.HasPrefix(capabilityID, prefix) {
		return strings.TrimPrefix(capabilityID, prefix)
	}
	return capabilityID
}

// exportName 将点分隔的标识符转为导出风格：每段首字母大写后拼接。
func exportName(identifier string) string {
	parts := strings.Split(identifier, ".")
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(upperFirst(part))
	}
	return builder.String()
}

// upperFirst 将首字母大写，其余原样保留。
func upperFirst(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// pythonMethodName 派生 Python 调用函数名：capability ID 分段以点分段
// （内核强制 StableLower：全小写字母数字，点/下划线/连字符分段）。
func pythonMethodName(capabilityID, packageID string) string {
	return strings.ReplaceAll(stripPackagePrefix(capabilityID, packageID), ".", "_")
}
