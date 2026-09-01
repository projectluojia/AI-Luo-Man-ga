package web_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatStreamTranslatesEchoEvents 验证前端流式契约：标准链路创建 Echo 后，
// 内核 reply.delta/reply.final 被翻译为 text_delta/final 并最终发送 done。
func TestChatStreamTranslatesEchoEvents(t *testing.T) {
	handler, _ := newTestServer(t, false)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader([]byte(
		`{"text":"有哪些线路"}`,
	)))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{
		"event: text_delta", `"text":"你好"`,
		"event: final", `"reply":"你好"`,
		"event: done",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("stream missing %q: %s", expected, body)
		}
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("stream unexpectedly failed: %s", body)
	}
}

// TestChatStreamRejectsInvalidRequests 验证前端聊天契约的严格校验：未知字段、
// 空消息、附件与畸形 JSON 一律拒绝，不进入标准链路。
func TestChatStreamRejectsInvalidRequests(t *testing.T) {
	handler, _ := newTestServer(t, false)
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty text", body: `{"text":"  "}`, code: "invalid_message"},
		{name: "client identity field", body: `{"text":"hi","user_id":"spoofed"}`, code: "invalid_request"},
		{name: "malformed json", body: `{"text":`, code: "invalid_request"},
		{name: "trailing json", body: `{"text":"hi"}{}`, code: "invalid_request"},
		{name: "attachments unsupported", body: `{"text":"hi","image_ids":["upload/a.png"]}`, code: "attachments_unsupported"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/chat/stream", bytes.NewReader([]byte(test.body)))
			request.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var result map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result["code"] != test.code {
				t.Fatalf("error=%#v err=%v want code %s", result, err, test.code)
			}
		})
	}
}
