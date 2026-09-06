package configui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	controlconfig "github.com/projectluojia/AI-Luo-Man-ga/internal/controlplane/config"
)

func TestConfigAPIStoresQQSecretWithoutReturningIt(t *testing.T) {
	manager, err := controlconfig.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(manager)
	if err != nil {
		t.Fatal(err)
	}
	defaults := controlconfig.DefaultSettings()
	payload := controlconfig.SaveInput{
		AppID: "test-app", ExecutorID: "executor.test", ExecutorConfig: json.RawMessage(`{"strategy":"test"}`), ExecutorTimeoutSeconds: 30,
		QQEnabled: true, QQWSURL: "ws://127.0.0.1:3001", QQWSToken: "qq-never-return-this",
		QQBotID: "2647414417", QQAllowedGroupIDs: []string{"123456"},
		QQQuickReplies: []controlconfig.QQQuickReply{{Trigger: "ping", Reply: "pong"}},
		QQPokeReplies:  []string{"在呢"},
		Execution:      defaults.Execution, Orchestration: defaults.Orchestration,
		ContextAssembly: defaults.ContextAssembly, Scheduler: defaults.Scheduler,
		QQConnection: defaults.QQConnection, RuntimeProcess: defaults.RuntimeProcess,
		Governance: defaults.Governance,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:9178/api/v1/config", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:43000"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "never-return-this") || strings.Contains(recorder.Body.String(), `"executor_config"`) {
		t.Fatal("protected configuration leaked in response")
	}
	if !strings.Contains(recorder.Body.String(), `"qq_ws_token_configured":true`) {
		t.Fatalf("body=%s", recorder.Body.String())
	}
	var snapshot controlconfig.PublicSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Settings.QQQuickReplies) != 1 || snapshot.Settings.QQQuickReplies[0].Reply != "pong" ||
		len(snapshot.Settings.QQPokeReplies) != 1 || snapshot.Settings.QQPokeReplies[0] != "在呢" ||
		snapshot.Settings.AppID != "test-app" {
		t.Fatalf("snapshot=%+v", snapshot.Settings)
	}
	resolved, ok := manager.CurrentResolved()
	if !ok || string(resolved.Settings.ExecutorConfig) != `{"strategy":"test"}` {
		t.Fatalf("stored executor config=%q", resolved.Settings.ExecutorConfig)
	}
	getRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9178/api/v1/config", nil)
	getRequest.RemoteAddr = "127.0.0.1:43000"
	getRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(getRecorder, getRequest)
	if getRecorder.Code != http.StatusOK || !strings.Contains(getRecorder.Body.String(), `"qq_poke_replies":["在呢"]`) || strings.Contains(getRecorder.Body.String(), `"executor_config"`) {
		t.Fatalf("status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	payload.Revision = snapshot.Settings.Revision
	payload.ExecutorConfig = nil
	payload.QQWSToken = ""
	body, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:9178/api/v1/config", bytes.NewReader(body))
	secondRequest.RemoteAddr = "127.0.0.1:43000"
	secondRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(secondRecorder, secondRequest)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second save status=%d body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}
	resolved, ok = manager.CurrentResolved()
	if !ok || string(resolved.Settings.ExecutorConfig) != `{"strategy":"test"}` {
		t.Fatalf("executor config was overwritten: %q", resolved.Settings.ExecutorConfig)
	}
}

func TestConfigAPIRejectsRemoteAndCrossOriginRequests(t *testing.T) {
	manager, err := controlconfig.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(manager)
	if err != nil {
		t.Fatal(err)
	}
	remoteRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9178/api/v1/config", nil)
	remoteRequest.RemoteAddr = "192.0.2.4:43000"
	remoteRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(remoteRecorder, remoteRequest)
	if remoteRecorder.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d", remoteRecorder.Code)
	}
	crossOriginRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9178/api/v1/config", nil)
	crossOriginRequest.RemoteAddr = "127.0.0.1:43000"
	crossOriginRequest.Header.Set("Origin", "http://attacker.example")
	crossOriginRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(crossOriginRecorder, crossOriginRequest)
	if crossOriginRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d", crossOriginRecorder.Code)
	}
}

func TestConfigUIIsEmbedded(t *testing.T) {
	manager, err := controlconfig.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(manager)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9178/", nil)
	request.RemoteAddr = "127.0.0.1:43000"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "AI珞 V3") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestConfigServerRejectsNonLoopbackListenAddress(t *testing.T) {
	manager, err := controlconfig.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Run(t.Context(), "0.0.0.0:9178"); err == nil {
		t.Fatal("non-loopback configuration listener was accepted")
	}
}
