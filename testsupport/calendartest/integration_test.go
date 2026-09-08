//go:build integration

// 学业校历 hosted 包端到端：真实 ailuo.toml 清单 + 真实 guest 源码现场编译 +
// packstore 权威快照窗口查询，全部经真实 Dispatcher。
package calendartest_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
)

// seedCalendar 播种校历系统快照：一个开学事件。
func seedCalendar(t *testing.T, store packstore.Store) {
	t.Helper()
	now := time.Now().UTC()
	packageID := hostedtest.ManifestOf(t, "calendar").ID
	scope := packstore.Scope{AppID: packageID, PackageID: packageID, Namespace: hostedtest.Packages["calendar"].StorageNamespace}
	if err := store.ReplaceSnapshot(context.Background(), scope, hostedtest.AuthoritativeMeta(now), map[string][]packstore.Document{
		"events": {hostedtest.MustDoc(t, "e1", map[string]any{"id": "e1", "title": "开学", "type": "term", "start_at": "2026-09-02T00:00:00Z", "end_at": "2026-09-03T00:00:00Z", "source_revision": "rev-1"})},
	}); err != nil {
		t.Fatal(err)
	}
}

// newCalendarDispatcher 装配校历包完整治理链路 Dispatcher。
func newCalendarDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	seedCalendar(t, store)
	hostedtest.RegisterHosted(t, reg, store, "calendar")
	return hostedtest.NewDispatcher(t, reg, hostedtest.ManifestOf(t, "calendar").ID, []string{"calendar.events.list"})
}

func invoke(t *testing.T, d *runtime.Dispatcher, appID, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	return hostedtest.Invoke(t, d, appID, capabilityID, payload, idempotencyKey, confirmationID)
}

// TestHostedCalendarEventsList 经真实 wasm guest 按窗口查询校历事件。
func TestHostedCalendarEventsList(t *testing.T) {
	d := newCalendarDispatcher(t)
	ok, result, errText := invoke(t, d, hostedtest.ManifestOf(t, "calendar").ID, "calendar.events.list",
		`{"from":"2026-09-01T00:00:00Z","to":"2026-09-30T00:00:00Z"}`, "", "")
	if !ok {
		t.Fatalf("events.list failed: %s", errText)
	}
	var decoded struct {
		Events []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"events"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatalf("events.list result %s: %v", result, err)
	}
	if len(decoded.Events) != 1 || decoded.Events[0].Title != "开学" {
		t.Fatalf("events = %#v", decoded.Events)
	}
}
