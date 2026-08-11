package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func FuzzValidateNewTask(f *testing.F) {
	for _, seed := range [][5]string{
		{"app-a", "task-1", "bus.catalog.sync", `{"source":"demo"}`, "op-1"},
		{"", "", "", "", ""},
		{"App-A", "task", "type", "{}", "k"},
		{"a", "b", "c", `not json`, strings.Repeat("x", 200)},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4])
	}
	f.Fuzz(func(t *testing.T, appID, taskID, taskType, params, idempotencyKey string) {
		created := time.Unix(1, 0).UTC()
		value := Task{
			AppID:          appID,
			TaskID:         taskID,
			Type:           taskType,
			Status:         StatusQueued,
			Attempt:        1,
			MaxAttempts:    3,
			Deadline:       created.Add(time.Hour),
			AvailableAt:    created,
			IdempotencyKey: idempotencyKey,
			Params:         json.RawMessage(params),
			CreatedAt:      created,
			UpdatedAt:      created,
		}
		// 任意输入不得 panic，错误必须是稳定的 ErrInvalidTask。
		err := ValidateNewTask(value)
		if err != nil && !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("ValidateNewTask(%#v) 返回未知错误类别: %v", value, err)
		}
	})
}
