package observe

import (
	"fmt"
	"log/slog"
	"strings"
)

func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug", "调试":
		return slog.LevelDebug, nil
	case "info", "信息":
		return slog.LevelInfo, nil
	case "warn", "warning", "警告":
		return slog.LevelWarn, nil
	case "error", "错误":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("不支持的日志级别 %q", value)
	}
}

func Component(value string) slog.Attr {
	return slog.String("component", value)
}

func StringAttr(key, value string) slog.Attr {
	return slog.String(key, value)
}

func IntAttr(key string, value int) slog.Attr {
	return slog.Int(key, value)
}

func Int64Attr(key string, value int64) slog.Attr {
	return slog.Int64(key, value)
}

func BoolAttr(key string, value bool) slog.Attr {
	return slog.Bool(key, value)
}
