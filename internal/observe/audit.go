package observe

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

func SanitizeAuditJSON(payload []byte, maxBytes int) []byte {
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return auditSummary(payload, "invalid_json")
	}
	clean := sanitizeJSONValue(value)
	encoded, err := json.Marshal(clean)
	if err != nil {
		return auditSummary(payload, "encode_failed")
	}
	if len(encoded) > maxBytes {
		return auditSummary(payload, "too_large")
	}
	return encoded
}

func sanitizeJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, child := range typed {
			if isSensitiveKey(key) {
				clean[key] = redactedValue
			} else {
				clean[key] = sanitizeJSONValue(child)
			}
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, child := range typed {
			clean[index] = sanitizeJSONValue(child)
		}
		return clean
	case string:
		return truncate(typed, 4096)
	default:
		return value
	}
}

func auditSummary(payload []byte, reason string) []byte {
	digest := sha256.Sum256(payload)
	encoded, _ := json.Marshal(map[string]any{
		"摘要原因":   reason,
		"原始字节数":  len(payload),
		"sha256": fmt.Sprintf("%x", digest[:]),
	})
	return encoded
}
