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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/confirmation"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/agent"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

// TestConfirmationRoundTripThroughPythonAgent 覆盖公共确认往返协议的端到端链路：
// 真实 Python Agent 调用需要确认的高风险 Capability → 内核自动创建持久确认并
// 返回 confirmation_required + 确认投影 → 模型向用户呈现等待状态 → 公共 HTTP
// 决策端点批准 → 同会话续跑时 Agent 携带 confirmation_id 重试成功。
func TestConfirmationRoundTripThroughPythonAgent(t *testing.T) {
	const capabilityName = "cap_e2e_confirm_send"
	var modelTurns atomic.Int32
	modelHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" || request.URL.Path == "/v1/models/test-model" {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"object":"list","data":[{"id":"test-model","object":"model","created":1,"owned_by":"test"}]}`)
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
			if !strings.Contains(bodyText, capabilityName) {
				t.Errorf("first model request missing confirmed capability: %s", body)
			}
			writeE2EToolCall(writer, "one", "confirm-call", capabilityName, `{"text":"你好"}`)
		case 2:
			if !strings.Contains(bodyText, "confirmation_required") || !strings.Contains(bodyText, "confirmation_id") {
				t.Errorf("model follow-up did not receive confirmation projection: %s", body)
			}
			writeE2EFinal(writer, "two", "该操作需要确认，请在界面上批准。")
		case 3:
			if !strings.Contains(bodyText, capabilityName) {
				t.Errorf("retry model request missing confirmed capability: %s", body)
			}
			writeE2EToolCall(writer, "three", "confirm-retry", capabilityName, `{"text":"你好"}`)
		case 4:
			if !strings.Contains(bodyText, `{\"sent\":true}`) {
				t.Errorf("retry follow-up did not receive capability result: %s", body)
			}
			writeE2EFinal(writer, "four", "已发送。")
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
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "confirmation-e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	defer cancel()
	logs := &strings.Builder{}
	agentHost, err := agent.NewHost(agent.Config{
		Resolve: func(context.Context) (agent.Spec, error) {
			return agent.Spec{
				PythonPath: agent.DefaultPythonPath(root),
				WorkDir:    root,
				Address:    address,
				Env: append(os.Environ(),
					"AILUO_MODEL_API_KEY=test-key",
					"AILUO_MODEL_BASE_URL="+modelServer.URL+"/v1",
				),
			}, nil
		},
		Spawn:          true,
		Model:          "test-model",
		Stdout:         logs,
		Stderr:         logs,
		DialTimeout:    10 * time.Second,
		StopGrace:      3 * time.Second,
		TerminateGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewAgentHost: %v", err)
	}
	agentManager, err := loader.New(agentHost)
	if err != nil {
		t.Fatalf("new agent manager: %v", err)
	}
	reg := registry.New()
	if err := loader.RegisterInstalled(ctx, agentManager, reg, []loader.InstalledRecord{agent.Record(agentHost)}); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	if err := agentManager.Warmup(ctx, agentManager.Pinned(), 1); err != nil {
		t.Fatalf("warm agent: %v\n%s", err, logs.String())
	}
	agentLease, err := agentManager.Executor(ctx)
	if err != nil {
		t.Fatalf("resolve executor: %v", err)
	}
	agentRuntime := agentLease.Runtime()
	clientProvider, ok := agentRuntime.(executor.ClientProvider)
	if !ok {
		t.Fatal("executor runtime does not expose an executor client")
	}
	agentClient := clientProvider.Client()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := agentManager.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown agent manager: %v", err)
		}
	}()
	defer agentLease.Release()

	const capabilityID = "e2e.confirm.send"
	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{ID: "e2e", Version: "1.0.0"},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			capabilityID: {
				Spec: registry.CapabilitySpec{
					ID: capabilityID, Version: "1.0.0", ServiceID: "e2e",
					Name: capabilityName, Description: "需要确认的测试外部调用",
					InputSchemaJSON:      `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`,
					SideEffect:           registry.SideEffectExternal,
					RequiresConfirmation: true,
				},
				Handler: func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
					return json.RawMessage(`{"sent":true}`), nil
				},
			},
		},
	}); err != nil {
		t.Fatalf("register capability: %v", err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(campus.AppID, capabilityID)
	confirmations := confirmation.NewService(store, confirmation.Config{})
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore:     store,
		ConfirmationVerifier: confirmations,
	})
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
	orchestrator := kernelecho.NewOrchestrator(agentClient, reg, dispatcher, policy, store, kernelecho.Config{
		AppID:           campus.AppID,
		AppConfigSource: store,
		Context:         sessionService,
		Confirmations:   confirmations,
	})
	if err := agent.Register(reg, orchestrator); err != nil {
		t.Fatal(err)
	}
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
	handler := web.NewServer(
		ctx, orchestrator, store,
		health.Combined{store, health.ExecutorChecker{Client: agentClient, Model: "test-model"}},
		reg, policy, campus.AppID, platformHub,
		web.WithWebAuthenticator(integrationWebAuthenticator{}),
		web.WithConfirmations(confirmations),
	).Handler()

	// 第一条消息：触发 confirmation_required，Run 正常结束并呈现等待状态。
	echoID := createE2EEcho(t, handler, "帮我发送消息", "e2e-confirm-1")
	waitForE2EEchoTerminal(t, handler, echoID, "该操作需要确认，请在界面上批准。")

	// 公共确认查询端点：返回 waiting 确认。
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/echoes/"+echoID+"/confirmations", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list confirmations status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed struct {
		Confirmations []struct {
			ConfirmationID string `json:"confirmation_id"`
			Status         string `json:"status"`
		} `json:"confirmations"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Confirmations) != 1 || listed.Confirmations[0].Status != confirmation.StatusWaiting {
		t.Fatalf("listed confirmations=%+v", listed.Confirmations)
	}
	confirmationID := listed.Confirmations[0].ConfirmationID

	// 公共决策端点：批准。
	decisionRecorder := httptest.NewRecorder()
	decisionRequest := httptest.NewRequest(http.MethodPost,
		"/api/v1/echoes/"+echoID+"/confirmations/"+confirmationID+"/decision",
		bytes.NewBufferString(`{"decision":"approved","confirmed_by":"integration-user"}`))
	decisionRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(decisionRecorder, decisionRequest)
	if decisionRecorder.Code != http.StatusOK || !strings.Contains(decisionRecorder.Body.String(), `"status":"approved"`) {
		t.Fatalf("decision status=%d body=%s", decisionRecorder.Code, decisionRecorder.Body.String())
	}

	// 同会话续跑：Agent 携带 confirmation_id 重试成功。
	continuationID := createE2EEcho(t, handler, "继续", "e2e-confirm-2")
	waitForE2EEchoTerminal(t, handler, continuationID, "已发送。")
}

// ---- e2e 辅助 ----

func createE2EEcho(t *testing.T, handler http.Handler, message, key string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewBufferString(`{"message":"`+message+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("create echo status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var accepted map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	return accepted["echo_id"]
}

