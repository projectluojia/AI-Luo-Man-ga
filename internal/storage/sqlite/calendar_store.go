package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	cal "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/academiccalendar"
	"sort"
	"time"
)

func init() {
	registerMigration(28, `
CREATE TABLE IF NOT EXISTS calendar_source_revisions (
  app_id TEXT NOT NULL,
  revision TEXT NOT NULL,
  source TEXT NOT NULL,
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0, 1)),
  complete INTEGER NOT NULL CHECK(complete IN (0, 1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  PRIMARY KEY(app_id, revision)
);
CREATE TABLE IF NOT EXISTS calendar_events (
  app_id TEXT NOT NULL,
  revision TEXT NOT NULL,
  event_id TEXT NOT NULL,
  title TEXT NOT NULL,
  event_type TEXT NOT NULL,
  start_at TEXT NOT NULL,
  end_at TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(app_id, revision, event_id),
  FOREIGN KEY(app_id, revision) REFERENCES calendar_source_revisions(app_id, revision) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS calendar_current_snapshots (
  app_id TEXT PRIMARY KEY,
  revision TEXT NOT NULL,
  activated_at TEXT NOT NULL,
  FOREIGN KEY(app_id, revision) REFERENCES calendar_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS calendar_events_range_idx ON calendar_events(app_id, revision, start_at, end_at);
`)
}

func (s *Store) ReplaceCalendarSnapshot(ctx context.Context, in cal.SnapshotInput) error {
	if in.AppID == "" || in.Revision == "" || in.Source == "" || !in.Complete || in.ImportedAt.IsZero() || in.ValidUntil.IsZero() || !in.ValidUntil.After(in.ImportedAt) || len(in.Events) > cal.MaxEvents {
		return cal.ErrInvalid
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	var resultErr error
	defer s.finishTx(tx, &resultErr, "replace calendar snapshot")
	if _, err = tx.ExecContext(ctx, `INSERT INTO calendar_source_revisions(app_id,revision,source,authoritative,complete,imported_at,valid_until) VALUES(?,?,?,?,?,?,?) ON CONFLICT(app_id,revision) DO UPDATE SET source=excluded.source,authoritative=excluded.authoritative,complete=excluded.complete,imported_at=excluded.imported_at,valid_until=excluded.valid_until`, in.AppID, in.Revision, in.Source, boolInt(in.Authoritative), boolInt(in.Complete), in.ImportedAt.UTC().Format(time.RFC3339Nano), in.ValidUntil.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM calendar_events WHERE app_id=? AND revision=?`, in.AppID, in.Revision); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range in.Events {
		if e.ID == "" || e.Title == "" || e.StartAt.IsZero() || !e.EndAt.After(e.StartAt) || !e.StartAt.Before(in.ValidUntil) || e.EndAt.After(in.ValidUntil) || seen[e.ID] {
			return cal.ErrInvalid
		}
		seen[e.ID] = true
		if _, err = tx.ExecContext(ctx, `INSERT INTO calendar_events(app_id,revision,event_id,title,event_type,start_at,end_at,description) VALUES(?,?,?,?,?,?,?,?)`, in.AppID, in.Revision, e.ID, e.Title, e.Type, e.StartAt.UTC().Format(time.RFC3339Nano), e.EndAt.UTC().Format(time.RFC3339Nano), e.Description); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO calendar_current_snapshots(app_id,revision,activated_at) VALUES(?,?,?) ON CONFLICT(app_id) DO UPDATE SET revision=excluded.revision,activated_at=excluded.activated_at`, in.AppID, in.Revision, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Search(ctx context.Context, appID string, req cal.QueryRequest) (cal.Snapshot, error) {
	tx, err := s.beginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return cal.Snapshot{}, err
	}
	var resultErr error
	defer s.finishTx(tx, &resultErr, "search calendar")
	var m cal.SnapshotMetadata
	var auth, complete int
	var imported, valid string
	err = tx.QueryRowContext(ctx, `SELECT r.revision,r.source,r.authoritative,r.complete,r.imported_at,r.valid_until FROM calendar_current_snapshots c JOIN calendar_source_revisions r ON r.app_id=c.app_id AND r.revision=c.revision WHERE c.app_id=?`, appID).Scan(&m.Revision, &m.Source, &auth, &complete, &imported, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return cal.Snapshot{}, contracts.ErrDataUnavailable
	}
	if err != nil {
		return cal.Snapshot{}, err
	}
	m.Authoritative = auth == 1
	m.Complete = complete == 1
	m.ImportedAt, err = time.Parse(time.RFC3339Nano, imported)
	if err != nil {
		return cal.Snapshot{}, err
	}
	m.ValidUntil, err = time.Parse(time.RFC3339Nano, valid)
	if err != nil {
		return cal.Snapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id,title,event_type,start_at,end_at,description FROM calendar_events WHERE app_id=? AND revision=? AND julianday(end_at)>julianday(?) AND julianday(start_at)<julianday(?) ORDER BY julianday(start_at),event_id LIMIT ?`, appID, m.Revision, req.From.UTC().Format(time.RFC3339Nano), req.To.UTC().Format(time.RFC3339Nano), req.Limit)
	if err != nil {
		return cal.Snapshot{}, err
	}
	events := []cal.Event{}
	for rows.Next() {
		var e cal.Event
		var st, en string
		if err := rows.Scan(&e.ID, &e.Title, &e.Type, &st, &en, &e.Description); err != nil {
			rows.Close()
			return cal.Snapshot{}, err
		}
		e.StartAt, err = time.Parse(time.RFC3339Nano, st)
		if err != nil {
			rows.Close()
			return cal.Snapshot{}, err
		}
		e.EndAt, err = time.Parse(time.RFC3339Nano, en)
		if err != nil {
			rows.Close()
			return cal.Snapshot{}, err
		}
		e.SourceRevision = m.Revision
		events = append(events, e)
	}
	rows.Close()
	sort.Slice(events, func(i, j int) bool { return events[i].StartAt.Before(events[j].StartAt) })
	if err := tx.Commit(); err != nil {
		return cal.Snapshot{}, fmt.Errorf("commit calendar read: %w", err)
	}
	return cal.Snapshot{Metadata: m, Events: events}, nil
}

var _ cal.Store = (*Store)(nil)
