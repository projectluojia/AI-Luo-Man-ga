package observe

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var durationBuckets = [...]time.Duration{
	time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

type durationHistogram struct {
	count   atomic.Uint64
	sumNano atomic.Uint64
	buckets [len(durationBuckets)]atomic.Uint64
}

func (h *durationHistogram) observe(value time.Duration) {
	if value < 0 {
		value = 0
	}
	h.count.Add(1)
	h.sumNano.Add(uint64(value))
	for index, boundary := range durationBuckets {
		if value <= boundary {
			h.buckets[index].Add(1)
		}
	}
}

type outcomeCounters struct {
	success atomic.Uint64
	failure atomic.Uint64
}

func (c *outcomeCounters) add(success bool) {
	if success {
		c.success.Add(1)
	} else {
		c.failure.Add(1)
	}
}

// Metrics 只接受闭集标签，避免外部标识造成高基数或敏感信息泄露。
type Metrics struct {
	httpResponses [5]atomic.Uint64
	httpDuration  durationHistogram

	activeRuns       atomic.Int64
	queuedRuns       atomic.Int64
	runSucceeded     atomic.Uint64
	runFailed        atomic.Uint64
	runCancelled     atomic.Uint64
	runTimedOut      atomic.Uint64
	runDuration      durationHistogram
	firstToken       durationHistogram
	cancellations    atomic.Uint64
	runRetries       atomic.Uint64
	providerRetries  atomic.Uint64
	modelInputTokens atomic.Uint64
	modelOutputToken atomic.Uint64
	modelCost        atomic.Uint64

	capabilities outcomeCounters
	capability   durationHistogram
	tools        outcomeCounters
	tool         durationHistogram
	storage      outcomeCounters
	storageTime  durationHistogram
	runtimeLoads outcomeCounters
	runtimeLoad  durationHistogram
	runtimeStops outcomeCounters
	runtimeStop  durationHistogram
	runtimeCalls atomic.Int64
}

var processMetrics = &Metrics{}

func DefaultMetrics() *Metrics {
	return processMetrics
}

func (m *Metrics) ObserveHTTPRequest(status int, duration time.Duration) {
	class := status/100 - 1
	if class < 0 || class >= len(m.httpResponses) {
		class = len(m.httpResponses) - 1
	}
	m.httpResponses[class].Add(1)
	m.httpDuration.observe(duration)
}

func (m *Metrics) RunStarted() {
	m.activeRuns.Add(1)
}

func (m *Metrics) RunStopped() {
	m.activeRuns.Add(-1)
}

func (m *Metrics) SetQueuedRuns(value int) {
	if value < 0 {
		value = 0
	}
	m.queuedRuns.Store(int64(value))
}

func (m *Metrics) QueueAdded() {
	m.queuedRuns.Add(1)
}

func (m *Metrics) QueueRemoved() {
	for {
		current := m.queuedRuns.Load()
		if current <= 0 || m.queuedRuns.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (m *Metrics) RunRetry() {
	m.runRetries.Add(1)
}

func (m *Metrics) ObserveRun(status string, duration time.Duration) {
	switch status {
	case "succeeded":
		m.runSucceeded.Add(1)
	case "cancelled":
		m.runCancelled.Add(1)
	case "timed_out":
		m.runTimedOut.Add(1)
	default:
		m.runFailed.Add(1)
	}
	m.runDuration.observe(duration)
}

func (m *Metrics) ObserveFirstToken(duration time.Duration) {
	m.firstToken.observe(duration)
}

func (m *Metrics) Cancellation() {
	m.cancellations.Add(1)
}

func (m *Metrics) ProviderRetry() {
	m.providerRetries.Add(1)
}

func (m *Metrics) AddModelUsage(inputTokens, outputTokens, costMicrousd uint64) {
	m.modelInputTokens.Add(inputTokens)
	m.modelOutputToken.Add(outputTokens)
	m.modelCost.Add(costMicrousd)
}

func (m *Metrics) ObserveCapability(success bool, duration time.Duration) {
	if success {
		m.capabilities.success.Add(1)
	} else {
		m.capabilities.failure.Add(1)
	}
	m.capability.observe(duration)
}

func (m *Metrics) ObserveTool(success bool, duration time.Duration) {
	if success {
		m.tools.success.Add(1)
	} else {
		m.tools.failure.Add(1)
	}
	m.tool.observe(duration)
}

func (m *Metrics) ObserveStorage(success bool, duration time.Duration) {
	if success {
		m.storage.success.Add(1)
	} else {
		m.storage.failure.Add(1)
	}
	m.storageTime.observe(duration)
}

func (m *Metrics) ObserveRuntimeLoad(success bool, duration time.Duration) {
	m.runtimeLoads.add(success)
	m.runtimeLoad.observe(duration)
}

func (m *Metrics) ObserveRuntimeStop(success bool, duration time.Duration) {
	m.runtimeStops.add(success)
	m.runtimeStop.observe(duration)
}

func (m *Metrics) RuntimeCallStarted() {
	m.runtimeCalls.Add(1)
}

func (m *Metrics) RuntimeCallStopped() {
	m.runtimeCalls.Add(-1)
}

func (m *Metrics) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	output := &strings.Builder{}
	for index := range m.httpResponses {
		writeLabeledMetric(output, "ailuo_http_responses_total", "status_class", strconv.Itoa(index+1)+"xx", m.httpResponses[index].Load())
	}
	writeHistogram(output, "ailuo_http_request_duration_seconds", &m.httpDuration)
	writeMetric(output, "ailuo_runs_active", m.activeRuns.Load())
	writeMetric(output, "ailuo_runs_queued", m.queuedRuns.Load())
	writeLabeledMetric(output, "ailuo_runs_total", "status", "succeeded", m.runSucceeded.Load())
	writeLabeledMetric(output, "ailuo_runs_total", "status", "failed", m.runFailed.Load())
	writeLabeledMetric(output, "ailuo_runs_total", "status", "cancelled", m.runCancelled.Load())
	writeLabeledMetric(output, "ailuo_runs_total", "status", "timed_out", m.runTimedOut.Load())
	writeHistogram(output, "ailuo_run_duration_seconds", &m.runDuration)
	writeHistogram(output, "ailuo_model_first_token_duration_seconds", &m.firstToken)
	writeMetric(output, "ailuo_run_cancellations_total", m.cancellations.Load())
	writeMetric(output, "ailuo_run_retries_total", m.runRetries.Load())
	writeMetric(output, "ailuo_provider_retries_total", m.providerRetries.Load())
	writeMetric(output, "ailuo_model_input_tokens_total", m.modelInputTokens.Load())
	writeMetric(output, "ailuo_model_output_tokens_total", m.modelOutputToken.Load())
	writeMetric(output, "ailuo_model_cost_microusd_total", m.modelCost.Load())
	writeOutcomes(output, "ailuo_capability_calls_total", &m.capabilities)
	writeHistogram(output, "ailuo_capability_duration_seconds", &m.capability)
	writeOutcomes(output, "ailuo_tool_calls_total", &m.tools)
	writeHistogram(output, "ailuo_tool_duration_seconds", &m.tool)
	writeOutcomes(output, "ailuo_storage_operations_total", &m.storage)
	writeHistogram(output, "ailuo_storage_duration_seconds", &m.storageTime)
	writeOutcomes(output, "ailuo_runtime_loads_total", &m.runtimeLoads)
	writeHistogram(output, "ailuo_runtime_load_duration_seconds", &m.runtimeLoad)
	writeOutcomes(output, "ailuo_runtime_stops_total", &m.runtimeStops)
	writeHistogram(output, "ailuo_runtime_stop_duration_seconds", &m.runtimeStop)
	writeMetric(output, "ailuo_runtime_calls_active", m.runtimeCalls.Load())
	if _, err := writer.Write([]byte(output.String())); err != nil {
		Error(request.Context(), "写入指标响应失败", err)
	}
}

func writeOutcomes(writer *strings.Builder, name string, values *outcomeCounters) {
	writeLabeledMetric(writer, name, "result", "success", values.success.Load())
	writeLabeledMetric(writer, name, "result", "failure", values.failure.Load())
}

func writeHistogram(writer *strings.Builder, name string, histogram *durationHistogram) {
	for index, boundary := range durationBuckets {
		_, _ = fmt.Fprintf(writer, "%s_bucket{le=%q} %d\n", name, formatSeconds(boundary), histogram.buckets[index].Load())
	}
	_, _ = fmt.Fprintf(writer, "%s_bucket{le=\"+Inf\"} %d\n", name, histogram.count.Load())
	_, _ = fmt.Fprintf(writer, "%s_sum %s\n", name, formatSeconds(time.Duration(histogram.sumNano.Load())))
	_, _ = fmt.Fprintf(writer, "%s_count %d\n", name, histogram.count.Load())
}

func writeMetric(writer *strings.Builder, name string, value any) {
	_, _ = fmt.Fprintf(writer, "%s %d\n", name, value)
}

func writeLabeledMetric(writer *strings.Builder, name, label, value string, count uint64) {
	_, _ = fmt.Fprintf(writer, "%s{%s=%q} %d\n", name, label, value, count)
}

func formatSeconds(value time.Duration) string {
	return strconv.FormatFloat(value.Seconds(), 'f', -1, 64)
}
