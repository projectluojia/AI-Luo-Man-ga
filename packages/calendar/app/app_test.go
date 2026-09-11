package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/projectluojia/calendar/calendar"
	"github.com/projectluojia/guestkit"
)

// memStore 是窄 store 接口的内存实现：系统作用域以快照集合提供。
type memStore struct {
	meta      guestkit.SnapshotMeta
	metaFound bool
	system    map[string][]guestkit.Document
}

func newMemStore(meta guestkit.SnapshotMeta, metaFound bool) *memStore {
	return &memStore{meta: meta, metaFound: metaFound, system: map[string][]guestkit.Document{}}
}

func (m *memStore) Get(guestkit.Scope, string, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (m *memStore) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	var docs []guestkit.Document
	for _, doc := range m.system[collection] {
		if afterID == "" || doc.ID > afterID {
			docs = append(docs, doc)
		}
	}
	if len(docs) > limit {
		docs = docs[:limit]
	}
	if docs == nil {
		docs = []guestkit.Document{}
	}
	return guestkit.ListPage{Docs: docs, Meta: m.meta, MetaFound: m.metaFound}, nil
}

func (m *memStore) Put(string, string, json.RawMessage) error { return nil }
func (m *memStore) Delete(string, string) (bool, error)       { return false, nil }

func authoritativeMeta() guestkit.SnapshotMeta {
	return guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true,
		Complete: true, ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}
}

func eventStore(t *testing.T, meta guestkit.SnapshotMeta, metaFound bool, events ...calendar.Event) *memStore {
	t.Helper()
	store := newMemStore(meta, metaFound)
	for index, item := range events {
		payload, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		store.system[calendar.EventsCollection] = append(store.system[calendar.EventsCollection],
			guestkit.Document{ID: eventID(index), Payload: payload})
	}
	return store
}

func eventID(index int) string { return "doc-" + string(rune('a'+index)) }

func dispatch(t *testing.T, store guestkit.Store, payload any) guestkit.ResultEnvelope {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return guestkit.NewDispatcher(Handlers(store)).Dispatch(EventsListCapabilityID, body)
}

func decodeResult(t *testing.T, envelope guestkit.ResultEnvelope, target any) {
	t.Helper()
	if !envelope.OK {
		t.Fatalf("unexpected failure envelope: %s %s", envelope.Code, envelope.Message)
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		t.Fatal(err)
	}
}

func TestEventsListFiltersWindowAndSorts(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(30 * 24 * time.Hour)
	store := eventStore(t, authoritativeMeta(), true,
		calendar.Event{ID: "e2", Title: "期中", Type: "exam", StartAt: from.Add(15 * 24 * time.Hour), EndAt: from.Add(16 * 24 * time.Hour), SourceRevision: "rev-1"},
		calendar.Event{ID: "e1", Title: "开学", Type: "term", StartAt: from.Add(24 * time.Hour), EndAt: from.Add(25 * time.Hour), SourceRevision: "rev-1"},
		calendar.Event{ID: "e3", Title: "暑假", Type: "break", StartAt: to.Add(24 * time.Hour), EndAt: to.Add(48 * time.Hour), SourceRevision: "rev-1"},
	)
	var result eventsListResult
	decodeResult(t, dispatch(t, store, calendar.QueryRequest{From: from, To: to}), &result)
	if len(result.Events) != 2 || result.Events[0].ID != "e1" || result.Events[1].ID != "e2" {
		t.Fatalf("events=%+v", result.Events)
	}
	if result.DataStatus.State != guestkit.DataStateAuthoritativeFresh {
		t.Fatalf("data_status=%+v", result.DataStatus)
	}
}

func TestQueryRejectsInvalidWindows(t *testing.T) {
	now := time.Now()
	for name, request := range map[string]calendar.QueryRequest{
		"empty":      {},
		"reversed":   {From: now, To: now.Add(-time.Minute)},
		"zero width": {From: now, To: now},
		"over range": {From: now, To: now.Add(calendar.MaxRangeDays*24*time.Hour + time.Minute)},
		"limit over": {From: now, To: now.Add(24 * time.Hour), Limit: calendar.MaxEvents + 1},
	} {
		if envelope := dispatch(t, eventStore(t, authoritativeMeta(), true), request); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
			t.Fatalf("%s: envelope=%+v", name, envelope)
		}
	}
}

func TestEventsListRejectsUnknownFields(t *testing.T) {
	store := eventStore(t, authoritativeMeta(), true)
	envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch(EventsListCapabilityID, json.RawMessage(`{"from":"2026-09-01T00:00:00Z","to":"2026-09-02T00:00:00Z","extra":1}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestUnknownCapabilityIsInvalidArgument(t *testing.T) {
	store := eventStore(t, authoritativeMeta(), true)
	if envelope := guestkit.NewDispatcher(Handlers(store)).Dispatch("calendar.missing", nil); envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestGovernanceFailuresMapToStableCodes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		meta      guestkit.SnapshotMeta
		metaFound bool
		code      string
	}{{"untrusted", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "demo-fixture", Complete: true,
		ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}, true, guestkit.CodeDataUntrusted}, {"expired", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: time.Now().Add(-2 * time.Hour), ValidUntil: time.Now().Add(-time.Minute),
	}, true, guestkit.CodeDataExpired}, {"incomplete", guestkit.SnapshotMeta{
		SourceRevision: "rev-1", Source: "zhihui-luojia", ImportedAt: time.Now().Add(-time.Hour), ValidUntil: time.Now().Add(time.Hour),
	}, true, guestkit.CodeDataIncomplete}, {"missing", guestkit.SnapshotMeta{}, false, guestkit.CodeDataUnavailable}} {
		t.Run(tc.name, func(t *testing.T) {
			store := eventStore(t, tc.meta, tc.metaFound)
			if envelope := dispatch(t, store, calendar.QueryRequest{From: time.Now(), To: time.Now().Add(24 * time.Hour)}); envelope.OK || envelope.Code != tc.code {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
}

func TestRevisionMismatchFailsClosed(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := eventStore(t, authoritativeMeta(), true,
		calendar.Event{ID: "e1", Title: "开学", Type: "term", StartAt: from, EndAt: from.Add(time.Hour), SourceRevision: "other"},
	)
	if envelope := dispatch(t, store, calendar.QueryRequest{From: from, To: from.Add(24 * time.Hour)}); envelope.OK || envelope.Code != guestkit.CodeDataUnavailable {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestLimitTruncates(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := eventStore(t, authoritativeMeta(), true,
		calendar.Event{ID: "e1", Title: "a", StartAt: from, EndAt: from.Add(time.Hour), SourceRevision: "rev-1"},
		calendar.Event{ID: "e2", Title: "b", StartAt: from.Add(time.Hour), EndAt: from.Add(2 * time.Hour), SourceRevision: "rev-1"},
	)
	var result eventsListResult
	decodeResult(t, dispatch(t, store, calendar.QueryRequest{From: from, To: from.Add(24 * time.Hour), Limit: 1}), &result)
	if len(result.Events) != 1 || result.Events[0].ID != "e1" {
		t.Fatalf("events=%+v", result.Events)
	}
}
