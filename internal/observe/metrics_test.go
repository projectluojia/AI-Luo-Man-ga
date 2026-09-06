package observe_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

func TestMetricsExposeOnlyClosedLabelsAndExpectedValues(t *testing.T) {
	metrics := &observe.Metrics{}
	metrics.ObserveHTTPRequest(http.StatusCreated, 25*time.Millisecond)
	metrics.RunStarted()
	metrics.SetQueuedRuns(3)
	metrics.ObserveCapability(false, 10*time.Millisecond)
	metrics.ObserveStorage(true, time.Millisecond)
	metrics.ObserveRuntimeLoad(true, 2*time.Millisecond)
	metrics.ObserveRuntimeStop(false, 3*time.Millisecond)
	metrics.RuntimeCallStarted()
	metrics.AddExecutorUsage(18, 9)

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		`ailuo_http_responses_total{status_class="2xx"} 1`,
		"ailuo_runs_active 1",
		"ailuo_runs_queued 3",
		`ailuo_capability_calls_total{result="failure"} 1`,
		`ailuo_runtime_loads_total{result="success"} 1`,
		`ailuo_runtime_stops_total{result="failure"} 1`,
		"ailuo_runtime_calls_active 1",
		"ailuo_executor_execution_units_total 18",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("指标缺少 %q：\n%s", expected, body)
		}
	}
	if strings.Contains(body, "app_id") || strings.Contains(body, "echo_id") || strings.Contains(body, "run_id") {
		t.Fatalf("指标包含高基数标识：\n%s", body)
	}
}

func TestMetricsRejectNonGET(t *testing.T) {
	response := httptest.NewRecorder()
	(&observe.Metrics{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码=%d", response.Code)
	}
}
