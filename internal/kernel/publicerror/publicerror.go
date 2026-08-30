package publicerror

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
)

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func Capability(err error) Error {
	var invocationError loader.InvocationError
	switch {
	case err == nil:
		return Error{}
	case errors.Is(err, context.Canceled):
		return capabilityCodeTable["cancelled"]
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, contracts.ErrDeadlineExceeded):
		return capabilityCodeTable["deadline_exceeded"]
	case errors.Is(err, contracts.ErrMissingAppID),
		errors.Is(err, contracts.ErrMissingEchoID),
		errors.Is(err, contracts.ErrMissingRequestID):
		return capabilityCodeTable["invalid_request_context"]
	case errors.Is(err, runtime.ErrCapabilityDisabled):
		return capabilityCodeTable["capability_disabled"]
	case errors.Is(err, runtime.ErrAppPolicyUnavailable):
		return capabilityCodeTable["app_policy_unavailable"]
	case errors.Is(err, runtime.ErrCallDepthExceeded):
		return capabilityCodeTable["call_depth_exceeded"]
	case errors.Is(err, runtime.ErrCycleDetected):
		return capabilityCodeTable["cycle_detected"]
	case errors.Is(err, registry.ErrSchemaValidation):
		return capabilityCodeTable["invalid_arguments"]
	case errors.Is(err, registry.ErrPermissionDenied):
		return capabilityCodeTable["permission_denied"]
	case errors.Is(err, runtime.ErrIdempotencyKeyRequired):
		return capabilityCodeTable["idempotency_key_required"]
	case errors.Is(err, runtime.ErrIdempotencyUnavailable):
		return capabilityCodeTable["idempotency_unavailable"]
	case errors.Is(err, idempotency.ErrInvalidRequest):
		return capabilityCodeTable["invalid_idempotency_key"]
	case errors.Is(err, idempotency.ErrKeyConflict):
		return capabilityCodeTable["idempotency_conflict"]
	case errors.Is(err, idempotency.ErrOutcomeUnknown), errors.Is(err, idempotency.ErrLeaseLost):
		return capabilityCodeTable["idempotency_outcome_unknown"]
	case errors.Is(err, idempotency.ErrPreviousFailure):
		return capabilityCodeTable["idempotency_previous_failure"]
	case errors.Is(err, runtime.ErrConfirmationRequired):
		return capabilityCodeTable["confirmation_required"]
	case errors.As(err, &invocationError):
		return NormalizeCapability(Error{Code: invocationError.Code, Retryable: invocationError.Retryable})
	case errors.Is(err, loader.ErrRuntimeProtocol):
		return capabilityCodeTable["runtime_protocol_error"]
	case errors.Is(err, loader.ErrLoadFailed), errors.Is(err, loader.ErrUnavailable),
		errors.Is(err, loader.ErrShuttingDown), errors.Is(err, loader.ErrNotFound),
		errors.Is(err, loader.ErrUnsupportedMode), errors.Is(err, loader.ErrRuntimeBusy),
		errors.Is(err, loader.ErrProcessCleanup):
		return capabilityCodeTable["runtime_unavailable"]
	case errors.Is(err, registry.ErrCapabilityNotFound):
		return capabilityCodeTable["capability_unavailable"]
	default:
		var syntaxError *json.SyntaxError
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &syntaxError) || errors.As(err, &typeError) {
			return capabilityCodeTable["invalid_arguments"]
		}
		return Error{Code: "capability_failed", Message: "Capability 调用失败"}
	}
}

// capabilityCodeTable 是 Capability 稳定错误码 → 公共消息的唯一映射表。
// Capability(err) 与 NormalizeCapability 共用，避免双份映射漂移。
var capabilityCodeTable = map[string]Error{
	"cancelled":                    {Code: "cancelled", Message: "Capability 调用已取消"},
	"deadline_exceeded":            {Code: "deadline_exceeded", Message: "Capability 调用已超时", Retryable: true},
	"invalid_request_context":      {Code: "invalid_request_context", Message: "Capability 请求上下文无效"},
	"data_unavailable":             {Code: "data_unavailable", Message: "当前没有可用的权威数据", Retryable: true},
	"data_incomplete":              {Code: "data_incomplete", Message: "数据新鲜度信息不完整"},
	"data_non_authoritative":       {Code: "data_non_authoritative", Message: "当前数据不是权威来源，不能作为事实返回"},
	"data_expired":                 {Code: "data_expired", Message: "权威数据已过期，不能作为当前事实返回", Retryable: true},
	"invalid_arguments":            {Code: "invalid_arguments", Message: "Capability 参数无效"},
	"capability_disabled":          {Code: "capability_disabled", Message: "当前 App 未启用该 Capability"},
	"app_policy_unavailable":       {Code: "app_policy_unavailable", Message: "当前 App 策略暂时不可用", Retryable: true},
	"call_depth_exceeded":          {Code: "call_depth_exceeded", Message: "Capability 调用深度超过限制"},
	"cycle_detected":               {Code: "cycle_detected", Message: "Capability 调用形成了无进展循环"},
	"permission_denied":            {Code: "permission_denied", Message: "Capability 权限不足"},
	"idempotency_key_required":     {Code: "idempotency_key_required", Message: "Capability 副作用调用缺少幂等键"},
	"idempotency_unavailable":      {Code: "idempotency_unavailable", Message: "Capability 幂等保障暂时不可用", Retryable: true},
	"invalid_idempotency_key":      {Code: "invalid_idempotency_key", Message: "Capability 幂等参数无效"},
	"idempotency_conflict":         {Code: "idempotency_conflict", Message: "幂等键已用于不同的 Capability 请求"},
	"idempotency_outcome_unknown":  {Code: "idempotency_outcome_unknown", Message: "Capability 前次副作用结果无法安全确认"},
	"idempotency_previous_failure": {Code: "idempotency_previous_failure", Message: "Capability 前次副作用调用已失败"},
	"confirmation_required":        {Code: "confirmation_required", Message: "Capability 调用需要有效确认"},
	"runtime_unavailable":          {Code: "runtime_unavailable", Message: "Capability 运行时暂时不可用", Retryable: true},
	"runtime_protocol_error":       {Code: "runtime_protocol_error", Message: "Capability 运行时协议响应无效"},
	"capability_unavailable":       {Code: "capability_unavailable", Message: "Capability 当前不可用"},
}

