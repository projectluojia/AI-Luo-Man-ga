package access

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// IntakePublicError 把 Hub.Intake 的错误映射为稳定的公共错误（HTTP 状态、
// 错误码与安全中文消息）。Web 适配器、平台 ingress 与进程内平台适配器
// （QQ 等）共用，保证跨入口的公共错误契约一致，不泄露内部细节。
func IntakePublicError(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, ErrAppMismatch), errors.Is(err, ErrAnonymousOnly):
		return http.StatusForbidden, "platform_identity_rejected", "平台身份不被接受"
	case errors.Is(err, identity.ErrNotFound):
		return http.StatusUnauthorized, "identity_not_found", "平台身份未绑定"
	case errors.Is(err, identity.ErrUserDisabled):
		return http.StatusForbidden, "user_disabled", "用户已禁用"
	case errors.Is(err, session.ErrMessageConflict):
		return http.StatusConflict, "idempotency_conflict", "Idempotency-Key 已用于不同的创建请求"
	case errors.Is(err, identity.ErrInvalid):
		return http.StatusBadRequest, "invalid_platform_identity", "平台身份标识非法"
	case errors.Is(err, session.ErrInvalidMessage), errors.Is(err, session.ErrInvalidSession):
		return http.StatusBadRequest, "invalid_request", "标准消息校验失败"
	default:
		return http.StatusInternalServerError, "internal_error", "消息入库失败"
	}
}

// WriteIntakeError 把 Hub.Intake 的错误写为稳定的 HTTP 响应。
func WriteIntakeError(writer http.ResponseWriter, request *http.Request, err error) {
	status, code, message := IntakePublicError(err)
	if status >= 500 {
		observe.Error(request.Context(), "标准消息入库失败", err)
	} else {
		observe.Warn(request.Context(), "平台消息入库被拒绝", observe.StringAttr("reason", err.Error()))
	}
	writeError(writer, status, code, message)
}

// writeError 输出稳定的 JSON 错误响应。
func writeError(writer http.ResponseWriter, status int, code, message string) {
	WriteJSON(writer, status, map[string]string{"code": code, "message": message})
}

// WriteJSON 输出 JSON 响应（web 与平台 ingress 共用）。
func WriteJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// SecurityHeaders 为接入层 HTTP 处理器统一附加基础安全响应头。
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'")
		next.ServeHTTP(writer, request)
	})
}
