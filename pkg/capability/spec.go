// Package capability 定义 AI珞 Service、Tool 和 Capability 的公共契约。
//
// 本包只包含可序列化的规格，不包含 Handler、Registry、Storage 或运行时实现。
package capability

import "regexp"

var stableIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// IsStableID 校验包、组件、Tool、Service 和 Capability 共用的稳定标识。
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

// ToolSpec 是可复用原子 Tool 的公共规格。
type ToolSpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
}

// CapabilitySpec 是 Service 对外暴露的受治理能力规格。
type CapabilitySpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	ServiceID            string   `json:"service_id"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
	// ToolID 声明该 Capability 直接执行的 Tool。空值表示 Capability 自行处理分发。
	ToolID string `json:"tool_id,omitempty"`
}

// ServiceSpec 是业务 Service 的公共规格。
type ServiceSpec struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	ToolDependencies     []string `json:"tool_dependencies,omitempty"`
	RequestedPermissions []string `json:"requested_permissions,omitempty"`
}
