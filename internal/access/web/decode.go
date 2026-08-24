package web

import (
	"encoding/json"
	"net/http"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
)

// decodeJSONBody 严格解码请求体：64 KiB 大小限制 + 拒绝未知字段 + 拒绝尾随
// 内容。失败时写出统一 400 响应并返回 false（web 各 handler 共用样板）。
func decodeJSONBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体不是有效的 JSON 对象"})
		return false
	}
	if err := jsonutil.EnsureEOF(decoder); err != nil {
		access.WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体只能包含一个 JSON 对象"})
		return false
	}
	return true
}
