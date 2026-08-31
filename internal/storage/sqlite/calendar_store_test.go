package sqlite

import (
	"context"
	"errors"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	cal "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarSQLiteRoundTripIsolationAndAtomicValidation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "calendar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	in := cal.SnapshotInput{AppID: "app-a", Revision: "r1", Source: cal.SourceDemo, Complete: true, ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour), Events: []cal.Event{{ID: "e", Title: "开学", StartAt: now.Add(10 * time.Minute), EndAt: now.Add(20 * time.Minute)}}}
	if err := s.ReplaceSnapshot(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Search(context.Background(), "app-a", cal.QueryRequest{From: now, To: now.Add(time.Hour), Limit: 10})
	if err != nil || len(out.Events) != 1 || out.Events[0].SourceRevision != "r1" {
		t.Fatalf("snapshot=%#v err=%v", out, err)
	}
	if _, err := s.Search(context.Background(), "app-b", cal.QueryRequest{From: now, To: now.Add(time.Hour), Limit: 10}); !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("missing app error=%v", err)
	}
	in.Events[0].Title = ""
	if err := s.ReplaceSnapshot(context.Background(), in); !errors.Is(err, cal.ErrInvalid) {
		t.Fatalf("invalid replacement error=%v", err)
	}
	out, err = s.Search(context.Background(), "app-a", cal.QueryRequest{From: now, To: now.Add(time.Hour), Limit: 10})
	if err != nil || len(out.Events) != 1 || out.Events[0].Title != "开学" {
		t.Fatalf("failed replacement changed snapshot=%#v err=%v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, "app-a", cal.QueryRequest{From: now, To: now.Add(time.Hour), Limit: 10}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel search error=%v", err)
	}
}
