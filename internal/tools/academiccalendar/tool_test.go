package academiccalendar

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
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
	r := QueryRequest{From: time.Now(), To: time.Now()}
	if !errors.Is(r.NormalizeAndValidate(), ErrQueryRequired) {
		t.Fatal("expected invalid range")
	}
}
