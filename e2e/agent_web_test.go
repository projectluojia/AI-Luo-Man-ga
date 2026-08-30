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
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/campustest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packagecontract"
)

type integrationWebAuthenticator struct{}

type integrationPromptRenderer struct{}

func (integrationPromptRenderer) RenderSystemPrompt(_ context.Context, request kernelecho.PromptRenderRequest) (string, error) {
	return request.BaseSystemPrompt, nil
}

func (integrationWebAuthenticator) Authenticate(*http.Request) (web.AuthenticatedWebIdentity, error) {
	return web.AuthenticatedWebIdentity{
		PlatformSpaceID: "web", PlatformUserID: "integration-user", PlatformSessionID: "integration-session",
	}, nil
}

func TestGoPythonModelToolDatabaseLoop(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("Python Agent 的受限模型密钥文件校验仅支持 Unix")
	}
	var modelTurns atomic.Int32
	modelHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"data":[{"id":"test-model","object":"model","created":1,"owned_by":"test"}]}`)
			return
		}
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
		// writeTurn 输出一轮 chat.completion 的 delta/结束/用量三段 SSE。
		writeTurn := func(id, delta, finish, usage string) {
			fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":`+delta+`,"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":`+usage+`}`+"\n\n")
		}
		// 多子编排为异步：子 Run 排队后 root 立即继续自身轮次，子 Run 最终结果
		// 只落子 Run 记录（ResultMessage），不进入 root 的后续轮次。请求按内容
		// 路由（子 Run 系统提示、是否含 tool 结果），不依赖 root/子 Run 轮次的
		// 到达顺序；轮次计数仅用于末端断言。
		modelTurns.Add(1)
		isChild := strings.Contains(bodyText, "这是受治理的子 Run")
		hasToolResult := strings.Contains(bodyText, `"role":"tool"`)
		switch {
		case !isChild && !hasToolResult:
			if !strings.Contains(bodyText, "cap_run_create_child") || !strings.Contains(bodyText, "cap_campus_bus_routes_list") {
				t.Errorf("root model request missing projected tools: %s", body)
			}
			writeTurn("one", `{"role":"assistant","tool_calls":[{"index":0,"id":"delegate-call","type":"function","function":{"name":"cap_run_create_child","arguments":"{\"task\":\"查询校巴线路\",\"capability_ids\":[\"campus.bus.routes.list\"]}"}}]}`, "tool_calls", `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`)
		case !isChild && hasToolResult:
			if !strings.Contains(bodyText, "cap_run_create_child") || !strings.Contains(bodyText, `"role":"tool"`) {
				t.Errorf("root follow-up request is invalid: %s", body)
			}
			writeTurn("two", `{"role":"assistant","content":"当前有一条测试线路。"}`, "stop", `{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}`)
		case isChild && !hasToolResult:
			if strings.Contains(bodyText, "cap_run_create_child") ||
				!strings.Contains(bodyText, "cap_campus_bus_routes_list") ||
				!strings.Contains(bodyText, "查询校巴线路") {
				t.Errorf("child model request has wrong projection or task: %s", body)
			}
			writeTurn("three", `{"role":"assistant","tool_calls":[{"index":0,"id":"child-route-call","type":"function","function":{"name":"cap_campus_bus_routes_list","arguments":"{\"limit\":10}"}}]}`, "tool_calls", `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`)
		case isChild && hasToolResult:
			if strings.Contains(bodyText, "cap_run_create_child") || !strings.Contains(bodyText, `"role":"tool"`) {
				t.Errorf("child follow-up request is invalid: %s", body)
			}
			writeTurn("four", `{"role":"assistant","content":"子任务确认一条测试线路。"}`, "stop", `{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}`)
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

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 关闭顺序（defer 逆序）：取消调度 → 租约归还 → 关闭 agent → 取消上下文 → 关闭存储。
	// 调度必须先于关库，避免活动 Run 或轮询打到已关闭的存储。
	defer store.Close()
	defer cancel()
	secretPath := filepath.Join(t.TempDir(), "model-key")
	if err := os.WriteFile(secretPath, []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Python 执行者经通用 executor.v1 宿主纳管：进程启动、健康、停止由 Loader 负责。
	logs := &strings.Builder{}
	executorManifest := loader.Manifest{
		ID: "ailuo.agent.test", Version: "1.0.0", Mode: loader.ModeIsolated,
		Role:         loader.RoleExecutor,
		LockedDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	executorHost, err := loader.NewExecutorHost(loader.ExecutorHostConfig{
		Manifest: executorManifest,
		Resolve: func(context.Context) (packagecontract.ProcessSpec, error) {
			return packagecontract.ProcessSpec{
				Path: filepath.Join(root, "agent", ".venv", "bin", "python"),
				Args: []string{"-m", "agent.runtime", "--listen", address},
				Env: append(os.Environ(),
					"AILUO_MODEL_API_KEY_FILE="+secretPath,
					"AILUO_MODEL_BASE_URL="+modelServer.URL+"/v1",
				),
				WorkDir: root, Address: address,
			}, nil
		},
		Spawn: true, Model: "test-model", Stdout: logs, Stderr: logs,
		DialTimeout:    10 * time.Second,
		StopGrace:      3 * time.Second,
		TerminateGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewExecutorHost: %v", err)
	}
	executorManager, err := loader.New(executorHost)
	if err != nil {
		t.Fatalf("new executor manager: %v", err)
	}
	reg := registry.New()
	if err := executorManager.Register(ctx, executorManifest); err != nil {
		t.Fatalf("register executor: %v", err)
	}
	if err := executorManager.Warmup(ctx, executorManager.Pinned(), 1); err != nil {
		t.Fatalf("warm executor: %v\n%s", err, logs.String())
	}
	executorLease, err := executorManager.Executor(ctx)
	if err != nil {
		t.Fatalf("resolve executor: %v", err)
	}
	executorRuntime := executorLease.Runtime()
	clientProvider, ok := executorRuntime.(executor.ClientProvider)
	if !ok {
		t.Fatal("executor runtime does not expose an executor client")
	}
	executorClient := clientProvider.Client()
	// 关闭顺序（defer 逆序）：Shutdown 需等待租约排空，故租约归还最后注册、最先执行。
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := executorManager.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown executor manager: %v", err)
		}
	}()
	defer executorLease.Release()

	docs := memory.NewDocuments()
	scope := packstore.Scope{AppID: campus.AppID, Namespace: campus.StorageNamespace}
	routes := []packstore.Document{{ID: "r", Payload: []byte(`{"id":"r","name":"测试线路","direction":"去程","source_revision":"e2e-revision"}`)}}
	if err := docs.ReplaceSnapshot(context.Background(), scope, packstore.SnapshotMeta{
		Revision: "e2e-revision", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: time.Now().UTC().Add(-time.Hour), ValidUntil: time.Now().UTC().Add(time.Hour),
	}, map[string][]packstore.Document{"routes": routes}); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	policy.Enable(campus.AppID, kernelecho.CreateChildRunCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	campustest.RegisterHosted(t, reg, docs)
	// 上下文装配使用真实会话来源（SQLite 消息存储 + 安全 Blob 存储）。
	blobStore, err := blob.Open(filepath.Join(t.TempDir(), "blobs"), session.MaxMessageContentBytes)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	defer blobStore.Close()
	sessionService, err := session.NewService(store, blobStore)
	if err != nil {
		t.Fatalf("new session service: %v", err)
	}
	if _, _, err := store.Ensure(ctx, appconfig.Config{
		AppID: campus.AppID, Enabled: true, Model: "test-model", SystemPrompt: "test",
		Timezone: "Asia/Shanghai", MaxSteps: 4, MaxToolCalls: 8, MaxInputTokens: 1000,
		MaxOutputTokens: 500, MaxTotalTokens: 1500, MaxOutputBytes: 4096,
		ProviderTimeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("seed app config: %v", err)
	}
	orchestrator := kernelecho.NewOrchestrator(executorClient, reg, dispatcher, policy, kernelecho.StorePorts{
		Idempotency: store, Creation: store, Execution: store, Recovery: store,
		Cancellation: store, Events: store, Audit: store,
	}, kernelecho.Config{
		AppID:           campus.AppID,
		AppConfigSource: store,
		Context:         sessionService,
		Prompts:         integrationPromptRenderer{},
	})
	if err := kernelecho.RegisterChildCapabilities(reg, orchestrator); err != nil {
		t.Fatal(err)
	}
	echoEvents := access.NewEventHub()
	runScheduler := kernelecho.NewScheduler(ctx, orchestrator, store, echoEvents, campus.AppID)
	if _, err := runScheduler.Recover(ctx); err != nil {
		t.Fatalf("recover durable runs: %v", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := runScheduler.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown run scheduler: %v", err)
		}
	}()
	identities := identity.NewService(store)
	if _, err := identities.CreateUser(ctx, "integration-user"); err != nil {
		t.Fatal(err)
	}
	if err := identities.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: campus.AppID, Platform: "web", PlatformSpaceID: "web",
		PlatformUserID: "integration-user", UserID: "integration-user",
	}); err != nil {
		t.Fatal(err)
	}
	if err := identities.SetMembership(ctx, identity.AppMembership{AppID: campus.AppID, UserID: "integration-user"}); err != nil {
		t.Fatal(err)
	}
	platformHub, err := access.NewHub(campus.AppID, store, identities)
	if err != nil {
		t.Fatal(err)
	}
	echoAdmission := kernelecho.NewAdmission(orchestrator, runScheduler)
	handler := web.NewServer(
		echoAdmission, store,
		health.Combined{store, health.ExecutorChecker{Client: executorClient, Model: "test-model"}},
		reg, policy, campus.AppID, platformHub, runScheduler, echoEvents,
		web.WithWebAuthenticator(integrationWebAuthenticator{}),
	).Handler()
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
	messages, err := store.ListMessages(ctx, campus.AppID, rootRun.SessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "integration-user" || messages[0].Type != "text" {
		t.Fatalf("会话消息未持久化或形状错误: messages=%#v err=%v", messages, err)
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
		if audit.CapabilityID == kernelecho.CreateChildRunCapabilityID &&
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
