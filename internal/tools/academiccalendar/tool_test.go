package academiccalendar

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"testing"
	"time"
)

type testStore struct {
	snapshot Snapshot
	err      error
}

func (s testStore) Search(context.Context, string, QueryRequest) (Snapshot, error) {
	return s.snapshot, s.err
}

func TestEventsHandlerGovernanceAndFiltering(t *testing.T) {
	now := time.Now().UTC()
	st := testStore{snapshot: Snapshot{Metadata: SnapshotMetadata{Revision: "r1", Source: "zhihui-luojia", Authoritative: true, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, Events: []Event{{ID: "e1", Title: "开学", Type: "term", StartAt: now.Add(time.Minute), EndAt: now.Add(time.Hour), SourceRevision: "r1"}}}}
	p := QueryRequest{From: now, To: now.Add(2 * time.Hour)}
	b, _ := json.Marshal(p)
	out, err := eventsHandler(st)(context.Background(), contracts.RequestContext{AppID: "campus-services"}, b)
	if err != nil {
		t.Fatal(err)
	}
	var got QueryResult
	if json.Unmarshal(out, &got) != nil || len(got.Events) != 1 || got.DataStatus.State != DataStateAuthoritativeFresh {
		t.Fatalf("bad result %s", out)
	}
	st.snapshot.Metadata.Authoritative = false
	if _, err := eventsHandler(st)(context.Background(), contracts.RequestContext{}, b); !errors.Is(err, ErrDataUntrusted) {
		t.Fatalf("err=%v", err)
	}
}

func TestQueryRejectsInvalidRange(t *testing.T) {
	now := time.Now()
	r := QueryRequest{From: now, To: now}
	if !errors.Is(r.NormalizeAndValidate(), ErrQueryRequired) {
		t.Fatal("expected invalid range")
	}
}

func TestEventsHandlerRejectsStrictSchemaAndSnapshotStates(t *testing.T) {
	now := time.Now().UTC()
	base := Snapshot{Metadata: SnapshotMetadata{Revision: "r1", Source: SourceWUDA, Authoritative: true, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}}
	p := QueryRequest{From: now, To: now.Add(time.Hour)}
	b, _ := json.Marshal(p)
	for _, tc := range []struct {
		name string
		snap Snapshot
		err  error
	}{{"expired", Snapshot{Metadata: SnapshotMetadata{Revision: "r1", Source: SourceWUDA, Authoritative: true, Complete: true, ImportedAt: now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Minute)}}, ErrDataExpired}, {"incomplete", Snapshot{Metadata: SnapshotMetadata{Revision: "r1", Source: SourceWUDA, Authoritative: true, Complete: false, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}}, ErrDataIncomplete}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := eventsHandler(testStore{snapshot: tc.snap})(context.Background(), contracts.RequestContext{AppID: "app"}, b); !errors.Is(err, tc.err) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if _, err := eventsHandler(testStore{snapshot: base})(context.Background(), contracts.RequestContext{AppID: "app"}, []byte(`{"from":"bad","to":"bad","unknown":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("schema error=%v", err)
	}
	base.Events = []Event{{ID: "e", Title: "event", StartAt: now, EndAt: now.Add(time.Hour), SourceRevision: "other"}}
	if _, err := eventsHandler(testStore{snapshot: base})(context.Background(), contracts.RequestContext{AppID: "app"}, b); !errors.Is(err, ErrDataIncomplete) {
		t.Fatalf("revision error=%v", err)
	}
}

func TestQueryValidationAndSnapshotGovernanceBoundaries(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []QueryRequest{{From: now, To: now.Add(-time.Minute)}, {From: now, To: now.Add(367 * 24 * time.Hour)}, {From: now, To: now.Add(time.Hour), Limit: 0}, {From: now, To: now.Add(time.Hour), Limit: MaxEvents + 1}} {
		r := tc
		if err := r.NormalizeAndValidate(); err == nil && tc.Limit == MaxEvents+1 {
			t.Fatal("oversized limit accepted")
		}
	}
	base := SnapshotMetadata{Revision: "r", Source: SourceWUDA, Complete: true, Authoritative: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}
	if _, err := base.Govern(now.Add(-2 * time.Hour)); !errors.Is(err, ErrDataIncomplete) {
		t.Fatalf("future snapshot error=%v", err)
	}
	base.Authoritative = false
	if _, err := base.Govern(now); !errors.Is(err, ErrDataUntrusted) {
		t.Fatalf("untrusted error=%v", err)
	}
}
