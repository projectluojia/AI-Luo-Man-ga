package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/sports"
)

var _ sports.Store = (*Store)(nil)

func init() {
	registerMigration(31, `
CREATE TABLE sports_source_revisions (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 256),
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0, 1)),
  complete INTEGER NOT NULL CHECK(complete IN (0, 1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  PRIMARY KEY (app_id, revision)
);
CREATE TABLE sports_current_snapshots (
  app_id TEXT PRIMARY KEY CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  activated_at TEXT NOT NULL,
  FOREIGN KEY (app_id, revision) REFERENCES sports_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE TABLE sports_venues (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  venue_id TEXT NOT NULL CHECK(length(venue_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  campus TEXT NOT NULL DEFAULT '' CHECK(length(campus) <= 256),
  address TEXT NOT NULL DEFAULT '' CHECK(length(address) <= 256),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, venue_id)
);
CREATE INDEX sports_venues_name_idx ON sports_venues(app_id, name, venue_id);
CREATE TABLE sports_projects (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  venue_id TEXT NOT NULL CHECK(length(venue_id) BETWEEN 1 AND 128),
  project_id TEXT NOT NULL CHECK(length(project_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, venue_id, project_id),
  FOREIGN KEY (app_id, venue_id) REFERENCES sports_venues(app_id, venue_id) ON DELETE CASCADE
);
CREATE INDEX sports_projects_venue_idx ON sports_projects(app_id, venue_id, name, project_id);
CREATE TABLE sports_slots (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  venue_id TEXT NOT NULL CHECK(length(venue_id) BETWEEN 1 AND 128),
  project_id TEXT NOT NULL CHECK(length(project_id) BETWEEN 1 AND 128),
  slot_id TEXT NOT NULL CHECK(length(slot_id) BETWEEN 1 AND 128),
  slot_date TEXT NOT NULL CHECK(length(slot_date)=10),
  start_at TEXT NOT NULL,
  end_at TEXT NOT NULL,
  capacity INTEGER NOT NULL CHECK(capacity >= 1 AND capacity <= 1024),
  remaining_quota INTEGER NOT NULL CHECK(remaining_quota >= 0 AND remaining_quota <= capacity),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, venue_id, project_id, slot_id),
  FOREIGN KEY (app_id, venue_id, project_id) REFERENCES sports_projects(app_id, venue_id, project_id) ON DELETE CASCADE
);
CREATE INDEX sports_slots_search_idx ON sports_slots(app_id, venue_id, project_id, slot_date, start_at, slot_id);
CREATE TABLE sports_webview_descriptors (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  entry_url TEXT NOT NULL CHECK(length(entry_url) BETWEEN 8 AND 512),
  required_user_agent TEXT NOT NULL CHECK(length(required_user_agent) BETWEEN 1 AND 256),
  required_headers TEXT NOT NULL CHECK(json_valid(required_headers) AND json_type(required_headers)='array'),
  requires_delegated_auth INTEGER NOT NULL CHECK(requires_delegated_auth IN (0, 1)),
  PRIMARY KEY (app_id, revision),
  FOREIGN KEY (app_id, revision) REFERENCES sports_source_revisions(app_id, revision) ON DELETE CASCADE
);
CREATE TABLE sports_reservations (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  reservation_id TEXT NOT NULL CHECK(length(reservation_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  venue_id TEXT NOT NULL CHECK(length(venue_id) BETWEEN 1 AND 128),
  project_id TEXT NOT NULL CHECK(length(project_id) BETWEEN 1 AND 128),
  slot_id TEXT NOT NULL CHECK(length(slot_id) BETWEEN 1 AND 128),
  count INTEGER NOT NULL CHECK(count >= 1 AND count <= 16),
  status TEXT NOT NULL CHECK(status IN ('confirmed','cancelled','expired','rejected_over_quota')),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancelled_at TEXT,
  PRIMARY KEY (app_id, reservation_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX sports_reservations_active_unique
  ON sports_reservations(app_id, venue_id, project_id, slot_id, user_id)
  WHERE status='confirmed';
CREATE INDEX sports_reservations_user_idx ON sports_reservations(app_id, user_id, updated_at, reservation_id);
CREATE INDEX sports_reservations_slot_idx ON sports_reservations(app_id, venue_id, project_id, slot_id, status);
CREATE TABLE sports_schedule_items (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  schedule_id TEXT NOT NULL CHECK(length(schedule_id) BETWEEN 1 AND 128),
  reservation_id TEXT NOT NULL CHECK(length(reservation_id) BETWEEN 1 AND 128),
  title TEXT NOT NULL CHECK(length(title) BETWEEN 1 AND 256),
  start_at TEXT NOT NULL,
  end_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (app_id, user_id, schedule_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT,
  FOREIGN KEY (app_id, reservation_id) REFERENCES sports_reservations(app_id, reservation_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX sports_schedule_reservation_unique
  ON sports_schedule_items(app_id, user_id, reservation_id);
`)
}

