package observe_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

func TestHTTPMiddlewarePropagatesCorrelationIDs(t *testing.T) {
	var requestID string
	var traceID string
	handler := observe.HTTPMiddleware("test_access", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID = observe.String(request.Context(), "request_id")
		traceID = observe.String(request.Context(), "trace_id")
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "request-123")
	request.Header.Set("X-Trace-ID", "0123456789abcdef0123456789abcdef")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if requestID != "request-123" || traceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("请求上下文关联标识错误：request_id=%q trace_id=%q", requestID, traceID)
	}
	if response.Header().Get("X-Request-ID") != requestID || response.Header().Get("X-Trace-ID") != traceID {
		t.Fatalf("响应关联标识错误：headers=%v", response.Header())
	}
	if response.Header().Get("traceparent") == "" {
		t.Fatalf("响应缺少 W3C traceparent：headers=%v", response.Header())
	}
}

func TestHTTPMiddlewareAcceptsValidTraceparentAndRejectsMalformedTraceID(t *testing.T) {
	var traceID string
	var parentSpanID string
	handler := observe.HTTPMiddleware("test_access", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		traceID = observe.String(request.Context(), "trace_id")
		parentSpanID = observe.String(request.Context(), "parent_span_id")
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if traceID != "11111111111111111111111111111111" || parentSpanID != "2222222222222222" {
		t.Fatalf("追踪上下文=%q/%q", traceID, parentSpanID)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Trace-ID", "secret-or-malformed")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Header().Get("X-Trace-ID") == "secret-or-malformed" || len(response.Header().Get("X-Trace-ID")) != 32 {
		t.Fatalf("畸形追踪标识未被替换：%v", response.Header())
	}
}

func TestCopyPreservesFieldsWithoutSharingStorage(t *testing.T) {
	source := observe.With(t.Context(), observe.StringAttr("request_id", "request-1"), observe.StringAttr("trace_id", "trace-1"))
	target := observe.Copy(source, t.Context())
	updated := observe.With(source, observe.StringAttr("request_id", "request-2"))

	if observe.String(target, "request_id") != "request-1" || observe.String(target, "trace_id") != "trace-1" {
		t.Fatalf("复制后的上下文字段不完整：%v", observe.Fields(target))
	}
	if observe.String(updated, "request_id") != "request-2" || observe.String(target, "request_id") != "request-1" {
		t.Fatal("上下文字段发生了意外共享")
	}
	requestIDCount := 0
	for _, field := range observe.Fields(updated) {
		if field.Key == "request_id" {
			requestIDCount++
		}
	}
	if requestIDCount != 1 {
		t.Fatalf("覆盖上下文字段后仍存在重复键：%v", observe.Fields(updated))
	}
}
