package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// invokeCapability 是消费方 SDK 的同步 capability 入口：
// POST /api/v1/capabilities/{capability_id}/invoke，请求体为 {"input": <payload>}，
// 响应为 {"capability_id": ..., "result": <value>}。治理（App 策略、权限收窄、
// Schema 校验、幂等/确认）由 Dispatcher 统一执行，本层只做 HTTP 信封与鉴权。
func (s *Server) invokeCapability(writer http.ResponseWriter, request *http.Request) {
	capabilityID := request.PathValue("capability_id")
	if s.dispatcher == nil {
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{
			"code": "capability_invoke_unavailable", "message": "Capability 调用暂不可用",
		})
		return
	}
	webIdentity, authenticated := s.authenticateWeb(writer, request)
	if !authenticated {
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var envelope struct {
		Input json.RawMessage `json:"input"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		observe.Warn(request.Context(), "Capability 调用请求体解析失败",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体必须是包含 input 的 JSON 对象"})
		return
	}
	if err := jsonutil.EnsureEOF(decoder); err != nil {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体只能包含一个 JSON 对象"})
		return
	}
	if len(envelope.Input) == 0 {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "input 不能为空"})
		return
	}
	ctx := request.Context()
	requestID := observe.String(ctx, "request_id")
	echoID := request.Header.Get("X-Echo-ID")
	if echoID == "" {
		echoID = requestID
	}
	invokeContext := contracts.RequestContext{
		AppID:          s.appID,
		EchoID:         echoID,
		RequestID:      requestID,
		TraceID:        observe.String(ctx, "trace_id"),
		UserID:         webIdentity.PlatformUserID,
		IdempotencyKey: request.Header.Get("Idempotency-Key"),
	}
	result, err := s.dispatcher.InvokeCapability(ctx, invokeContext, capabilityID, envelope.Input)
	if err != nil {
		writeInvokeError(writer, request, capabilityID, err)
		return
	}
	access.WriteJSON(writer, http.StatusOK, map[string]any{"capability_id": capabilityID, "result": result})
}

// writeInvokeError 将 Dispatcher 治理错误映射为稳定的 HTTP 错误响应，
// 不泄露内部错误细节（错误消息、堆栈、SQL、Provider 响应）。
func writeInvokeError(writer http.ResponseWriter, request *http.Request, capabilityID string, err error) {
	switch {
	case errors.Is(err, registry.ErrCapabilityNotFound), errors.Is(err, runtime.ErrCapabilityDisabled):
		observe.Warn(request.Context(), "Capability 不存在或未启用",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusNotFound, map[string]string{"code": "capability_not_found", "message": "Capability 不存在或未启用"})
	case errors.Is(err, registry.ErrSchemaValidation):
		observe.Warn(request.Context(), "Capability 输入未通过 Schema 校验",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_input", "message": "输入不满足 Capability 的 Schema"})
	case errors.Is(err, runtime.ErrIdempotencyKeyRequired):
		observe.Warn(request.Context(), "Capability 写调用缺少幂等键",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "idempotency_key_required", "message": "写操作必须携带 Idempotency-Key"})
	case errors.Is(err, runtime.ErrConfirmationRequired):
		observe.Warn(request.Context(), "Capability 需要受治理确认",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "confirmation_required", "message": "该 Capability 需要确认"})
	case errors.Is(err, registry.ErrPermissionDenied):
		observe.Warn(request.Context(), "Capability 调用权限不足",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusForbidden, map[string]string{"code": "permission_denied", "message": "当前 App 无权调用该 Capability"})
	case errors.Is(err, runtime.ErrAppPolicyUnavailable):
		observe.Error(request.Context(), "读取 App Capability 策略失败", err)
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "app_policy_unavailable", "message": "当前 App 策略暂时不可用"})
	case errors.Is(err, runtime.ErrIdempotencyUnavailable):
		observe.Error(request.Context(), "幂等存储不可用", err)
		access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "idempotency_unavailable", "message": "幂等存储暂时不可用"})
	default:
		observe.Error(request.Context(), "Capability 调用失败", err,
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Capability 调用失败"})
	}
}