func (s *Store) ReplaceSportsSnapshot(ctx context.Context, snapshot sports.CatalogSnapshot) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "replace_sports_snapshot", started, resultErr) }()
	if err := validateSportsSnapshot(ctx, snapshot); err != nil {
		return err
	}
	normalizedWebView, err := sports.NormalizeWebViewDescriptor(snapshot.WebView)
	if err != nil {
		return fmt.Errorf("invalid sports webview descriptor")
	}
	snapshot.WebView = normalizedWebView
	headersJSON, err := json.Marshal(snapshot.WebView.RequiredHeaders)
	if err != nil {
		return fmt.Errorf("encode sports webview headers: %w", err)
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sports snapshot transaction: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "replace sports snapshot")
	for _, table := range []string{"sports_slots", "sports_projects", "sports_venues", "sports_webview_descriptors"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE app_id = ?", snapshot.AppID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_source_revisions(app_id,revision,source,authoritative,complete,imported_at,valid_until)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(app_id,revision) DO UPDATE SET
  source=excluded.source,
  authoritative=excluded.authoritative,
  complete=excluded.complete,
  imported_at=excluded.imported_at,
  valid_until=excluded.valid_until`,
		snapshot.AppID, snapshot.Revision, snapshot.Source, boolInt(snapshot.Authoritative), boolInt(snapshot.Complete),
		snapshot.ImportedAt.UTC().Format(time.RFC3339Nano), snapshot.ValidUntil.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("write sports source revision: %w", err)
	}
	for _, venue := range snapshot.Venues {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_venues(app_id,venue_id,name,campus,address,source_revision) VALUES(?,?,?,?,?,?)`,
			snapshot.AppID, venue.ID, venue.Name, venue.Campus, venue.Address, snapshot.Revision,
		); err != nil {
			return fmt.Errorf("insert sports venue: %w", err)
		}
	}
	for _, project := range snapshot.Projects {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_projects(app_id,venue_id,project_id,name,source_revision) VALUES(?,?,?,?,?)`,
			snapshot.AppID, project.VenueID, project.ID, project.Name, snapshot.Revision,
		); err != nil {
			return fmt.Errorf("insert sports project: %w", err)
		}
	}
	for _, slot := range snapshot.Slots {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_slots(app_id,venue_id,project_id,slot_id,slot_date,start_at,end_at,capacity,remaining_quota,source_revision)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
			snapshot.AppID, slot.VenueID, slot.ProjectID, slot.ID, slot.Date,
			slot.StartAt.UTC().Format(time.RFC3339Nano), slot.EndAt.UTC().Format(time.RFC3339Nano),
			slot.Capacity, slot.Capacity, snapshot.Revision,
		); err != nil {
			return fmt.Errorf("insert sports slot: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_webview_descriptors(app_id,revision,entry_url,required_user_agent,required_headers,requires_delegated_auth)
VALUES(?,?,?,?,?,?)`,
		snapshot.AppID, snapshot.Revision, snapshot.WebView.EntryURL, snapshot.WebView.RequiredUserAgent,
		string(headersJSON), boolInt(snapshot.WebView.RequiresDelegatedAuth),
	); err != nil {
		return fmt.Errorf("insert sports webview descriptor: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_current_snapshots(app_id,revision,activated_at) VALUES(?,?,?)
ON CONFLICT(app_id) DO UPDATE SET revision=excluded.revision, activated_at=excluded.activated_at`,
		snapshot.AppID, snapshot.Revision, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("activate sports snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sports_slots
SET remaining_quota = capacity - COALESCE((
  SELECT SUM(r.count) FROM sports_reservations r
  WHERE r.app_id=sports_slots.app_id AND r.venue_id=sports_slots.venue_id
    AND r.project_id=sports_slots.project_id AND r.slot_id=sports_slots.slot_id
    AND r.status='confirmed'
), 0)
WHERE app_id=?`, snapshot.AppID); err != nil {
		return fmt.Errorf("recompute sports remaining quota: %w", err)
	}
	var overbooked int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM sports_slots WHERE app_id=? AND remaining_quota < 0`, snapshot.AppID).Scan(&overbooked); err != nil {
		return fmt.Errorf("check sports remaining quota: %w", err)
	}
	if overbooked > 0 {
		return fmt.Errorf("sports snapshot capacity would drop below confirmed reservations")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sports snapshot: %w", err)
	}
	observe.Info(ctx, "运动场馆快照已替换",
		observe.StringAttr("app_id", snapshot.AppID),
		observe.StringAttr("source_revision", snapshot.Revision),
		observe.StringAttr("source", snapshot.Source),
		observe.BoolAttr("authoritative", snapshot.Authoritative),
		observe.IntAttr("venue_count", len(snapshot.Venues)),
		observe.IntAttr("project_count", len(snapshot.Projects)),
		observe.IntAttr("slot_count", len(snapshot.Slots)),
		observe.Duration(started),
	)
	return nil
}

func (s *Store) ListVenues(ctx context.Context, appID string) (_ sports.VenueSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_sports_venues", started, resultErr) }()
	tx, metadata, err := s.beginSportsSnapshotRead(ctx, appID)
	if err != nil {
		return sports.VenueSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "list sports venues")
	rows, err := tx.QueryContext(ctx, `
SELECT venue_id,name,campus,address,source_revision
FROM sports_venues WHERE app_id=? AND source_revision=? ORDER BY name,venue_id`, appID, metadata.Revision)
	if err != nil {
		return sports.VenueSnapshot{}, err
	}
	defer rows.Close()
	var venues []sports.Venue
	for rows.Next() {
		var item sports.Venue
		if err := rows.Scan(&item.ID, &item.Name, &item.Campus, &item.Address, &item.SourceRevision); err != nil {
			return sports.VenueSnapshot{}, err
		}
		venues = append(venues, item)
	}
	if err := rows.Err(); err != nil {
		return sports.VenueSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return sports.VenueSnapshot{}, err
	}
	if venues == nil {
		venues = []sports.Venue{}
	}
	return sports.VenueSnapshot{Metadata: metadata, Venues: venues}, nil
}

func (s *Store) ListProjects(ctx context.Context, appID string, request sports.ProjectListRequest) (_ sports.ProjectSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_sports_projects", started, resultErr) }()
	if err := request.NormalizeAndValidate(); err != nil {
		return sports.ProjectSnapshot{}, err
	}
	tx, metadata, err := s.beginSportsSnapshotRead(ctx, appID)
	if err != nil {
		return sports.ProjectSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "list sports projects")
	rows, err := tx.QueryContext(ctx, `
SELECT project_id,venue_id,name,source_revision
FROM sports_projects WHERE app_id=? AND venue_id=? AND source_revision=? ORDER BY name,project_id`,
		appID, request.VenueID, metadata.Revision)
	if err != nil {
		return sports.ProjectSnapshot{}, err
	}
	defer rows.Close()
	var projects []sports.Project
	for rows.Next() {
		var item sports.Project
		if err := rows.Scan(&item.ID, &item.VenueID, &item.Name, &item.SourceRevision); err != nil {
			return sports.ProjectSnapshot{}, err
		}
		projects = append(projects, item)
	}
	if err := rows.Err(); err != nil {
		return sports.ProjectSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return sports.ProjectSnapshot{}, err
	}
	if projects == nil {
		projects = []sports.Project{}
	}
	return sports.ProjectSnapshot{Metadata: metadata, Projects: projects}, nil
}

func (s *Store) SearchSlots(ctx context.Context, appID string, request sports.SlotSearchRequest) (_ sports.SlotSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "search_sports_slots", started, resultErr) }()
	if err := request.NormalizeAndValidate(); err != nil {
		return sports.SlotSnapshot{}, err
	}
	tx, metadata, err := s.beginSportsSnapshotRead(ctx, appID)
	if err != nil {
		return sports.SlotSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "search sports slots")
	rows, err := tx.QueryContext(ctx, `
SELECT slot_id,venue_id,project_id,slot_date,start_at,end_at,capacity,remaining_quota,source_revision
FROM sports_slots
WHERE app_id=? AND venue_id=? AND project_id=? AND slot_date=? AND source_revision=?
ORDER BY start_at,slot_id`,
		appID, request.VenueID, request.ProjectID, request.Date, metadata.Revision)
	if err != nil {
		return sports.SlotSnapshot{}, err
	}
	defer rows.Close()
	var slots []sports.Slot
	for rows.Next() {
		item, err := scanSportsSlot(rows)
		if err != nil {
			return sports.SlotSnapshot{}, err
		}
		slots = append(slots, item)
	}
	if err := rows.Err(); err != nil {
		return sports.SlotSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return sports.SlotSnapshot{}, err
	}
	if slots == nil {
		slots = []sports.Slot{}
	}
	return sports.SlotSnapshot{Metadata: metadata, Slots: slots}, nil
}

func (s *Store) CreateReservation(ctx context.Context, input sports.CreateReservationInput) (_ sports.Reservation, _ sports.SnapshotMetadata, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_sports_reservation", started, resultErr) }()
	if err := identity.ValidateAppID(input.AppID); err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, sports.ErrInvalid
	}
	if err := identity.ValidateUserID(input.UserID); err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, sports.ErrUserRequired
	}
	request := sports.CreateReservationRequest{VenueID: input.VenueID, ProjectID: input.ProjectID, SlotID: input.SlotID, Count: input.Count}
	if err := request.NormalizeAndValidate(); err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, err
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, err
	}
	defer s.finishTx(tx, &resultErr, "create sports reservation")
	metadata, err := readSportsSnapshotMetadata(ctx, tx, input.AppID)
	if err != nil {
		return sports.Reservation{}, metadata, err
	}
	slot, venueName, projectName, err := loadSportsSlotForUpdate(ctx, tx, input.AppID, request.VenueID, request.ProjectID, request.SlotID, metadata.Revision)
	if err != nil {
		return sports.Reservation{}, metadata, err
	}
	if !slot.EndAt.After(now) {
		return sports.Reservation{}, metadata, sports.ErrSlotExpired
	}
	if request.Count > slot.RemainingQuota {
		return sports.Reservation{}, metadata, sports.ErrOverQuota
	}
	reservationID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_reservations(
  app_id,reservation_id,user_id,venue_id,project_id,slot_id,count,status,source_revision,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		input.AppID, reservationID, input.UserID, request.VenueID, request.ProjectID, request.SlotID,
		request.Count, sports.StatusConfirmed, metadata.Revision, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	); err != nil {
		if isUniqueConstraint(err) {
			return sports.Reservation{}, metadata, sports.ErrConflict
		}
		return sports.Reservation{}, metadata, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE sports_slots SET remaining_quota = remaining_quota - ?
WHERE app_id=? AND venue_id=? AND project_id=? AND slot_id=? AND source_revision=? AND remaining_quota >= ?`,
		request.Count, input.AppID, request.VenueID, request.ProjectID, request.SlotID, metadata.Revision, request.Count)
	if err != nil {
		return sports.Reservation{}, metadata, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sports.Reservation{}, metadata, err
	}
	if affected != 1 {
		return sports.Reservation{}, metadata, sports.ErrOverQuota
	}
	if err := tx.Commit(); err != nil {
		return sports.Reservation{}, metadata, err
	}
	return sports.Reservation{
		AppID: input.AppID, UserID: input.UserID, ID: reservationID,
		VenueID: request.VenueID, ProjectID: request.ProjectID, SlotID: request.SlotID,
		VenueName: venueName, ProjectName: projectName, Count: request.Count,
		Status: sports.StatusConfirmed, StartAt: slot.StartAt, EndAt: slot.EndAt,
		SourceRevision: metadata.Revision, CreatedAt: now, UpdatedAt: now,
	}, metadata, nil
}

