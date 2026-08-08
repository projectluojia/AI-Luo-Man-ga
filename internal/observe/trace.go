package observe

import (
	"context"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	traceIDPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

type Span struct {
	ctx     context.Context
	name    string
	started time.Time
	once    sync.Once
}

func StartSpan(ctx context.Context, name string) (context.Context, *Span) {
	traceID := String(ctx, "trace_id")
	if !validTraceID(traceID) {
		traceID = newTraceID()
	}
	parentSpanID := String(ctx, "span_id")
	spanID := newSpanID()
	attrs := []slog.Attr{
		slog.String("trace_id", traceID),
		slog.String("span_id", spanID),
	}
	if validSpanID(parentSpanID) {
		attrs = append(attrs, slog.String("parent_span_id", parentSpanID))
	}
	spanContext := With(ctx, attrs...)
	Debug(spanContext, "追踪 Span 已开始",
		slog.String("span_name", name),
	)
	return spanContext, &Span{ctx: spanContext, name: name, started: time.Now()}
}

func (s *Span) End(err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		attrs := []slog.Attr{
			slog.String("span_name", s.name),
			slog.String("span_status", "ok"),
			Duration(s.started),
		}
		if err != nil {
			attrs[1] = slog.String("span_status", "error")
			attrs = append(attrs,
				slog.String("error_class", errorClass(err)),
				slog.String("error_type", errorType(err)),
			)
		}
		Debug(s.ctx, "追踪 Span 已结束", attrs...)
	})
}

func ParseTraceparent(value string) (traceID, parentSpanID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[3]) != 2 {
		return "", "", false
	}
	if !validTraceID(parts[1]) || !validSpanID(parts[2]) || parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[3]); err != nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func Traceparent(ctx context.Context) string {
	traceID := String(ctx, "trace_id")
	spanID := String(ctx, "span_id")
	if !validTraceID(traceID) || !validSpanID(spanID) {
		return ""
	}
	return "00-" + traceID + "-" + spanID + "-01"
}

func validTraceID(value string) bool {
	return traceIDPattern.MatchString(value)
}

func validSpanID(value string) bool {
	return spanIDPattern.MatchString(value)
}

func validRequestID(value string) bool {
	return requestIDPattern.MatchString(value)
}

func newTraceID() string {
	return randomHex(16)
}

func newSpanID() string {
	return randomHex(8)
}

func randomHex(size int) string {
	value := strings.ReplaceAll(uuid.NewString(), "-", "")
	return value[:size*2]
}
