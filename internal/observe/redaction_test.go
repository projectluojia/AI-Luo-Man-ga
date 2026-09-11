package observe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

func TestFreeTextCredentialsNeverReachLogsOrAudit(t *testing.T) {
	for _, value := range []string{
		"请求失败 password=synthetic-private-value",
		`响应 {"api_key": "synthetic-private-value"}`,
		"Authorization: Bearer synthetic-private-value",
		"Cookie: sid=synthetic-private-value",
		"https://user:synthetic-private-value@example.invalid/path",
		"-----BEGIN PRIVATE KEY-----\nsynthetic-private-value",
		"Bearer " + strings.Repeat("x", 100),
		"Basic " + strings.Repeat("x", 40),
		"ghp_" + strings.Repeat("x", 36),
		"github_pat_" + strings.Repeat("x", 32),
		"sk-proj-" + strings.Repeat("x", 32),
		"-----BEGIN RSA PRIVATE KEY-----\nsynthetic-private-value",
	} {
		for _, format := range []string{"console", "json"} {
			buffer := &bytes.Buffer{}
			logger, err := observe.New(observe.Config{Service: "test", Format: format, Writer: buffer, MaxValueLength: 32})
			if err != nil {
				t.Fatal(err)
			}
			logger.Log(context.Background(), slog.LevelInfo, value,
				slog.String("detail", value), slog.Group("nested", slog.String("note", value)),
				slog.String("request_id", "request-1"), slog.String("error_code", "unavailable"), slog.Int("input_tokens", 12))
			if strings.Contains(buffer.String(), "synthetic-private") || strings.Contains(buffer.String(), "xxxx") {
				t.Fatalf("%s 自由文本脱敏失败", format)
			}
			if !strings.Contains(buffer.String(), "request-1") || !strings.Contains(buffer.String(), "unavailable") {
				t.Fatal("稳定诊断字段丢失")
			}
		}
		payload, _ := json.Marshal(map[string]any{"detail": []any{value}})
		clean := observe.SanitizeAuditJSON(payload, 8192)
		if !json.Valid(clean) || bytes.Contains(clean, []byte("synthetic-private")) || bytes.Contains(clean, []byte("xxxx")) {
			t.Fatal("审计自由文本脱敏失败")
		}
	}
}

func TestBoundLogFieldsUseSameRedaction(t *testing.T) {
	for _, format := range []string{"console", "json"} {
		buffer := &bytes.Buffer{}
		_, err := observe.Configure(observe.Config{Service: "test", Format: format, Writer: buffer, MaxValueLength: 16})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = observe.Configure(observe.Config{Service: "test"}) })
		slog.Default().With("detail", "password=synthetic-private-value").WithGroup("group").Info("完成",
			slog.String("note", "Bearer synthetic-private-value"),
			slog.String("long_field_name_that_ends_with_secret", "synthetic-private-value"))
		if strings.Contains(buffer.String(), "synthetic") || strings.Count(buffer.String(), "[已脱敏]") != 3 {
			t.Fatal("绑定字段或长敏感字段名未脱敏")
		}
	}
}

func TestOpaqueLogValuesCannotSerializePrivateContent(t *testing.T) {
	for _, format := range []string{"console", "json"} {
		buffer := &bytes.Buffer{}
		logger, err := observe.New(observe.Config{Service: "test", Format: format, Writer: buffer})
		if err != nil {
			t.Fatal(err)
		}
		logger.Log(context.Background(), slog.LevelWarn, "调用失败",
			slog.Any("detail", errors.New("synthetic-private-content")),
			slog.Any("object", map[string]any{"note": "synthetic-private-content"}),
			slog.Any("bytes", []byte("synthetic-private-content")))
		if strings.Contains(buffer.String(), "synthetic-private-content") || strings.Count(buffer.String(), "[已脱敏]") != 3 {
			t.Fatal("不透明对象没有在序列化前统一脱敏")
		}
	}
}