func (s *Store) CancelReservation(ctx context.Context, input sports.CancelReservationInput) (_ sports.Reservation, _ sports.SnapshotMetadata, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_sports_reservation", started, resultErr) }()
	if err := identity.ValidateAppID(input.AppID); err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, sports.ErrInvalid
	}
	if err := identity.ValidateUserID(input.UserID); err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, sports.ErrUserRequired
	}
	request := sports.CancelReservationRequest{ReservationID: input.ReservationID}
	if err := request.NormalizeAndValidate(); err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, err
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return sports.Reservation{}, sports.SnapshotMetadata{}, err
	}
	defer s.finishTx(tx, &resultErr, "cancel sports reservation")
	metadata, err := readSportsSnapshotMetadata(ctx, tx, input.AppID)
	if err != nil {
		return sports.Reservation{}, metadata, err
	}
	if err := expireSportsReservations(ctx, tx, input.AppID, input.UserID, now); err != nil {
		return sports.Reservation{}, metadata, err
	}
	item, err := loadSportsReservation(ctx, tx, input.AppID, input.UserID, request.ReservationID)
	if err != nil {
		return sports.Reservation{}, metadata, err
	}
	if item.Status == sports.StatusCancelled {
		if err := tx.Commit(); err != nil {
			return sports.Reservation{}, metadata, err
		}
		return item, metadata, nil
	}
	if item.Status != sports.StatusConfirmed {
		return sports.Reservation{}, metadata, sports.ErrNotCancellable
	}
	cancelledAt := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
