//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/agenthost"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/subagent"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestGoPythonModelToolDatabaseLoop(t *testing.T) {
	var modelTurns atomic.Int32
	modelHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models/test-model" {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"id":"test-model","object":"model","created":1,"owned_by":"test"}`)
			return
		}
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		body, _ := io.ReadAll(request.Body)
		bodyText := string(body)
		writer.Header().Set("Content-Type", "text/event-stream")
		switch modelTurns.Add(1) {
		case 1:
			if !strings.Contains(bodyText, "cap_agent_run") || !strings.Contains(bodyText, "cap_campus_bus_routes_list") {
				t.Errorf("root model request missing projected tools: %s", body)
			}
			fmt.Fprint(writer, `data: {"id":"one","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"delegate-call","type":"function","function":{"name":"cap_agent_run","arguments":"{\"task\":\"查询校巴线路\",\"capability_ids\":[\"campus.bus.routes.list\"]}"}}]},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"one","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"one","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`+"\n\n")
		case 2:
			if strings.Contains(bodyText, "cap_agent_run") ||
				!strings.Contains(bodyText, "cap_campus_bus_routes_list") ||
				!strings.Contains(bodyText, "查询校巴线路") {
				t.Errorf("child model request has wrong projection or task: %s", body)
			}
			fmt.Fprint(writer, `data: {"id":"two","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"child-route-call","type":"function","function":{"name":"cap_campus_bus_routes_list","arguments":"{\"limit\":10}"}}]},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"two","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"two","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`+"\n\n")
		case 3:
			if strings.Contains(bodyText, "cap_agent_run") || !strings.Contains(bodyText, `"role":"tool"`) {
				t.Errorf("child follow-up request is invalid: %s", body)
			}
			fmt.Fprint(writer, `data: {"id":"three","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"子任务确认一条测试线路。"},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"three","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"three","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}}`+"\n\n")
		case 4:
			if !strings.Contains(bodyText, "cap_agent_run") ||
				!strings.Contains(bodyText, `"role":"tool"`) ||
				!strings.Contains(bodyText, "子任务确认一条测试线路") {
				t.Errorf("root follow-up request did not contain child result: %s", body)
			}
			fmt.Fprint(writer, `data: {"id":"two","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"当前有一条测试线路。"},"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"two","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"two","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}}`+"\n\n")
		default:
			t.Errorf("unexpected model request: %s", body)
			http.Error(writer, "unexpected model request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(writer, "data: [DONE]\n\n")
	})
	modelListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	modelServer := httptest.NewUnstartedServer(modelHandler)
	modelServer.Listener = modelListener
	modelServer.Start()
	defer modelServer.Close()

	address := freeAddress(t)
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, agenthost.DefaultPythonPath(root), "-m", "agent.runtime", "--listen", address)
	command.Dir = root
	command.Env = append(os.Environ(),
		"AILUO_MODEL_API_KEY=test-key",
		"AILUO_MODEL_BASE_URL="+modelServer.URL+"/v1",
	)
	logs := &strings.Builder{}
	command.Stdout = logs
	command.Stderr = logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = command.Wait()
	}()

	dialContext, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()
	connection, agentClient, err := agenthost.Dial(dialContext, address)
	if err != nil {
		t.Fatalf("dial agent: %v\n%s", err, logs.String())
	}
	defer connection.Close()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	busStore := memory.NewBusStore()
	busStore.ReplaceCatalog(campus.AppID, nil, []bus.Route{{ID: "r1", Name: "测试线路", Direction: "去程"}})
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	policy.Enable(campus.AppID, subagent.CapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.WithIdempotencyStore(store))
	registerHostedCampus(t, reg, busStore)
	orchestrator := kernelecho.NewOrchestrator(agentClient, reg, dispatcher, policy, store, kernelecho.Config{
		AppID: campus.AppID, Model: "test-model", SystemPrompt: "test", Timezone: "Asia/Shanghai",
		MaxSteps: 4, MaxToolCalls: 8, MaxInputTokens: 1000, MaxOutputTokens: 500,
		MaxTotalTokens: 1500, MaxOutputBytes: 4096, ProviderTimeout: 5 * time.Second,
	})
	if err := subagent.Register(reg, orchestrator); err != nil {
		t.Fatal(err)
	}
	handler := web.NewServer(ctx, orchestrator, store, health.Combined{store, agenthost.NewHealthChecker(agentClient, "test-model")}, reg, policy, campus.AppID, access.NewHub(campus.AppID, store, nil)).Handler()
	readinessRecorder := httptest.NewRecorder()
	handler.ServeHTTP(readinessRecorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readinessRecorder.Code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s logs=%s", readinessRecorder.Code, readinessRecorder.Body.String(), logs.String())
	}
	createRecorder := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewBufferString(`{"message":"有哪些校巴线路？"}`))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Idempotency-Key", "integration-create-echo")
	handler.ServeHTTP(createRecorder, createRequest)
	if createRecorder.Code != http.StatusAccepted {
		t.Fatalf("create echo status=%d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	traceID := createRecorder.Header().Get("X-Trace-ID")
	var accepted map[string]string
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	echoID := accepted["echo_id"]
	eventsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(eventsRecorder, httptest.NewRequest(http.MethodGet, accepted["events_url"], nil))
	if eventsRecorder.Code != http.StatusOK || !strings.Contains(eventsRecorder.Body.String(), "event: reply.final") {
		t.Fatalf("events status=%d body=%s logs=%s", eventsRecorder.Code, eventsRecorder.Body.String(), logs.String())
	}
	// 平台消息必须已持久化到会话台账（Web 匿名会话），验证"平台与 Agent 历史解耦"。
	messages, err := store.ListMessages(ctx, campus.AppID, "web-anonymous", session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "anonymous" || messages[0].Type != "text" {
		t.Fatalf("会话消息未持久化或形状错误: messages=%#v err=%v", messages, err)
	}
	record, events, err := store.GetEcho(ctx, campus.AppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.FinalMessage != "当前有一条测试线路。" || modelTurns.Load() != 4 {
		encoded, _ := json.Marshal(events)
		t.Fatalf("record=%#v model_turns=%d events=%s logs=%s", record, modelTurns.Load(), encoded, logs.String())
	}
	runs, err := store.ListRuns(ctx, campus.AppID, echoID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%#v err=%v logs=%s", runs, err, logs.String())
	}
	var rootRun, childRun kernelecho.RunRecord
	for _, run := range runs {
		if run.ParentRunID == "" {
			rootRun = run
		} else {
			childRun = run
		}
	}
	if rootRun.ID == "" ||
		childRun.ParentRunID != rootRun.ID ||
		childRun.OriginCallID != "delegate-call" ||
		childRun.ResultMessage != "子任务确认一条测试线路。" ||
		len(childRun.CapabilityScope) != 1 ||
		childRun.CapabilityScope[0] != campus.BusRouteListCapabilityID {
		t.Fatalf("root=%#v child=%#v logs=%s", rootRun, childRun, logs.String())
	}
	for _, run := range []kernelecho.RunRecord{rootRun, childRun} {
		if run.LastAgentSequence != 6 ||
			run.UsedInputTokens != 30 ||
			run.UsedOutputTokens != 5 ||
			run.UsedTotalTokens != 35 ||
			run.UsedProviderRetries != 0 {
			t.Fatalf("run usage/state=%#v logs=%s", run, logs.String())
		}
	}
	audits, err := store.ListCapabilityCalls(ctx, campus.AppID, echoID)
	if err != nil || len(audits) != 2 {
		t.Fatalf("audits=%#v err=%v logs=%s", audits, err, logs.String())
	}
	for _, audit := range audits {
		if audit.CapabilityID == subagent.CapabilityID &&
			(bytes.Contains(audit.Payload, []byte("查询校巴线路")) ||
				!bytes.Contains(audit.Payload, []byte(`"task":"[已脱敏]"`))) {
			t.Fatalf("Subagent task leaked into audit: %s", audit.Payload)
		}
	}
	if traceID == "" || !strings.Contains(logs.String(), `trace_id="`+traceID+`"`) || !strings.Contains(logs.String(), "parent_span_id=") {
		t.Fatalf("Go→Python trace context missing: trace_id=%q logs=%s", traceID, logs.String())
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

// registerHostedCampus 以 hosted 包形态装配校园服务：内置 wasm 工件经进程内沙箱执行，
// 权威存储经宿主函数投影。端到端测试通过真实 hosted 链路验证 Go → Python Agent → Capability。
func registerHostedCampus(t *testing.T, target *registry.Registry, store bus.Store) {
	t.Helper()
	host, err := loader.NewWasmHost(loader.WasmHostConfig{
		ReadArtifact:  campus.ReadArtifact,
		HostFunctions: campus.HostedFunctions(store),
	})
	if err != nil {
		t.Fatalf("NewWasmHost: %v", err)
	}
	manager, err := loader.New(map[string]loader.Host{loader.ModeHosted: host})
	if err != nil {
		t.Fatalf("loader.New: %v", err)
	}
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(shutdownContext); err != nil {
			t.Errorf("loader shutdown: %v", err)
		}
	})
	record := loader.InstalledRecord{
		Runtime:      campus.Manifest(),
		Tools:        campus.ToolSpecs(),
		Service:      campus.ServiceSpec(),
		Capabilities: campus.CapabilitySpecs(),
	}
	if err := loader.RegisterInstalled(manager, target, []loader.InstalledRecord{record}); err != nil {
		t.Fatalf("RegisterInstalled: %v", err)
	}
	// 预热 pin 的内置包：编译在测试装配时完成，避免占用 Run 的 deadline。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := manager.Warmup(ctx, []string{campus.ServiceID}, 1); err != nil {
		t.Fatalf("warm campus hosted package: %v", err)
	}
}
