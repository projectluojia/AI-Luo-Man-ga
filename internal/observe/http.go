package observe

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func HTTPMiddleware(component string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		requestID := validIDOrNew(request.Header.Get("X-Request-ID"))
		traceID, parentSpanID, ok := ParseTraceparent(request.Header.Get("traceparent"))
		if !ok {
			traceID = newTraceID()
		}
		ctx := With(request.Context(),
			slog.String("component", component),
			slog.String("request_id", requestID),
			slog.String("trace_id", traceID),
			slog.String("method", request.Method),
			slog.String("path", request.URL.Path),
		)
		if parentSpanID != "" {
			ctx = With(ctx, slog.String("span_id", parentSpanID))
		}
		ctx, span := StartSpan(ctx, "http.server")
		var spanErr error
		defer func() {
			span.End(spanErr)
		}()
		request = request.WithContext(ctx)
		writer.Header().Set("X-Request-ID", requestID)
		writer.Header().Set("X-Trace-ID", traceID)
		writer.Header().Set("traceparent", Traceparent(ctx))
		capture := &responseCapture{ResponseWriter: writer}
		Debug(ctx, "开始处理网页请求",
			slog.String("remote_ip", remoteIP(request.RemoteAddr)),
			slog.String("user_agent", request.UserAgent()),
		)
		defer func() {
			if recovered := recover(); recovered != nil {
				spanErr = fmt.Errorf("http panic")
				Error(ctx, "处理网页请求时发生未捕获异常", fmt.Errorf("%v", recovered),
					slog.Int("status_code", http.StatusInternalServerError),
					Duration(started),
				)
				panic(recovered)
			}
			status := capture.status
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []slog.Attr{
				slog.Int("status_code", status),
				slog.Int64("response_bytes", capture.bytes),
				slog.String("route", request.Pattern),
				Duration(started),
			}
			if capture.writeErr != nil {
				Error(ctx, "网页响应写入失败", capture.writeErr, attrs...)
			}
			switch {
			case status >= 500:
				spanErr = fmt.Errorf("http status 5xx")
				Error(ctx, "网页请求处理失败", nil, attrs...)
			case status >= 400:
				Warn(ctx, "网页请求未成功", attrs...)
			default:
				Info(ctx, "网页请求处理完成", attrs...)
			}
			DefaultMetrics().ObserveHTTPRequest(status, time.Since(started))
		}()
		next.ServeHTTP(capture, request)
	})
}

type responseCapture struct {
	http.ResponseWriter
	status   int
	bytes    int64
	writeErr error
}

func (w *responseCapture) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseCapture) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(payload)
	w.bytes += int64(written)
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return written, err
}

func (w *responseCapture) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseCapture) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func validIDOrNew(value string) string {
	value = strings.TrimSpace(value)
	if validRequestID(value) {
		return value
	}
	return uuid.NewString()
}

func remoteIP(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		return host
	}
	return address
}
