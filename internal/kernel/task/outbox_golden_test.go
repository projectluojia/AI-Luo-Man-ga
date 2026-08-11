package task

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "更新 golden 文件")

// TestEventGoldenSerialization 固定 Outbox 事件 JSON 形状：该形状是版本化契约，
// 任何字段名、顺序或序列化行为的变更都会在 golden 回归中显式暴露。
func TestEventGoldenSerialization(t *testing.T) {
	event := Event{
		AppID:          "campus-services",
		TaskID:         "task-1",
		Type:           EventSucceeded,
		Status:         StatusSucceeded,
		Attempt:        2,
		TaskType:       "bus.catalog.sync",
		ErrorClass:     ErrorClassNonRetryable,
		IdempotencyKey: "secret-idempotency-key", // json:"-"，绝不进入输出
		CreatedAt:      time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
	}
	got, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("testdata", "event.golden")
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
	if !bytes.Equal(append(got, '\n'), want) {
		t.Fatalf("Outbox 事件 JSON 形状变更：\ngot  %s\nwant %s", got, want)
	}
	if bytes.Contains(got, []byte("secret-idempotency-key")) {
		t.Fatal("幂等键泄露进事件 JSON")
	}
}