UPDATE sports_reservations SET status=?, updated_at=?, cancelled_at=?
WHERE app_id=? AND user_id=? AND reservation_id=? AND status=?`,
		sports.StatusCancelled, cancelledAt, cancelledAt, input.AppID, input.UserID, request.ReservationID, sports.StatusConfirmed,
	); err != nil {
		return sports.Reservation{}, metadata, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE sports_slots SET remaining_quota = remaining_quota + ?
WHERE app_id=? AND venue_id=? AND project_id=? AND slot_id=?`,
		item.Count, input.AppID, item.VenueID, item.ProjectID, item.SlotID,
	); err != nil {
		return sports.Reservation{}, metadata, err
	}
	if err := tx.Commit(); err != nil {
		return sports.Reservation{}, metadata, err
	}
	item.Status = sports.StatusCancelled
	item.UpdatedAt = now
	item.CancelledAt = &now
	return item, metadata, nil
}

func (s *Store) ListMyReservations(ctx context.Context, appID, userID string, now time.Time) (_ sports.ReservationListSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_sports_reservations", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return sports.ReservationListSnapshot{}, sports.ErrInvalid
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return sports.ReservationListSnapshot{}, sports.ErrUserRequired
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return sports.ReservationListSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "list sports reservations")
	metadata, err := readSportsSnapshotMetadata(ctx, tx, appID)
	if err != nil {
		return sports.ReservationListSnapshot{}, err
	}
	if err := expireSportsReservations(ctx, tx, appID, userID, now); err != nil {
		return sports.ReservationListSnapshot{}, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT r.reservation_id,r.venue_id,r.project_id,r.slot_id,r.count,r.status,r.source_revision,r.created_at,r.updated_at,r.cancelled_at,
       COALESCE(v.name,''),COALESCE(p.name,''),COALESCE(s.start_at,''),COALESCE(s.end_at,'')
FROM sports_reservations r
LEFT JOIN sports_venues v ON v.app_id=r.app_id AND v.venue_id=r.venue_id
LEFT JOIN sports_projects p ON p.app_id=r.app_id AND p.venue_id=r.venue_id AND p.project_id=r.project_id
LEFT JOIN sports_slots s ON s.app_id=r.app_id AND s.venue_id=r.venue_id AND s.project_id=r.project_id AND s.slot_id=r.slot_id
WHERE r.app_id=? AND r.user_id=?
ORDER BY r.created_at DESC, r.reservation_id`, appID, userID)
	if err != nil {
		return sports.ReservationListSnapshot{}, err
	}
	defer rows.Close()
	var items []sports.Reservation
	for rows.Next() {
		item, err := scanSportsReservation(rows, appID, userID)
		if err != nil {
			return sports.ReservationListSnapshot{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return sports.ReservationListSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return sports.ReservationListSnapshot{}, err
	}
	if items == nil {
		items = []sports.Reservation{}
	}
	return sports.ReservationListSnapshot{Metadata: metadata, Reservations: items}, nil
}

func (s *Store) GetWebViewDescriptor(ctx context.Context, appID string) (_ sports.WebViewSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "get_sports_webview", started, resultErr) }()
	tx, metadata, err := s.beginSportsSnapshotRead(ctx, appID)
	if err != nil {
		return sports.WebViewSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "get sports webview")
	var descriptor sports.WebViewDescriptor
	var headersJSON string
	var requiresAuth int
	err = tx.QueryRowContext(ctx, `
SELECT entry_url,required_user_agent,required_headers,requires_delegated_auth
FROM sports_webview_descriptors WHERE app_id=? AND revision=?`, appID, metadata.Revision).
		Scan(&descriptor.EntryURL, &descriptor.RequiredUserAgent, &headersJSON, &requiresAuth)
	if errors.Is(err, sql.ErrNoRows) {
		return sports.WebViewSnapshot{}, contracts.ErrDataUnavailable
	}
	if err != nil {
		return sports.WebViewSnapshot{}, err
	}
	if err := json.Unmarshal([]byte(headersJSON), &descriptor.RequiredHeaders); err != nil {
		return sports.WebViewSnapshot{}, fmt.Errorf("decode sports webview headers: %w", err)
	}
	descriptor.RequiresDelegatedAuth = requiresAuth == 1
	descriptor.SourceRevision = metadata.Revision
	normalized, err := sports.NormalizeWebViewDescriptor(descriptor)
	if err != nil {
		return sports.WebViewSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return sports.WebViewSnapshot{}, err
	}
	return sports.WebViewSnapshot{Metadata: metadata, Descriptor: normalized}, nil
}

func (s *Store) AddScheduleItem(ctx context.Context, input sports.AddScheduleInput) (_ sports.ScheduleItem, _ sports.SnapshotMetadata, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "add_sports_schedule", started, resultErr) }()
	if err := identity.ValidateAppID(input.AppID); err != nil {
		return sports.ScheduleItem{}, sports.SnapshotMetadata{}, sports.ErrInvalid
	}
	if err := identity.ValidateUserID(input.UserID); err != nil {
		return sports.ScheduleItem{}, sports.SnapshotMetadata{}, sports.ErrUserRequired
	}
	request := sports.AddScheduleRequest{ReservationID: input.ReservationID}
	if err := request.NormalizeAndValidate(); err != nil {
		return sports.ScheduleItem{}, sports.SnapshotMetadata{}, err
	}
	now := input.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return sports.ScheduleItem{}, sports.SnapshotMetadata{}, err
	}
	defer s.finishTx(tx, &resultErr, "add sports schedule")
	metadata, err := readSportsSnapshotMetadata(ctx, tx, input.AppID)
	if err != nil {
		return sports.ScheduleItem{}, metadata, err
	}
	if err := expireSportsReservations(ctx, tx, input.AppID, input.UserID, now); err != nil {
		return sports.ScheduleItem{}, metadata, err
	}
	existing, err := loadSportsScheduleByReservation(ctx, tx, input.AppID, input.UserID, request.ReservationID)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return sports.ScheduleItem{}, metadata, err
		}
		return existing, metadata, nil
	}
	if !errors.Is(err, sports.ErrNotFound) {
		return sports.ScheduleItem{}, metadata, err
	}
	reservation, err := loadSportsReservation(ctx, tx, input.AppID, input.UserID, request.ReservationID)
	if err != nil {
		return sports.ScheduleItem{}, metadata, err
	}
	if reservation.Status != sports.StatusConfirmed && reservation.Status != sports.StatusExpired {
		return sports.ScheduleItem{}, metadata, sports.ErrInvalid
	}
	title := strings.TrimSpace(reservation.VenueName + " " + reservation.ProjectName)
	if title == "" {
		title = "运动场馆预约"
	}
	if utf8.RuneCountInString(title) > 256 {
		title = string([]rune(title)[:256])
	}
	item := sports.ScheduleItem{
		AppID: input.AppID, UserID: input.UserID, ID: uuid.NewString(),
		ReservationID: reservation.ID, Title: title, StartAt: reservation.StartAt, EndAt: reservation.EndAt, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sports_schedule_items(app_id,user_id,schedule_id,reservation_id,title,start_at,end_at,created_at)
VALUES(?,?,?,?,?,?,?,?)`,
		item.AppID, item.UserID, item.ID, item.ReservationID, item.Title,
		item.StartAt.UTC().Format(time.RFC3339Nano), item.EndAt.UTC().Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	); err != nil {
		if isUniqueConstraint(err) {
			existing, loadErr := loadSportsScheduleByReservation(ctx, tx, input.AppID, input.UserID, request.ReservationID)
			if loadErr != nil {
				return sports.ScheduleItem{}, metadata, sports.ErrConflict
			}
			if err := tx.Commit(); err != nil {
				return sports.ScheduleItem{}, metadata, err
			}
			return existing, metadata, nil
		}
		return sports.ScheduleItem{}, metadata, err
	}
	if err := tx.Commit(); err != nil {
		return sports.ScheduleItem{}, metadata, err
	}
	return item, metadata, nil
}

