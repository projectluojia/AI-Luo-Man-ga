// Package capability 定义 Provider 对外暴露的 Capability 公共契约。
//
// 本包只包含可序列化的规格，不包含处理器、注册表、存储或运行时实现。
package capability

import "regexp"

var stableIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// IsStableID 校验包、组件和 Capability 共用的稳定标识。
func IsStableID(value string) bool {
	return len(value) <= 128 && stableIDPattern.MatchString(value)
}

const (
	// SideEffectNone 表示调用没有外部可见副作用。
	SideEffectNone = "none"
	// SideEffectRead 表示调用只读取数据。
	SideEffectRead = "read"
	// SideEffectWrite 表示调用写入受治理状态。
	SideEffectWrite = "write"
	// SideEffectExternal 表示调用触达外部系统。
	SideEffectExternal = "external"
)

// CapabilitySpec 是 Provider 对外暴露的受治理能力规格。
// Capability 是唯一的授权、版本、Schema、幂等和审计边界；具体实现由
// Provider Component 在运行时绑定，契约不暴露内部函数或实现名称。
type CapabilitySpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
}
