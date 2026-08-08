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

func TestLoggerProducesReadableChineseAndRedactsSecrets(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger, err := observe.New(observe.Config{
		Service: "test", Environment: "test", Format: "console", Writer: buffer, MaxValueLength: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := observe.With(context.Background(), slog.String("echo_id", "echo-1"))
	logger.Log(ctx, slog.LevelInfo, "校巴查询完成",
		slog.String("api_key", "should-not-appear"),
		slog.String("input_message", "private-message"),
		slog.Int64("input_tokens", 12),
		slog.String("output_tokens", "secret-token"),
		slog.String("detail", strings.Repeat("长", 20)),
	)
	output := buffer.String()
	if !strings.Contains(output, "level=信息") || !strings.Contains(output, "msg=校巴查询完成") || !strings.Contains(output, "echo_id=echo-1") {
		t.Fatalf("日志不可读：%s", output)
	}
	if strings.Contains(output, "should-not-appear") || strings.Contains(output, "private-message") || !strings.Contains(output, "[已脱敏]") || !strings.Contains(output, "[已截断]") {
		t.Fatalf("日志脱敏或截断失败：%s", output)
	}
	if !strings.Contains(output, "input_tokens=12") || strings.Contains(output, "secret-token") {
		t.Fatalf("Token 计数与凭据未被正确区分：%s", output)
	}
}

func TestLoggerRejectsSourcePathDisclosure(t *testing.T) {
	if _, err := observe.New(observe.Config{Service: "test", AddSource: true}); err == nil {
		t.Fatal("日志器接受了源码路径输出")
	}
}

func TestAuditJSONRedactsAndKeepsValidJSON(t *testing.T) {
	result := observe.SanitizeAuditJSON([]byte(`{"query":"文理学部","task":"仅供子 Run 的任务正文","result_message":"私有结果","token":"secret"}`), 1024)
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("审计结果不是合法 JSON：%s", result)
	}
	if decoded["token"] != "[已脱敏]" ||
		decoded["task"] != "[已脱敏]" ||
		decoded["result_message"] != "[已脱敏]" ||
		decoded["query"] != "文理学部" ||
		bytes.Contains(result, []byte("仅供子 Run 的任务正文")) ||
		bytes.Contains(result, []byte("私有结果")) {
		t.Fatalf("审计脱敏错误：%s", result)
	}
}

func TestOversizedAuditJSONBecomesHashSummary(t *testing.T) {
	result := observe.SanitizeAuditJSON([]byte(`{"value":"`+strings.Repeat("x", 100)+`"}`), 32)
	if !bytes.Contains(result, []byte("sha256")) || !json.Valid(result) {
		t.Fatalf("超长审计摘要错误：%s", result)
	}
}

func TestJSONLoggerProducesStableValidFields(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger, err := observe.New(observe.Config{
		Service: "test", Environment: "test", Format: "json", Writer: buffer,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Log(context.Background(), slog.LevelWarn, "校巴数据源暂不可用",
		slog.String("component", "bus_tool"),
		slog.String("request_payload", `{"query":"不能出现"}`),
	)
	var entry map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &entry); err != nil {
		t.Fatalf("JSON 日志无法解析：%v，内容=%s", err, buffer.String())
	}
	if entry["level"] != "WARN" || entry["msg"] != "校巴数据源暂不可用" || entry["component"] != "bus_tool" {
		t.Fatalf("JSON 日志稳定字段错误：%v", entry)
	}
	if entry["request_payload"] != "[已脱敏]" || strings.Contains(buffer.String(), "不能出现") {
		t.Fatalf("JSON 日志正文脱敏失败：%s", buffer.String())
	}
}

func TestLocalFieldsOverrideContextWithoutDuplicateKeys(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger, err := observe.New(observe.Config{Service: "test", Format: "json", Writer: buffer})
	if err != nil {
		t.Fatal(err)
	}
	ctx := observe.With(context.Background(), slog.String("request_id", "outer"))
	logger.Log(ctx, slog.LevelInfo, "测试字段覆盖", slog.String("request_id", "inner"))
	if strings.Count(buffer.String(), `"request_id"`) != 1 || !strings.Contains(buffer.String(), `"request_id":"inner"`) {
		t.Fatalf("日志字段覆盖错误：%s", buffer.String())
	}
}

func TestErrorDetailsNeverReachConsoleOrJSON(t *testing.T) {
	for _, format := range []string{"console", "json"} {
		t.Run(format, func(t *testing.T) {
			buffer := &bytes.Buffer{}
			logger, err := observe.New(observe.Config{Service: "test", Format: format, Writer: buffer})
			if err != nil {
				t.Fatal(err)
			}
			logger.Log(context.Background(), slog.LevelWarn, "依赖失败",
				slog.String("error", "provider-secret-body"),
				slog.String("reason", "/private/database/path"),
				slog.String("system_prompt", "private-system-prompt"),
				slog.String("result_message", "private-child-result"),
			)
			if strings.Contains(buffer.String(), "provider-secret-body") ||
				strings.Contains(buffer.String(), "/private/database/path") ||
				strings.Contains(buffer.String(), "private-system-prompt") ||
				strings.Contains(buffer.String(), "private-child-result") {
				t.Fatalf("错误正文泄露到 %s 日志：%s", format, buffer.String())
			}
			if strings.Count(buffer.String(), "[已脱敏]") != 4 {
				t.Fatalf("错误字段没有统一脱敏：%s", buffer.String())
			}
			logger.Error(context.Background(), "Provider 调用失败",
				errors.New("provider-secret-body /private/database/path"),
			)
			if strings.Contains(buffer.String(), "provider-secret-body") || strings.Contains(buffer.String(), "/private/database/path") {
				t.Fatalf("原始 error 正文泄露到 %s 日志：%s", format, buffer.String())
			}
			if !strings.Contains(buffer.String(), "error_class") || !strings.Contains(buffer.String(), "error_type") {
				t.Fatalf("错误稳定字段缺失：%s", buffer.String())
			}
		})
	}
}
