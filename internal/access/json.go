package access

import (
	"encoding/json"
	"net/http"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
)

// DecodeJSONBody 严格解码一个有大小上限的 JSON 请求体。
func DecodeJSONBody(writer http.ResponseWriter, request *http.Request, target any, maxBytes int64) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体不是有效的 JSON 对象"})
		return false
	}
	if err := jsonutil.EnsureEOF(decoder); err != nil {
		WriteJSON(writer, http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "请求体只能包含一个 JSON 对象"})
		return false
	}
	return true
}
