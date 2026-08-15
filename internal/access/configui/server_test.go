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

func TestConfigAPIStoresSecretsWithoutReturningThem(t *testing.T) {
	manager, err := controlconfig.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(manager)
	if err != nil {
		t.Fatal(err)
	}
	payload := controlconfig.SaveInput{
		Model: "test-model", ModelAPIKey: "never-return-this", ModelRequestTimeoutSeconds: 30,
		ModelReadinessTimeoutSeconds: 3, ModelMaxRetries: 2, ModelRetryBaseSeconds: 0.25,
		ModelRetryMaxSeconds: 2, ModelRequestsPerMinute: 60, ModelMaxConcurrency: 4,
		QQEnabled: true, QQWSURL: "ws://127.0.0.1:3001", QQWSToken: "qq-never-return-this",
		QQBotID: "2647414417", QQAllowedGroupIDs: []string{"123456"},
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
	if strings.Contains(recorder.Body.String(), "never-return-this") {
		t.Fatal("secret leaked in configuration response")
	}
	if !strings.Contains(recorder.Body.String(), `"model_api_key_configured":true`) || !strings.Contains(recorder.Body.String(), `"qq_ws_token_configured":true`) {
		t.Fatalf("body=%s", recorder.Body.String())
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
