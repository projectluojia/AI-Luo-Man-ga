// Package capability 定义 Provider 对外暴露的 Capability 公共契约。
//
// 本包只包含可序列化的规格，不包含处理器、注册表、存储或运行时实现。
package capability

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

var stableIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

// IsStableID 校验包、组件和 Capability 共用的稳定标识。
func IsStableID(value string) bool {
	return len(value) <= 128 && stableIDPattern.MatchString(value)
}

// EffectTarget 描述调用可能改变的边界，不表达是否有权调用。
const (
	EffectNone     = "none"
	EffectState    = "governed_state"
	EffectExternal = "external"
)

// ReplayPolicy 描述失败重试和结果重放规则。
const (
	ReplaySafe           = "safe"
	ReplayIdempotencyKey = "idempotency_key"
	ReplayNever          = "never"
)

// ConfirmationFloor 描述能力自身要求的最低确认级别。
const (
	ConfirmationPolicy   = "policy"
	ConfirmationRequired = "required"
)

const (
	PrincipalAny         = "any"
	PrincipalCurrentUser = "current_user"
)

// AuthorizationSpec 描述 Capability 如何把一次调用绑定到主体和资源。
// 该声明只定义可被 Core 验证的绑定，不允许包注入授权代码或策略脚本。
type AuthorizationSpec struct {
	ResourceType   string `json:"resource_type"`
	ResourceIDFrom string `json:"resource_id_from,omitempty"`
	Principal      string `json:"principal,omitempty"`
}

// ExecutionSpec 描述执行安全语义，不承担授权职责。
type ExecutionSpec struct {
	EffectTarget      string `json:"effect_target"`
	Replay            string `json:"replay"`
	ConfirmationFloor string `json:"confirmation_floor"`
}

// ValidateAuthorizationSpec 校验 Capability 的主体与资源绑定声明。
func ValidateAuthorizationSpec(spec AuthorizationSpec) error {
	if !IsStableID(spec.ResourceType) || spec.ResourceType == "any" ||
		(spec.ResourceIDFrom != "" && (len(spec.ResourceIDFrom) > 256 ||
			!strings.HasPrefix(spec.ResourceIDFrom, "/") || strings.ContainsRune(spec.ResourceIDFrom, '\x00'))) {
		return errors.New("invalid capability authorization")
	}
	if spec.Principal != "" && spec.Principal != PrincipalAny && spec.Principal != PrincipalCurrentUser {
		return errors.New("invalid capability authorization principal")
	}
	return nil
}

// ValidateExecutionSpec 校验执行安全语义。
func ValidateExecutionSpec(spec ExecutionSpec) error {
	if spec.EffectTarget != EffectNone && spec.EffectTarget != EffectState && spec.EffectTarget != EffectExternal {
		return errors.New("invalid capability effect target")
	}
	if spec.Replay != ReplaySafe && spec.Replay != ReplayIdempotencyKey && spec.Replay != ReplayNever {
		return errors.New("invalid capability replay policy")
	}
	if spec.ConfirmationFloor != ConfirmationPolicy && spec.ConfirmationFloor != ConfirmationRequired {
		return errors.New("invalid capability confirmation floor")
	}
	return nil
}

// ResourceScope 是授权可以收窄的资源集合。
type ResourceScope struct {
	Type     string   `json:"type"`
	IDs      []string `json:"ids,omitempty"`
	Relation string   `json:"relation,omitempty"`
}

// Grant 是授予主体调用一个 Capability 的持久授权实例。
type Grant struct {
	ID                 string        `json:"id"`
	AppID              string        `json:"app_id"`
	Principal          string        `json:"principal"`
	CapabilityID       string        `json:"capability_id"`
	Resource           ResourceScope `json:"resource"`
	NotBefore          time.Time     `json:"not_before,omitempty"`
	ExpiresAt          time.Time     `json:"expires_at"`
	MaxCalls           uint32        `json:"max_calls"`
	MaxCostMicrousd    uint64        `json:"max_cost_microusd"`
	Audience           string        `json:"audience,omitempty"`
	Delegable          bool          `json:"delegable"`
	MaxDelegationDepth uint16        `json:"max_delegation_depth"`
	PolicyRevision     string        `json:"policy_revision"`
}