func (s *Store) beginSportsSnapshotRead(ctx context.Context, appID string) (*sql.Tx, sports.SnapshotMetadata, error) {
	if err := identity.ValidateAppID(appID); err != nil {
		return nil, sports.SnapshotMetadata{}, contracts.ErrDataUnavailable
	}
	tx, err := s.beginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, sports.SnapshotMetadata{}, fmt.Errorf("begin sports snapshot read: %w", err)
	}
	metadata, err := readSportsSnapshotMetadata(ctx, tx, appID)
	if err != nil {
		return nil, sports.SnapshotMetadata{}, s.rollbackTx(tx, err, "read sports snapshot")
	}
	return tx, metadata, nil
}

func readSportsSnapshotMetadata(ctx context.Context, tx *sql.Tx, appID string) (sports.SnapshotMetadata, error) {
	var metadata sports.SnapshotMetadata
	var authoritative, complete int
	var importedAt, validUntil string
	err := tx.QueryRowContext(ctx, `
SELECT source.revision,source.source,source.authoritative,source.complete,source.imported_at,source.valid_until
FROM sports_current_snapshots current
JOIN sports_source_revisions source
  ON source.app_id=current.app_id AND source.revision=current.revision
WHERE current.app_id=?`, appID).Scan(
		&metadata.Revision, &metadata.Source, &authoritative, &complete, &importedAt, &validUntil,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return sports.SnapshotMetadata{}, contracts.ErrDataUnavailable
	}
	if err != nil {
		return sports.SnapshotMetadata{}, fmt.Errorf("read current sports snapshot: %w", err)
	}
	metadata.Authoritative = authoritative == 1
	metadata.Complete = complete == 1
	metadata.ImportedAt, err = parseSportsTime(importedAt)
	if err != nil {
		return sports.SnapshotMetadata{}, fmt.Errorf("parse sports snapshot import time: %w", err)
	}
	metadata.ValidUntil, err = parseSportsTime(validUntil)
	if err != nil {
		return sports.SnapshotMetadata{}, fmt.Errorf("parse sports snapshot validity: %w", err)
	}
	return metadata, nil
}

