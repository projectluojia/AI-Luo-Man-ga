package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/authorization"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const invokeTimeout = 30 * time.Second

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
	deadline := time.Now().UTC().Add(invokeTimeout)
	if parentDeadline, ok := request.Context().Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	defer cancel()
	resolved, err := s.resolveWebIdentity(ctx, webIdentity)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			access.WriteJSON(writer, http.StatusGatewayTimeout, publicerror.Capability(err))
		case errors.Is(err, access.ErrHubConfiguration):
			observe.Error(ctx, "Capability 身份解析未配置", err)
			access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "identity_unavailable", "message": "身份服务暂不可用"})
		case errors.Is(err, identity.ErrUserDisabled), errors.Is(err, access.ErrMembershipRequired):
			observe.Warn(ctx, "Web 用户不具备当前 App 成员资格")
			access.WriteJSON(writer, http.StatusForbidden, map[string]string{"code": "permission_denied", "message": "当前用户无权调用 Capability"})
		case errors.Is(err, identity.ErrNotFound), errors.Is(err, identity.ErrInvalid):
			observe.Warn(ctx, "Web 用户身份未绑定")
			access.WriteJSON(writer, http.StatusUnauthorized, map[string]string{"code": "authentication_required", "message": "请先完成身份绑定"})
		case errors.Is(err, access.ErrIdentityContextInvalid), errors.Is(err, access.ErrAppMismatch):
			observe.Error(ctx, "身份解析器返回非法上下文", err)
			access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "身份认证服务异常"})
		default:
			observe.Error(ctx, "身份服务解析失败", err)
			access.WriteJSON(writer, http.StatusServiceUnavailable, map[string]string{"code": "identity_unavailable", "message": "身份服务暂不可用"})
		}
		return
	}
	var envelope struct {
		Input json.RawMessage `json:"input"`
	}
	if !access.DecodeJSONBody(writer, request, &envelope, 64<<10) {
		observe.Warn(request.Context(), "Capability 调用请求体解析失败",
			observe.StringAttr("capability_id", capabilityID),
		)
		return
	}
	if len(envelope.Input) == 0 {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "input 不能为空"})
		return
	}
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
		UserID:         resolved.UserID,
		SessionID:      webIdentity.PlatformSessionID,
		Deadline:       deadline,
		IdempotencyKey: request.Header.Get("Idempotency-Key"),
		ConfirmationID: request.Header.Get("X-Confirmation-ID"),
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
	var invocationError loader.InvocationError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, contracts.ErrDeadlineExceeded):
		access.WriteJSON(writer, http.StatusGatewayTimeout, publicerror.Capability(err))
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
	case errors.Is(err, idempotency.ErrInvalidRequest):
		observe.Warn(request.Context(), "Capability 调用幂等键无效",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusBadRequest, publicerror.Capability(err))
	case errors.Is(err, idempotency.ErrKeyConflict):
		observe.Warn(request.Context(), "Capability 调用幂等键冲突",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusConflict, publicerror.Capability(err))
	case errors.Is(err, runtime.ErrConfirmationRequired):
		observe.Warn(request.Context(), "Capability 需要受治理确认",
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusConflict, map[string]string{"code": "confirmation_required", "message": "该 Capability 需要确认"})
	case errors.Is(err, authorization.ErrDenied):
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
	case errors.As(err, &invocationError):
		public := publicerror.Capability(err)
		observe.Warn(request.Context(), "Capability 运行时返回结构化错误",
			observe.StringAttr("capability_id", capabilityID),
			observe.StringAttr("error_code", public.Code),
		)
		access.WriteJSON(writer, http.StatusServiceUnavailable, public)
	default:
		observe.Error(request.Context(), "Capability 调用失败", err,
			observe.StringAttr("capability_id", capabilityID),
		)
		access.WriteJSON(writer, http.StatusInternalServerError, map[string]string{"code": "internal_error", "message": "Capability 调用失败"})
	}
}
