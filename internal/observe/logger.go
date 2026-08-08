package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const redactedValue = "[已脱敏]"

var privateFieldNames = map[string]struct{}{
	"arguments":      {},
	"body":           {},
	"content":        {},
	"input_message":  {},
	"message_body":   {},
	"messages":       {},
	"model_message":  {},
	"output_message": {},
	"payload":        {},
	"prompt":         {},
	"response_body":  {},
	"result_message": {},
	"child_result":   {},
	"task":           {},
	"error":          {},
	"reason":         {},
	"tool_arguments": {},
	"tool_result":    {},
	"user_message":   {},
}

type Config struct {
	Service        string
	Environment    string
	Level          slog.Level
	Format         string
	AddSource      bool
	MaxValueLength int
	Writer         io.Writer
}

type Logger struct {
	logger *slog.Logger
}

var defaultLogger atomic.Pointer[Logger]

func New(config Config) (*Logger, error) {
	if config.Service == "" {
		return nil, fmt.Errorf("日志配置缺少 service")
	}
	if config.Environment == "" {
		config.Environment = "development"
	}
	if config.Format == "" {
		config.Format = "console"
	}
	if config.MaxValueLength <= 0 {
		config.MaxValueLength = 4096
	}
	if config.Writer == nil {
		config.Writer = os.Stdout
	}
	if config.AddSource {
		return nil, fmt.Errorf("日志配置不允许输出源码路径")
	}
	options := &slog.HandlerOptions{Level: config.Level}
	var handler slog.Handler
	switch strings.ToLower(config.Format) {
	case "console", "text":
		options.ReplaceAttr = chineseConsoleAttrs
		handler = slog.NewTextHandler(config.Writer, options)
	case "json":
		handler = slog.NewJSONHandler(config.Writer, options)
	default:
		return nil, fmt.Errorf("不支持的日志格式 %q", config.Format)
	}
	handler = &sanitizingHandler{next: handler, maxValueLength: config.MaxValueLength}
	base := slog.New(handler).With(
		slog.String("service", config.Service),
		slog.String("environment", config.Environment),
		slog.Int("pid", os.Getpid()),
	)
	return &Logger{logger: base}, nil
}

func Configure(config Config) (*Logger, error) {
	logger, err := New(config)
	if err != nil {
		return nil, err
	}
	defaultLogger.Store(logger)
	slog.SetDefault(logger.logger)
	return logger, nil
}

func Default() *Logger {
	if logger := defaultLogger.Load(); logger != nil {
		return logger
	}
	logger, _ := New(Config{Service: "ailuo", Environment: "development", Format: "console"})
	if defaultLogger.CompareAndSwap(nil, logger) {
		return logger
	}
	return defaultLogger.Load()
}

func (l *Logger) Log(ctx context.Context, level slog.Level, message string, attrs ...slog.Attr) {
	all := mergeAttrs(Fields(ctx), attrs)
	l.logger.LogAttrs(ctx, level, message, all...)
}

func Debug(ctx context.Context, message string, attrs ...slog.Attr) {
	Default().Log(ctx, slog.LevelDebug, message, attrs...)
}

func Info(ctx context.Context, message string, attrs ...slog.Attr) {
	Default().Log(ctx, slog.LevelInfo, message, attrs...)
}

func Warn(ctx context.Context, message string, attrs ...slog.Attr) {
	Default().Log(ctx, slog.LevelWarn, message, attrs...)
}

func Error(ctx context.Context, message string, err error, attrs ...slog.Attr) {
	Default().Error(ctx, message, err, attrs...)
}

func (l *Logger) Error(ctx context.Context, message string, err error, attrs ...slog.Attr) {
	if err != nil {
		attrs = append(attrs,
			slog.String("error_class", errorClass(err)),
			slog.String("error_type", errorType(err)),
		)
	}
	l.Log(ctx, slog.LevelError, message, attrs...)
}

func errorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "internal"
	}
}

func errorType(err error) string {
	valueType := reflect.TypeOf(err)
	if valueType == nil {
		return ""
	}
	return valueType.String()
}

func Duration(start time.Time) slog.Attr {
	return slog.Int64("duration_ms", time.Since(start).Milliseconds())
}

func chineseConsoleAttrs(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) == 0 && attr.Key == slog.LevelKey {
		level, ok := attr.Value.Any().(slog.Level)
		if !ok {
			return attr
		}
		switch {
		case level <= slog.LevelDebug:
			attr.Value = slog.StringValue("调试")
		case level < slog.LevelWarn:
			attr.Value = slog.StringValue("信息")
		case level < slog.LevelError:
			attr.Value = slog.StringValue("警告")
		default:
			attr.Value = slog.StringValue("错误")
		}
	}
	return attr
}

type sanitizingHandler struct {
	next           slog.Handler
	maxValueLength int
}

func (h *sanitizingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *sanitizingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time.UTC(), record.Level, truncate(record.Message, h.maxValueLength), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(sanitizeAttr(attr, h.maxValueLength))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *sanitizingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		clean = append(clean, sanitizeAttr(attr, h.maxValueLength))
	}
	return &sanitizingHandler{next: h.next.WithAttrs(clean), maxValueLength: h.maxValueLength}
}

func (h *sanitizingHandler) WithGroup(name string) slog.Handler {
	return &sanitizingHandler{next: h.next.WithGroup(name), maxValueLength: h.maxValueLength}
}

func sanitizeAttr(attr slog.Attr, maxLength int) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if isSensitiveKey(attr.Key) && !isSafeNumericTokenCount(attr.Key, attr.Value) {
		return slog.String(attr.Key, redactedValue)
	}
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(truncate(attr.Value.String(), maxLength))
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(truncate(err.Error(), maxLength))
		}
	case slog.KindGroup:
		group := attr.Value.Group()
		clean := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			clean = append(clean, sanitizeAttr(child, maxLength))
		}
		attr.Value = slog.GroupValue(clean...)
	}
	return attr
}

func isSafeNumericTokenCount(key string, value slog.Value) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch normalized {
	case "input_tokens", "output_tokens", "total_tokens", "cached_tokens", "reasoning_tokens",
		"max_input_tokens", "max_output_tokens", "max_total_tokens", "token_count":
		return value.Kind() == slog.KindInt64 || value.Kind() == slog.KindUint64
	default:
		return false
	}
}

func isSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	if _, private := privateFieldNames[normalized]; private {
		return true
	}
	if strings.HasPrefix(normalized, "raw_") {
		return true
	}
	for _, suffix := range []string{"_arguments", "_body", "_content", "_messages", "_payload", "_prompt"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	for _, marker := range []string{"password", "passwd", "secret", "token", "api_key", "authorization", "cookie", "credential"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func truncate(value string, maxLength int) string {
	if utf8.RuneCountInString(value) <= maxLength {
		return value
	}
	return string([]rune(value)[:maxLength]) + "…[已截断]"
}