func loadSportsSlotForUpdate(ctx context.Context, tx *sql.Tx, appID, venueID, projectID, slotID, revision string) (sports.Slot, string, string, error) {
	var slot sports.Slot
	var startAt, endAt, venueName, projectName string
	err := tx.QueryRowContext(ctx, `
SELECT s.slot_id,s.venue_id,s.project_id,s.slot_date,s.start_at,s.end_at,s.capacity,s.remaining_quota,s.source_revision,
       v.name,p.name
FROM sports_slots s
JOIN sports_venues v ON v.app_id=s.app_id AND v.venue_id=s.venue_id
JOIN sports_projects p ON p.app_id=s.app_id AND p.venue_id=s.venue_id AND p.project_id=s.project_id
WHERE s.app_id=? AND s.venue_id=? AND s.project_id=? AND s.slot_id=? AND s.source_revision=?`,
		appID, venueID, projectID, slotID, revision,
	).Scan(&slot.ID, &slot.VenueID, &slot.ProjectID, &slot.Date, &startAt, &endAt, &slot.Capacity, &slot.RemainingQuota, &slot.SourceRevision, &venueName, &projectName)
	if errors.Is(err, sql.ErrNoRows) {
		return sports.Slot{}, "", "", sports.ErrNotFound
	}
	if err != nil {
		return sports.Slot{}, "", "", err
	}
	slot.StartAt, err = parseSportsTime(startAt)
	if err != nil {
		return sports.Slot{}, "", "", err
	}
	slot.EndAt, err = parseSportsTime(endAt)
	if err != nil {
		return sports.Slot{}, "", "", err
	}
	return slot, venueName, projectName, nil
}

