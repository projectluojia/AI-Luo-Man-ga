package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq"
	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
)

type qqAdminTestManager struct{ value qqsettings.Settings }

func (m *qqAdminTestManager) Snapshot(context.Context) (qqsettings.Settings, qq.RuntimeStatus, error) {
	return m.value, qq.RuntimeStatus{Running: m.value.Enabled}, nil
}

func (m *qqAdminTestManager) Update(_ context.Context, generation uint64, value qqsettings.Settings) (qqsettings.Settings, qq.RuntimeStatus, error) {
	if generation != m.value.Generation {
		return qqsettings.Settings{}, qq.RuntimeStatus{}, qqsettings.ErrConflict
	}
	value.Generation = generation + 1
	m.value = value
	return value, qq.RuntimeStatus{Running: value.Enabled}, nil
}

func TestQQAdminOnlyAcceptsLoopbackAndStrictUpdates(t *testing.T) {
	manager := &qqAdminTestManager{value: qqsettings.Settings{AppID: "campus-services", Generation: 1}}
	server := &Server{appID: "campus-services", qqAccessAdmin: manager}

	remoteRequest := httptest.NewRequest(http.MethodGet, "http://localhost/api/v1/admin/qq-access", nil)
	remoteRequest.RemoteAddr = "192.0.2.1:1234"
	remoteResponse := httptest.NewRecorder()
	server.getQQAccess(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusForbidden {
		t.Fatalf("remote status=%d", remoteResponse.Code)
	}

	body := `{"generation":1,"enabled":true,"ws_url":"ws://127.0.0.1:3001","bot_qq_id":"2647414417","allowed_group_ids":["12345"],"allowed_private_user_ids":["67890"]}`
	request := httptest.NewRequest(http.MethodPut, "http://localhost/api/v1/admin/qq-access", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Host = "localhost"
	request.Header.Set("Origin", "http://localhost")
	response := httptest.NewRecorder()
	server.updateQQAccess(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Settings qqsettings.Settings `json:"settings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload.Settings.Generation != 2 || !payload.Settings.Enabled {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}

	unknownRequest := httptest.NewRequest(http.MethodPut, "http://localhost/api/v1/admin/qq-access", strings.NewReader(`{"generation":2,"unknown":true}`))
	unknownRequest.RemoteAddr = "127.0.0.1:1234"
	unknownRequest.Host = "localhost"
	unknownResponse := httptest.NewRecorder()
	server.updateQQAccess(unknownResponse, unknownRequest)
	if unknownResponse.Code != http.StatusBadRequest {
		t.Fatalf("unknown status=%d", unknownResponse.Code)
	}
}