// NormalizeGrant 规范化 Grant，保证集合比较具有确定结果。
func NormalizeGrant(grant Grant) (Grant, error) {
	if !IsStableID(grant.ID) || !IsStableID(grant.AppID) || grant.Principal == "" ||
		!IsStableID(grant.CapabilityID) || !IsStableID(grant.Resource.Type) ||
		grant.ExpiresAt.IsZero() || grant.MaxCalls == 0 ||
		(!grant.NotBefore.IsZero() && !grant.NotBefore.Before(grant.ExpiresAt)) {
		return Grant{}, errors.New("invalid capability grant")
	}
	if grant.Audience != "" && !IsStableID(grant.Audience) {
		return Grant{}, errors.New("invalid capability grant audience")
	}
	if grant.Resource.Relation != "" && !IsStableID(grant.Resource.Relation) {
		return Grant{}, errors.New("invalid capability grant relation")
	}
	ids := append([]string(nil), grant.Resource.IDs...)
	sort.Strings(ids)
	for i, id := range ids {
		if !IsStableID(id) || (i > 0 && ids[i-1] == id) {
			return Grant{}, errors.New("invalid capability grant resource ids")
		}
	}
	grant.Resource.IDs = ids
	return grant, nil
}

// GrantSubset 判断 child 是否不会扩大 parent 的权限。
func GrantSubset(child, parent Grant) bool {
	if child.AppID != parent.AppID || child.Principal != parent.Principal ||
		child.CapabilityID != parent.CapabilityID || child.Resource.Type != parent.Resource.Type ||
		child.Resource.Relation != parent.Resource.Relation || child.Audience == "" ||
		parent.Audience != "" && child.Audience != parent.Audience ||
		parent.MaxCalls < child.MaxCalls || child.ExpiresAt.After(parent.ExpiresAt) ||
		(!parent.NotBefore.IsZero() && child.NotBefore.Before(parent.NotBefore)) ||
		(!parent.Delegable && child.Delegable) ||
		child.MaxDelegationDepth > parent.MaxDelegationDepth ||
		(parent.MaxCostMicrousd != 0 && (child.MaxCostMicrousd == 0 || child.MaxCostMicrousd > parent.MaxCostMicrousd)) {
		return false
	}
	if len(parent.Resource.IDs) > 0 {
		if len(child.Resource.IDs) == 0 {
			return false
		}
		allowed := make(map[string]struct{}, len(parent.Resource.IDs))
		for _, id := range parent.Resource.IDs {
			allowed[id] = struct{}{}
		}
		for _, id := range child.Resource.IDs {
			if _, ok := allowed[id]; !ok {
				return false
			}
		}
	}
	// 空 ID 集合表示父 Grant 覆盖该资源类型的全部资源，子 Grant 可收窄到指定 ID。
	return true
}

// NarrowGrant 将授权收窄为 requested；requested 扩权时明确失败，不静默裁剪。
func NarrowGrant(parent, requested Grant) (Grant, error) {
	parent, err := NormalizeGrant(parent)
	if err != nil {
		return Grant{}, err
	}
	requested, err = NormalizeGrant(requested)
	if err != nil || !GrantSubset(requested, parent) {
		return Grant{}, errors.New("requested grant exceeds parent grant")
	}
	return requested, nil
}

// CapabilitySpec 是 Provider 对外暴露的受治理能力规格。
// Capability 是唯一的授权、版本、Schema、幂等和审计边界；具体实现由
// Provider Component 在运行时绑定，契约不暴露内部函数或实现名称。
type CapabilitySpec struct {
	ID              string            `json:"id"`
	Version         string            `json:"version"`
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	InputSchemaJSON string            `json:"input_schema_json"`
	Authorization   AuthorizationSpec `json:"authorization"`
	Execution       ExecutionSpec     `json:"execution"`
}
