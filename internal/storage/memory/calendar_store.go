package memory

import (
	"context"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	cal "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"
	"sort"
	"sync"
)

type CalendarStore struct {
	mu        sync.RWMutex
	snapshots map[string]cal.Snapshot
}

func NewCalendarStore() *CalendarStore {
	return &CalendarStore{snapshots: make(map[string]cal.Snapshot)}
}
func (s *CalendarStore) ReplaceSnapshot(ctx context.Context, in cal.SnapshotInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cal.ValidateSnapshotInput(in); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	ev := append([]cal.Event(nil), in.Events...)
	sort.Slice(ev, func(i, j int) bool {
		if ev[i].StartAt.Equal(ev[j].StartAt) {
			return ev[i].ID < ev[j].ID
		}
		return ev[i].StartAt.Before(ev[j].StartAt)
	})
	seen := make(map[string]struct{}, len(ev))
	for i := range ev {
		if ev[i].ID == "" || ev[i].Title == "" || ev[i].StartAt.IsZero() || !ev[i].EndAt.After(ev[i].StartAt) || !ev[i].StartAt.Before(in.ValidUntil) || ev[i].EndAt.After(in.ValidUntil) {
			return cal.ErrInvalid
		}
		if _, ok := seen[ev[i].ID]; ok {
			return cal.ErrInvalid
		}
		seen[ev[i].ID] = struct{}{}
		ev[i].SourceRevision = in.Revision
	}
	s.snapshots[in.AppID] = cal.Snapshot{Metadata: cal.SnapshotMetadata{Revision: in.Revision, Source: in.Source, Authoritative: in.Authoritative, Complete: in.Complete, ImportedAt: in.ImportedAt, ValidUntil: in.ValidUntil}, Events: ev}
	return nil
}
func (s *CalendarStore) Search(ctx context.Context, appID string, req cal.QueryRequest) (cal.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return cal.Snapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[appID]
	if !ok {
		return cal.Snapshot{}, contracts.ErrDataUnavailable
	}
	out := make([]cal.Event, 0, req.Limit)
	for _, e := range snap.Events {
		if err := ctx.Err(); err != nil {
			return cal.Snapshot{}, err
		}
		if e.EndAt.After(req.From) && e.StartAt.Before(req.To) {
			out = append(out, e)
			if len(out) == req.Limit {
				break
			}
		}
	}
	snap.Events = out
	return snap, nil
}