func waitForE2EEchoTerminal(t *testing.T, handler http.Handler, echoID, finalMessage string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/echoes/"+echoID, nil))
		if recorder.Code == http.StatusOK {
			var status struct {
				Echo struct {
					Status       string `json:"status"`
					FinalMessage string `json:"final_message"`
				} `json:"echo"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &status); err == nil &&
				(status.Echo.Status == kernelecho.StatusSucceeded || status.Echo.Status == kernelecho.StatusFailed) {
				if status.Echo.Status != kernelecho.StatusSucceeded || status.Echo.FinalMessage != finalMessage {
					t.Fatalf("echo terminal=%s final=%q want succeeded/%q", status.Echo.Status, status.Echo.FinalMessage, finalMessage)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("echo %s did not reach terminal state in time; last body=%s", echoID, recorder.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writeE2EToolCall(writer http.ResponseWriter, id, callID, name, arguments string) {
	argumentsJSON, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"`+callID+`","type":"function","function":{"name":"`+name+`","arguments":`+string(argumentsJSON)+`}}]},"finish_reason":null}]}`+"\n\n")
	fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
	fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`+"\n\n")
}

func writeE2EFinal(writer http.ResponseWriter, id, text string) {
	fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"`+text+`"},"finish_reason":null}]}`+"\n\n")
	fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}}`+"\n\n")
}
