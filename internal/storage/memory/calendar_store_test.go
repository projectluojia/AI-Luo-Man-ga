package memory

import (
	"context"
	"errors"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	cal "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"
	"testing"
	"time"
)

func TestCalendarStoreRoundTripIsolationAndCancellation(t *testing.T) {
	s := NewCalendarStore()
	now := time.Now().UTC().Truncate(time.Second)
	in := cal.SnapshotInput{AppID: "app-a", Revision: "r1", Source: cal.SourceDemo, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour), Events: []cal.Event{{ID: "b", Title: "B", StartAt: now.Add(10 * time.Minute), EndAt: now.Add(20 * time.Minute)}, {ID: "a", Title: "A", StartAt: now.Add(10 * time.Minute), EndAt: now.Add(15 * time.Minute)}}}
	if err := s.ReplaceSnapshot(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Search(context.Background(), "app-a", cal.QueryRequest{From: now, To: now.Add(time.Hour), Limit: 10})
	if err != nil || len(out.Events) != 2 || out.Events[0].ID != "a" {
		t.Fatalf("snapshot=%#v err=%v", out, err)
	}
	if _, err := s.Search(context.Background(), "app-b", cal.QueryRequest{From: now, To: now.Add(time.Hour), Limit: 10}); !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatal("missing app must not return data")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ReplaceSnapshot(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestCalendarStoreRejectsUnmarkedSourceAndInvalidEvent(t *testing.T) {
	s := NewCalendarStore()
	now := time.Now().UTC()
	base := cal.SnapshotInput{AppID: "app", Revision: "r", Source: "untrusted", Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}
	if err := s.ReplaceSnapshot(context.Background(), base); !errors.Is(err, cal.ErrInvalid) {
		t.Fatalf("source error=%v", err)
	}
	base.Source = cal.SourceDemo
	base.Events = []cal.Event{{ID: "e", Title: "", StartAt: now, EndAt: now.Add(time.Hour)}}
	if err := s.ReplaceSnapshot(context.Background(), base); !errors.Is(err, cal.ErrInvalid) {
		t.Fatalf("event error=%v", err)
	}
}
