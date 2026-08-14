package web

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

var updateGolden = flag.Bool("update", false, "更新 golden 文件")

// TestSSEEnvelopeGolden 固定 SSE 线上信封格式（id/event/data 定式与 JSON 载荷），
// 该格式是版本化对外契约，任何改动都在 golden 回归中显式暴露。
func TestSSEEnvelopeGolden(t *testing.T) {
	event := kernelecho.Event{
		AppID:     "campus-services",
		EchoID:    "echo-1",
		RunID:     "run-1",
		Sequence:  1,
		Type:      "reply.delta",
		Payload:   json.RawMessage(`{"text":"正在查询"}`),
		CreatedAt: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}
	var buffer bytes.Buffer
	if err := writeSSE(&buffer, event); err != nil {
		t.Fatal(err)
	}
	got := buffer.Bytes()
	goldenPath := filepath.Join("testdata", "sse.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("SSE 信封格式变更：\ngot:\n%s\nwant:\n%s", got, want)
	}
}
