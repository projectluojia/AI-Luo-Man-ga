package observe

import "regexp"

// 仅识别明确的凭据格式；任意个人信息和业务正文仍必须在调用点禁止记录。
var credentialTextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:password|passwd|secret|(?:access[_-]?|refresh[_-]?)?token|api[_-]?key|authorization|cookie|credential)["']?\s*[:=]\s*\S`),
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+\S+`),
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{0,31}://[^\s/@]+@`),
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,})`),
}

func sanitizeText(value string, maxLength int) string {
	for _, pattern := range credentialTextPatterns {
		if pattern.MatchString(value) {
			// 整个值替换，避免只遮住引号、多行凭据或截断后的一部分。
			return redactedValue
		}
	}
	return truncate(value, maxLength)
}
