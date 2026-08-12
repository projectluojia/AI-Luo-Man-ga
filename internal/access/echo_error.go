package access

import (
	"errors"
	"net/http"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// WriteEchoError 把 Echo 创建错误映射为稳定的 HTTP 响应。
// 供 Web 适配器与平台 ingress 入口共用，保证跨入口的公共错误契约一致。
func WriteEchoError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, kernelecho.ErrAppDisabled):
		writeError(writer, http.StatusServiceUnavailable, "app_disabled", "当前 App 已停用")
	case errors.Is(err, kernelecho.ErrAppConfigUnavailable):
		observe.Error(request.Context(), "读取 App 配置失败", err)
		writeError(writer, http.StatusServiceUnavailable, "app_config_unavailable", "当前 App 配置暂时不可用")
	case errors.Is(err, kernelecho.ErrQueueFull):
		observe.Warn(request.Context(), "Run 队列已达到配置容量")
		writer.Header().Set("Retry-After", "1")
		writeError(writer, http.StatusTooManyRequests, "queue_full", "当前任务队列已满，请稍后重试")
	case errors.Is(err, idempotency.ErrKeyConflict):
		observe.Warn(request.Context(), "Echo 创建幂等键与既有请求冲突")
		writeError(writer, http.StatusConflict, "idempotency_conflict", "Idempotency-Key 已用于不同的创建请求")
	default:
		observe.Error(request.Context(), "创建 Echo 失败", err)
		writeError(writer, http.StatusInternalServerError, "internal_error", "Echo 创建失败")
	}
}
