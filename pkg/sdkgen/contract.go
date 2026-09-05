// Package sdkgen 从包清单的 extensions 段生成消费方 SDK。
//
// 输入是内核 extensions 段的严格 JSON 形状（tools/service/capabilities），
// 输出是自包含源码：仅用标准库，不依赖内核 internal 包，可被外部项目
// 直接 go get 后复用。生成是唯一 SDK 路径，不保留手写 SDK 或兼容回退。
package sdkgen

import (
	"encoding/json"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

// capabilitySpec 是生成 SDK 所需的 capability 契约视图，JSON 形状与内核
// CapabilitySpec 一致（严格解码拒绝未知字段）。
type capabilitySpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	ServiceID            string   `json:"service_id"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
	ToolID               string   `json:"tool_id,omitempty"`
	// InputSchema 是 capability 输入 JSON Schema 的文本（与内核
	// CapabilitySpec.InputSchemaJSON 同形）。
	InputSchema string `json:"input_schema_json"`
}

// extensions 是 Manifest.Extensions 的严格形状：tools 与 service 由内核
// 解释（SDK 消费方只调用 capability），capabilities 驱动 SDK 生成。
type extensions struct {
	Tools        json.RawMessage  `json:"tools"`
	Service      json.RawMessage  `json:"service"`
	Capabilities []capabilitySpec `json:"capabilities"`
}

// decodeCapabilities 严格解码 extensions 段并校验生成所需的最小契约。
func decodeCapabilities(source json.RawMessage) ([]capabilitySpec, error) {
	var ext extensions
	if err := packmgr.DecodeStrictJSON(source, &ext); err != nil {
		return nil, fmt.Errorf("sdkgen: 解码契约失败: %w", err)
	}
	if len(ext.Capabilities) == 0 {
		return nil, fmt.Errorf("sdkgen: extensions 未声明任何 capability")
	}
	seen := make(map[string]struct{}, len(ext.Capabilities))
	for index := range ext.Capabilities {
		capability := &ext.Capabilities[index]
		if capability.ID == "" {
			return nil, fmt.Errorf("sdkgen: capabilities[%d] 缺少 id", index)
		}
		if len(capability.InputSchema) == 0 {
			return nil, fmt.Errorf("sdkgen: capability %q 缺少 input_schema_json", capability.ID)
		}
		// ID 唯一：重复 ID 会生成重复的方法名与输入类型名，产物编译不过。
		if _, exists := seen[capability.ID]; exists {
			return nil, fmt.Errorf("sdkgen: capability %q 重复声明", capability.ID)
		}
		seen[capability.ID] = struct{}{}
	}
	return ext.Capabilities, nil
}
