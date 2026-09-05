// Package sdkgen 从 Capability 契约生成消费方 SDK。
//
// 输入是 Capability 规格数组的严格 JSON，输出是自包含源码：仅用标准库，
// 不依赖内核 internal 包，可被外部项目直接复用。
package sdkgen

import (
	"encoding/json"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// decodeCapabilities 严格解码 Capability 数组并校验生成所需的最小契约。
func decodeCapabilities(source json.RawMessage) ([]capability.CapabilitySpec, error) {
	var capabilities []capability.CapabilitySpec
	if err := packagecontract.DecodeStrictJSON(source, &capabilities); err != nil {
		return nil, fmt.Errorf("sdkgen: 解码 Capability 契约失败: %w", err)
	}
	if len(capabilities) == 0 {
		return nil, fmt.Errorf("sdkgen: 未声明任何 Capability")
	}
	seen := make(map[string]struct{}, len(capabilities))
	for index := range capabilities {
		spec := &capabilities[index]
		if spec.ID == "" {
			return nil, fmt.Errorf("sdkgen: capabilities[%d] 缺少 id", index)
		}
		if len(spec.InputSchemaJSON) == 0 {
			return nil, fmt.Errorf("sdkgen: Capability %q 缺少 input_schema_json", spec.ID)
		}
		if _, exists := seen[spec.ID]; exists {
			return nil, fmt.Errorf("sdkgen: Capability %q 重复声明", spec.ID)
		}
		seen[spec.ID] = struct{}{}
	}
	return capabilities, nil
}