func NormalizeCapability(value Error) Error {
	if normalized, ok := capabilityCodeTable[value.Code]; ok {
		return normalized
	}
	return Error{Code: "capability_failed", Message: "Capability 调用失败"}
}

func Agent(code string, retryable bool) Error {
	switch code {
	case "invalid_request":
		return Error{Code: "protocol_violation", Message: "Agent 协议请求无效"}
	case "cancelled":
		return Error{Code: "cancelled", Message: "Agent Run 已取消"}
	case "deadline_exceeded":
		return Error{Code: "deadline_exceeded", Message: "Agent Run 已超时", Retryable: retryable}
	case "provider_timeout":
		return Error{Code: "provider_timeout", Message: "模型服务响应超时", Retryable: retryable}
	case "rate_limited":
		return Error{Code: "rate_limited", Message: "模型服务请求过于频繁", Retryable: retryable}
	case "provider_unavailable":
		return Error{Code: "provider_unavailable", Message: "模型服务暂时不可用", Retryable: retryable}
	case "provider_rejected":
		return Error{Code: "provider_rejected", Message: "模型服务拒绝了请求"}
	case "provider_failure":
		return Error{Code: "provider_failure", Message: "模型服务请求失败"}
	case "provider_protocol_error":
		return Error{Code: "provider_protocol_error", Message: "模型服务响应无效"}
	case "budget_exceeded":
		return Error{Code: "budget_exceeded", Message: "Agent Run 已达到资源预算"}
	case "protocol_violation":
		return Error{Code: "protocol_violation", Message: "Agent 协议响应无效"}
	case "protocol_version_mismatch":
		return Error{Code: "protocol_version_mismatch", Message: "Agent 协议版本不兼容"}
	default:
		return Error{Code: "agent_run_failed", Message: "Agent Run 执行失败", Retryable: retryable}
	}
}

func Echo(code string) Error {
	switch code {
	case "cancelled":
		return Error{Code: "cancelled", Message: "Echo 已取消"}
	case "deadline_exceeded":
		return Error{Code: "deadline_exceeded", Message: "Echo 执行超时", Retryable: true}
	case "provider_timeout":
		return Error{Code: "provider_timeout", Message: "模型服务响应超时", Retryable: true}
	case "rate_limited":
		return Error{Code: "rate_limited", Message: "模型服务请求过于频繁", Retryable: true}
	case "provider_unavailable":
		return Error{Code: "provider_unavailable", Message: "模型服务暂时不可用", Retryable: true}
	case "provider_rejected":
		return Error{Code: "provider_rejected", Message: "模型服务拒绝了请求"}
	case "provider_failure":
		return Error{Code: "provider_failure", Message: "模型服务请求失败"}
	case "provider_protocol_error":
		return Error{Code: "provider_protocol_error", Message: "模型服务响应无效"}
	case "budget_exceeded":
		return Error{Code: "budget_exceeded", Message: "Agent Run 已达到资源预算"}
	case "agent_unavailable", "agent_start_failed", "agent_stream_failed":
		return Error{Code: "agent_unavailable", Message: "Agent 服务暂时不可用", Retryable: true}
	case "lease_lost":
		return Error{Code: "lease_lost", Message: "Run 执行租约已失效", Retryable: true}
	case "protocol_violation":
		return Error{Code: "protocol_violation", Message: "Agent 协议响应无效"}
	case "protocol_version_mismatch":
		return Error{Code: "protocol_version_mismatch", Message: "Agent 协议版本不兼容"}
	case "agent_run_failed":
		return Error{Code: "agent_run_failed", Message: "Agent Run 执行失败"}
	case "recovery_failed":
		return Error{Code: "recovery_failed", Message: "Run 无法安全恢复"}
	case "app_policy_unavailable":
		return Error{Code: "app_policy_unavailable", Message: "当前 App 策略暂时不可用", Retryable: true}
	case "app_disabled":
		return Error{Code: "app_disabled", Message: "当前 App 已停用"}
	case "context_unavailable":
		return Error{Code: "context_unavailable", Message: "Run 上下文暂时不可用", Retryable: true}
	case "context_budget_exceeded":
		return Error{Code: "context_budget_exceeded", Message: "Run 上下文超出装配预算"}
	case "event_delivery_failed":
		return Error{Code: "event_persistence_failed", Message: "Echo 事件持久化失败", Retryable: true}
	default:
		return Error{Code: "internal_error", Message: "Echo 执行失败"}
	}
}
