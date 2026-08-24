package sdkgen

import (
	"encoding/json"
	"fmt"
	"go/format"
	"strings"
)

// Language 是目标 SDK 语言。
type Language string

const (
	LanguageGo         Language = "go"
	LanguagePython     Language = "python"
	LanguageTypeScript Language = "typescript"
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
		if err := ValidateCapabilityID(capability.ID); err != nil {
			return nil, err
		}
		inputName := typeName(capability.ID, options.PackageID)
		model, err := schemaType(json.RawMessage(capability.InputSchema), inputName)
		if err != nil {
			return nil, fmt.Errorf("sdkgen: capability %q: %w", capability.ID, err)
		}
		if err := ValidateFieldNames(model); err != nil {
			return nil, fmt.Errorf("sdkgen: capability %q: %w", capability.ID, err)
		}
		models[capability.ID] = model
	}
	switch options.Language {
	case LanguageGo:
		generated := emitGo(options.PackageID, capabilities, models)
		// go/format（gofmt 核心）：生成的 Go 源码统一为标准格式；格式化失败
		// 说明 emitter 生成语法错误，fail-closed 报错而非产出畸形代码。
		formatted, formatErr := format.Source(generated[0].Code)
		if formatErr != nil {
			return nil, fmt.Errorf("sdkgen: Go 生成代码格式化失败: %w", formatErr)
		}
		generated[0].Code = formatted
		return generated, nil
	case LanguagePython:
		return emitPython(options.PackageID, capabilities, models), nil
	case LanguageTypeScript:
		return emitTypeScript(options.PackageID, capabilities, models), nil
	default:
		return nil, fmt.Errorf("sdkgen: 不支持的语言 %q", options.Language)
	}
}

// packageName 派生 Go 包名/Python 模块名：取包 ID 第一段（campus.bus → campus）。
func packageName(packageID string) string {
	return strings.Split(packageID, ".")[0]
}