func expireSportsReservations(ctx context.Context, tx *sql.Tx, appID, userID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE sports_reservations
SET status=?, updated_at=?
WHERE app_id=? AND user_id=? AND status=? AND EXISTS (
  SELECT 1 FROM sports_slots s
  WHERE s.app_id=sports_reservations.app_id
    AND s.venue_id=sports_reservations.venue_id
    AND s.project_id=sports_reservations.project_id
    AND s.slot_id=sports_reservations.slot_id
    AND julianday(s.end_at) <= julianday(?)
)`,
		sports.StatusExpired, now.Format(time.RFC3339Nano),
		appID, userID, sports.StatusConfirmed, now.Format(time.RFC3339Nano),
	)
	return err
}

func loadSportsReservation(ctx context.Context, tx *sql.Tx, appID, userID, reservationID string) (sports.Reservation, error) {
	row := tx.QueryRowContext(ctx, `
SELECT r.reservation_id,r.venue_id,r.project_id,r.slot_id,r.count,r.status,r.source_revision,r.created_at,r.updated_at,r.cancelled_at,
       COALESCE(v.name,''),COALESCE(p.name,''),COALESCE(s.start_at,''),COALESCE(s.end_at,'')
FROM sports_reservations r
LEFT JOIN sports_venues v ON v.app_id=r.app_id AND v.venue_id=r.venue_id
LEFT JOIN sports_projects p ON p.app_id=r.app_id AND p.venue_id=r.venue_id AND p.project_id=r.project_id
LEFT JOIN sports_slots s ON s.app_id=r.app_id AND s.venue_id=r.venue_id AND s.project_id=r.project_id AND s.slot_id=r.slot_id
WHERE r.app_id=? AND r.user_id=? AND r.reservation_id=?`, appID, userID, reservationID)
	item, err := scanSportsReservation(row, appID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return sports.Reservation{}, sports.ErrNotFound
	}
	return item, err
}

func loadSportsScheduleByReservation(ctx context.Context, tx *sql.Tx, appID, userID, reservationID string) (sports.ScheduleItem, error) {
	var item sports.ScheduleItem
	var startAt, endAt, createdAt string
	err := tx.QueryRowContext(ctx, `
SELECT schedule_id,reservation_id,title,start_at,end_at,created_at
FROM sports_schedule_items WHERE app_id=? AND user_id=? AND reservation_id=?`,
		appID, userID, reservationID,
	).Scan(&item.ID, &item.ReservationID, &item.Title, &startAt, &endAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sports.ScheduleItem{}, sports.ErrNotFound
	}
	if err != nil {
		return sports.ScheduleItem{}, err
	}
	item.AppID, item.UserID = appID, userID
	item.StartAt, err = parseSportsTime(startAt)
	if err != nil {
		return sports.ScheduleItem{}, err
	}
	item.EndAt, err = parseSportsTime(endAt)
	if err != nil {
		return sports.ScheduleItem{}, err
	}
	item.CreatedAt, err = parseSportsTime(createdAt)
	if err != nil {
		return sports.ScheduleItem{}, err
	}
	return item, nil
}

type sportsRowScanner interface {
	Scan(dest ...any) error
}

func scanSportsSlot(row sportsRowScanner) (sports.Slot, error) {
	var item sports.Slot
	var startAt, endAt string
	if err := row.Scan(&item.ID, &item.VenueID, &item.ProjectID, &item.Date, &startAt, &endAt, &item.Capacity, &item.RemainingQuota, &item.SourceRevision); err != nil {
		return sports.Slot{}, err
	}
	var err error
	item.StartAt, err = parseSportsTime(startAt)
	if err != nil {
		return sports.Slot{}, err
	}
	item.EndAt, err = parseSportsTime(endAt)
	if err != nil {
		return sports.Slot{}, err
	}
	return item, nil
}

func scanSportsReservation(row sportsRowScanner, appID, userID string) (sports.Reservation, error) {
	var item sports.Reservation
	var createdAt, updatedAt string
	var cancelledAt sql.NullString
	var startAt, endAt string
	if err := row.Scan(
		&item.ID, &item.VenueID, &item.ProjectID, &item.SlotID, &item.Count, &item.Status, &item.SourceRevision,
		&createdAt, &updatedAt, &cancelledAt, &item.VenueName, &item.ProjectName, &startAt, &endAt,
	); err != nil {
		return sports.Reservation{}, err
	}
	var err error
	item.AppID, item.UserID = appID, userID
	item.CreatedAt, err = parseSportsTime(createdAt)
	if err != nil {
		return sports.Reservation{}, err
	}
	item.UpdatedAt, err = parseSportsTime(updatedAt)
	if err != nil {
		return sports.Reservation{}, err
	}
	if cancelledAt.Valid && cancelledAt.String != "" {
		parsed, err := parseSportsTime(cancelledAt.String)
		if err != nil {
			return sports.Reservation{}, err
		}
		item.CancelledAt = &parsed
	}
	if startAt != "" {
		item.StartAt, err = parseSportsTime(startAt)
		if err != nil {
			return sports.Reservation{}, err
		}
	}
	if endAt != "" {
		item.EndAt, err = parseSportsTime(endAt)
		if err != nil {
			return sports.Reservation{}, err
		}
	}
	return item, nil
}

func parseSportsTime(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func validateSportsSnapshot(ctx context.Context, snapshot sports.CatalogSnapshot) error {
	if err := identity.ValidateAppID(snapshot.AppID); err != nil {
		return fmt.Errorf("invalid sports snapshot app_id")
	}
	if !validSportsStableID(snapshot.Revision) || !validSportsText(snapshot.Source, 256) || !snapshot.Complete ||
		snapshot.ImportedAt.IsZero() || snapshot.ValidUntil.IsZero() || !snapshot.ValidUntil.After(snapshot.ImportedAt) ||
		len(snapshot.Venues) == 0 || len(snapshot.Venues) > 1000 || len(snapshot.Projects) == 0 || len(snapshot.Projects) > 5000 ||
		len(snapshot.Slots) == 0 || len(snapshot.Slots) > 50_000 {
		return fmt.Errorf("invalid sports snapshot metadata")
	}
	descriptor := snapshot.WebView
	descriptor.SourceRevision = snapshot.Revision
	normalized, err := sports.NormalizeWebViewDescriptor(descriptor)
	if err != nil {
		return fmt.Errorf("invalid sports webview descriptor")
	}
	if parsed, err := url.Parse(normalized.EntryURL); err != nil || parsed.User != nil {
		return fmt.Errorf("invalid sports webview descriptor")
	}
	snapshot.WebView = normalized
	venueIDs := make(map[string]struct{}, len(snapshot.Venues))
	for _, venue := range snapshot.Venues {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validSportsStableID(venue.ID) || !validSportsText(venue.Name, 256) ||
			utf8.RuneCountInString(venue.Campus) > 256 || utf8.RuneCountInString(venue.Address) > 256 ||
			(venue.SourceRevision != "" && venue.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid sports venue %q", venue.ID)
		}
		if _, duplicate := venueIDs[venue.ID]; duplicate {
			return fmt.Errorf("duplicate sports venue %q", venue.ID)
		}
		venueIDs[venue.ID] = struct{}{}
	}
	projectIDs := make(map[string]struct{}, len(snapshot.Projects))
	for _, project := range snapshot.Projects {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := project.VenueID + "/" + project.ID
		if !validSportsStableID(project.ID) || !validSportsStableID(project.VenueID) || !validSportsText(project.Name, 256) ||
			(project.SourceRevision != "" && project.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid sports project %q", project.ID)
		}
		if _, exists := venueIDs[project.VenueID]; !exists {
			return fmt.Errorf("sports project %q references unknown venue", project.ID)
		}
		if _, duplicate := projectIDs[key]; duplicate {
			return fmt.Errorf("duplicate sports project %q", project.ID)
		}
		projectIDs[key] = struct{}{}
	}
	slotIDs := make(map[string]struct{}, len(snapshot.Slots))
	for _, slot := range snapshot.Slots {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := slot.VenueID + "/" + slot.ProjectID + "/" + slot.ID
		if !validSportsStableID(slot.ID) || !validSportsStableID(slot.VenueID) || !validSportsStableID(slot.ProjectID) ||
			!validSportsSlotDate(slot.Date) || slot.StartAt.IsZero() || slot.EndAt.IsZero() || !slot.EndAt.After(slot.StartAt) ||
			slot.Capacity < 1 || slot.Capacity > 1024 ||
			(slot.SourceRevision != "" && slot.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid sports slot %q", slot.ID)
		}
		if _, exists := projectIDs[slot.VenueID+"/"+slot.ProjectID]; !exists {
			return fmt.Errorf("sports slot %q references unknown project", slot.ID)
		}
		if _, duplicate := slotIDs[key]; duplicate {
			return fmt.Errorf("duplicate sports slot %q", slot.ID)
		}
		slotIDs[key] = struct{}{}
	}
	return nil
}

func validSportsStableID(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && id.StableMixed.MatchString(value)
}

func validSportsText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}

func validSportsSlotDate(value string) bool {
	if len(value) != 10 {
		return false
	}
	_, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	return err == nil
}
