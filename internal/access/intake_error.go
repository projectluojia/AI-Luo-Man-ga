package access

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// WriteIntakeError 把 Hub.Intake 的错误映射为稳定的 HTTP 响应。
// 供 Web 适配器与平台 ingress 入口共用，保证跨入口的公共错误契约一致。
func WriteIntakeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, ErrAppMismatch), errors.Is(err, ErrAnonymousOnly):
		observe.Warn(request.Context(), "平台消息身份校验被拒绝", observe.StringAttr("reason", err.Error()))
		writeError(writer, http.StatusForbidden, "platform_identity_rejected", "平台身份不被接受")
	case errors.Is(err, identity.ErrNotFound):
		observe.Warn(request.Context(), "平台身份未绑定内部用户", observe.StringAttr("reason", err.Error()))
		writeError(writer, http.StatusUnauthorized, "identity_not_found", "平台身份未绑定")
	case errors.Is(err, identity.ErrUserDisabled):
		observe.Warn(request.Context(), "平台身份对应的用户已禁用", observe.StringAttr("reason", err.Error()))
		writeError(writer, http.StatusForbidden, "user_disabled", "用户已禁用")
	case errors.Is(err, session.ErrMessageConflict):
		observe.Warn(request.Context(), "平台消息去重键与既有消息冲突", observe.StringAttr("reason", err.Error()))
		writeError(writer, http.StatusConflict, "idempotency_conflict", "Idempotency-Key 已用于不同的创建请求")
	case errors.Is(err, identity.ErrInvalid):
		observe.Warn(request.Context(), "平台身份标识未通过规范校验", observe.StringAttr("reason", err.Error()))
		writeError(writer, http.StatusBadRequest, "invalid_platform_identity", "平台身份标识非法")
	case errors.Is(err, session.ErrInvalidMessage), errors.Is(err, session.ErrInvalidSession):
		observe.Warn(request.Context(), "平台消息未通过标准消息校验", observe.StringAttr("reason", err.Error()))
		writeError(writer, http.StatusBadRequest, "invalid_request", "标准消息校验失败")
	default:
		observe.Error(request.Context(), "标准消息入库失败", err)
		writeError(writer, http.StatusInternalServerError, "internal_error", "消息入库失败")
	}
}

// writeError 输出稳定的 JSON 错误响应。
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code, "message": message})
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
