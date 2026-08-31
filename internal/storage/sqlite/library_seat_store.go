package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	libraryseat "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/libraryseat"
)

var _ libraryseat.Store = (*Store)(nil)

func init() {
	// 迁移 26：图书馆座位目录快照与预约状态机。25/27/28 留给并行业务线。
	registerMigration(30, `
CREATE TABLE library_seat_source_revisions (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 256),
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0, 1)),
  complete INTEGER NOT NULL CHECK(complete IN (0, 1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  PRIMARY KEY (app_id, revision)
);
CREATE TABLE library_seat_current_snapshots (
  app_id TEXT PRIMARY KEY CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  activated_at TEXT NOT NULL,
  FOREIGN KEY (app_id, revision) REFERENCES library_seat_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE TABLE library_seat_spaces (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  space_id TEXT NOT NULL CHECK(length(space_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  campus TEXT NOT NULL DEFAULT '' CHECK(length(campus) <= 128),
  building TEXT NOT NULL DEFAULT '' CHECK(length(building) <= 128),
  floor TEXT NOT NULL DEFAULT '' CHECK(length(floor) <= 64),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, space_id),
  FOREIGN KEY (app_id, source_revision) REFERENCES library_seat_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE INDEX library_seat_spaces_name_idx ON library_seat_spaces(app_id, name, space_id);
CREATE TABLE library_seat_seats (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  space_id TEXT NOT NULL CHECK(length(space_id) BETWEEN 1 AND 128),
  seat_id TEXT NOT NULL CHECK(length(seat_id) BETWEEN 1 AND 128),
  label TEXT NOT NULL CHECK(length(label) BETWEEN 1 AND 128),
  area TEXT NOT NULL DEFAULT '' CHECK(length(area) <= 128),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, space_id, seat_id),
  FOREIGN KEY (app_id, space_id) REFERENCES library_seat_spaces(app_id, space_id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, source_revision) REFERENCES library_seat_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE TABLE library_seat_slots (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  slot_id TEXT NOT NULL CHECK(length(slot_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  start_minute INTEGER NOT NULL CHECK(start_minute BETWEEN 0 AND 1439),
  end_minute INTEGER NOT NULL CHECK(end_minute BETWEEN 1 AND 1440 AND end_minute > start_minute),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, slot_id),
  FOREIGN KEY (app_id, source_revision) REFERENCES library_seat_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE TABLE library_seat_reservations (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  reservation_id TEXT NOT NULL CHECK(length(reservation_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  space_id TEXT NOT NULL CHECK(length(space_id) BETWEEN 1 AND 128),
  seat_id TEXT NOT NULL CHECK(length(seat_id) BETWEEN 1 AND 128),
  slot_id TEXT NOT NULL CHECK(length(slot_id) BETWEEN 1 AND 128),
  slot_date TEXT NOT NULL CHECK(length(slot_date)=10 AND slot_date GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
  starts_at TEXT NOT NULL,
  ends_at TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('pending_confirm','confirmed','cancelled','expired','completed')),
  catalog_revision TEXT NOT NULL CHECK(length(catalog_revision) BETWEEN 1 AND 128),
  catalog_source TEXT NOT NULL CHECK(length(catalog_source) BETWEEN 1 AND 256),
  catalog_authoritative INTEGER NOT NULL CHECK(catalog_authoritative IN (0,1)),
  create_idempotency_key TEXT NOT NULL DEFAULT '' CHECK(length(create_idempotency_key) <= 256),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancelled_at TEXT,
  PRIMARY KEY (app_id, reservation_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT,
  CHECK(ends_at > starts_at),
  CHECK((status='cancelled' AND cancelled_at IS NOT NULL) OR (status<>'cancelled' AND cancelled_at IS NULL))
);
CREATE UNIQUE INDEX library_seat_occupancy_idx
  ON library_seat_reservations(app_id, space_id, seat_id, slot_date, slot_id)
  WHERE status IN ('pending_confirm','confirmed');
CREATE UNIQUE INDEX library_seat_create_idempotency_idx
  ON library_seat_reservations(app_id, create_idempotency_key)
  WHERE create_idempotency_key <> '';
CREATE INDEX library_seat_reservations_user_idx
  ON library_seat_reservations(app_id, user_id, starts_at, reservation_id);
CREATE TRIGGER library_seat_reservation_status_guard
BEFORE UPDATE OF status ON library_seat_reservations
WHEN NOT (
  (OLD.status='pending_confirm' AND NEW.status IN ('confirmed','cancelled','expired')) OR
  (OLD.status='confirmed' AND NEW.status IN ('cancelled','expired','completed')) OR
  (OLD.status=NEW.status)
)
BEGIN
  SELECT RAISE(ABORT, 'illegal library seat reservation transition');
END;
`)
}

func (s *Store) ReplaceCatalog(ctx context.Context, snapshot libraryseat.CatalogSnapshot) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "replace_library_seat_catalog", started, resultErr) }()
	if err := libraryseat.ValidateCatalog(snapshot); err != nil {
		return err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin library seat catalog transaction: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "replace library seat catalog")
	for _, table := range []string{"library_seat_seats", "library_seat_spaces", "library_seat_slots"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE app_id = ?", snapshot.AppID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO library_seat_source_revisions(app_id,revision,source,authoritative,complete,imported_at,valid_until)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(app_id,revision) DO UPDATE SET
  source=excluded.source, authoritative=excluded.authoritative, complete=excluded.complete,
  imported_at=excluded.imported_at, valid_until=excluded.valid_until`,
		snapshot.AppID, snapshot.Revision, snapshot.Source,
		boolInt(snapshot.Authoritative), boolInt(snapshot.Complete),
		snapshot.ImportedAt.UTC().Format(time.RFC3339Nano),
		snapshot.ValidUntil.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("write library seat source revision: %w", err)
	}
	for _, space := range snapshot.Spaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO library_seat_spaces(app_id,space_id,name,campus,building,floor,source_revision)
VALUES(?,?,?,?,?,?,?)`,
			snapshot.AppID, space.ID, space.Name, space.Campus, space.Building, space.Floor, snapshot.Revision); err != nil {
			return fmt.Errorf("insert library space %q: %w", space.ID, err)
		}
	}
	for _, seat := range snapshot.Seats {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO library_seat_seats(app_id,space_id,seat_id,label,area,source_revision)
VALUES(?,?,?,?,?,?)`,
			snapshot.AppID, seat.SpaceID, seat.ID, seat.Label, seat.Area, snapshot.Revision); err != nil {
			return fmt.Errorf("insert library seat %q: %w", seat.ID, err)
		}
	}
	for _, slot := range snapshot.Slots {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO library_seat_slots(app_id,slot_id,name,start_minute,end_minute,source_revision)
VALUES(?,?,?,?,?,?)`,
			snapshot.AppID, slot.ID, slot.Name, slot.StartMinute, slot.EndMinute, snapshot.Revision); err != nil {
			return fmt.Errorf("insert library slot %q: %w", slot.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO library_seat_current_snapshots(app_id,revision,activated_at) VALUES(?,?,?)
ON CONFLICT(app_id) DO UPDATE SET revision=excluded.revision, activated_at=excluded.activated_at`,
		snapshot.AppID, snapshot.Revision, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("activate library seat catalog: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit library seat catalog: %w", err)
	}
	observe.Info(ctx, "图书馆座位目录快照已经原子替换",
		observe.Component("storage"),
		observe.StringAttr("app_id", snapshot.AppID),
		observe.StringAttr("source_revision", snapshot.Revision),
		observe.StringAttr("source", snapshot.Source),
		observe.BoolAttr("authoritative", snapshot.Authoritative),
		observe.IntAttr("space_count", len(snapshot.Spaces)),
		observe.IntAttr("seat_count", len(snapshot.Seats)),
		observe.IntAttr("slot_count", len(snapshot.Slots)),
		observe.Duration(started),
	)
	return nil
}

func (s *Store) ListSpaces(ctx context.Context, appID string, request libraryseat.SpaceListRequest) (_ libraryseat.SpaceSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_library_spaces", started, resultErr) }()
	tx, metadata, err := s.beginLibrarySeatSnapshotRead(ctx, appID)
	if err != nil {
		return libraryseat.SpaceSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "list library spaces")
	rows, err := tx.QueryContext(ctx, `
SELECT space_id,name,campus,building,floor,source_revision
FROM library_seat_spaces
WHERE app_id=? AND source_revision=?
ORDER BY name, space_id
LIMIT ?`, appID, metadata.Revision, request.Limit)
	if err != nil {
		return libraryseat.SpaceSnapshot{}, err
	}
	defer rows.Close()
	var spaces []libraryseat.Space
	for rows.Next() {
		var space libraryseat.Space
		if err := rows.Scan(&space.ID, &space.Name, &space.Campus, &space.Building, &space.Floor, &space.SourceRevision); err != nil {
			return libraryseat.SpaceSnapshot{}, err
		}
		spaces = append(spaces, space)
	}
	if err := rows.Err(); err != nil {
		return libraryseat.SpaceSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return libraryseat.SpaceSnapshot{}, err
	}
	if spaces == nil {
		spaces = []libraryseat.Space{}
	}
	return libraryseat.SpaceSnapshot{Metadata: metadata, Spaces: spaces}, nil
}

func (s *Store) SearchSeats(ctx context.Context, appID string, request libraryseat.SlotSearchRequest) (_ libraryseat.SeatSnapshot, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "search_library_seats", started, resultErr) }()
	now := time.Now().UTC()
	tx, metadata, err := s.beginLibrarySeatSnapshotWrite(ctx, appID)
	if err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "search library seats")
	if err := expireLibrarySeatReservations(ctx, tx, appID, now); err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	snapshot, err := loadSeatSnapshot(ctx, tx, appID, metadata, request)
	if err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) CreateReservation(ctx context.Context, input libraryseat.CreateReservationInput, now time.Time) (_ libraryseat.Reservation, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_library_seat_reservation", started, resultErr) }()
	if err := libraryseat.RequireApp(input.AppID); err != nil {
		return libraryseat.Reservation{}, err
	}
	if err := libraryseat.RequireUser(input.UserID); err != nil {
		return libraryseat.Reservation{}, err
	}
	if input.IdempotencyKey != "" && (utf8.RuneCountInString(input.IdempotencyKey) > 256 || !utf8.ValidString(input.IdempotencyKey)) {
		return libraryseat.Reservation{}, libraryseat.ErrInvalid
	}
	tx, metadata, err := s.beginLibrarySeatSnapshotWrite(ctx, input.AppID)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	defer s.finishTx(tx, &resultErr, "create library seat reservation")
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if err := expireLibrarySeatReservations(ctx, tx, input.AppID, now); err != nil {
		return libraryseat.Reservation{}, err
	}
	if input.IdempotencyKey != "" {
		existing, found, err := loadReservationByCreateKey(ctx, tx, input.AppID, input.IdempotencyKey)
		if err != nil {
			return libraryseat.Reservation{}, err
		}
		if found {
			if existing.UserID == input.UserID && existing.SpaceID == input.SpaceID &&
				existing.SeatID == input.SeatID && existing.SlotID == input.SlotID && existing.Date == input.Date {
				if err := tx.Commit(); err != nil {
					return libraryseat.Reservation{}, err
				}
				return existing, nil
			}
			return libraryseat.Reservation{}, libraryseat.ErrIdempotencyConflict
		}
	}
	space, seat, slot, err := loadCatalogRefs(ctx, tx, input.AppID, metadata.Revision, input.SpaceID, input.SeatID, input.SlotID)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	startsAt, endsAt, err := libraryseat.SlotBounds(input.Date, slot.StartMinute, slot.EndMinute)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	if !endsAt.After(now) {
		return libraryseat.Reservation{}, libraryseat.ErrSlotInPast
	}
	var active int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM library_seat_reservations
WHERE app_id=? AND user_id=? AND status IN ('pending_confirm','confirmed')`,
		input.AppID, input.UserID).Scan(&active); err != nil {
		return libraryseat.Reservation{}, err
	}
	if active >= libraryseat.MaxActiveReservationsPerUser {
		return libraryseat.Reservation{}, libraryseat.ErrQuotaExceeded
	}
	reservationID := uuid.NewString()
	createdAt := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO library_seat_reservations(
  app_id,reservation_id,user_id,space_id,seat_id,slot_id,slot_date,starts_at,ends_at,status,
  catalog_revision,catalog_source,catalog_authoritative,create_idempotency_key,created_at,updated_at,cancelled_at)
VALUES(?,?,?,?,?,?,?,?,?,'confirmed',?,?,?,?,?,?,NULL)`,
		input.AppID, reservationID, input.UserID, input.SpaceID, input.SeatID, input.SlotID, input.Date,
		startsAt.Format(time.RFC3339Nano), endsAt.Format(time.RFC3339Nano),
		metadata.Revision, metadata.Source, boolInt(metadata.Authoritative), input.IdempotencyKey, createdAt, createdAt,
	); err != nil {
		if isUniqueConstraint(err) {
			if input.IdempotencyKey != "" {
				existing, found, loadErr := loadReservationByCreateKey(ctx, tx, input.AppID, input.IdempotencyKey)
				if loadErr != nil {
					return libraryseat.Reservation{}, loadErr
				}
				if found {
					return libraryseat.Reservation{}, libraryseat.ErrIdempotencyConflict
				}
				_ = existing
			}
			return libraryseat.Reservation{}, libraryseat.ErrConflict
		}
		return libraryseat.Reservation{}, fmt.Errorf("insert library seat reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return libraryseat.Reservation{}, err
	}
	return libraryseat.Reservation{
		ID: reservationID, AppID: input.AppID, UserID: input.UserID,
		SpaceID: space.ID, SpaceName: space.Name, SeatID: seat.ID, SeatLabel: seat.Label,
		SlotID: slot.ID, SlotName: slot.Name, Date: input.Date, StartsAt: startsAt, EndsAt: endsAt,
		Status: libraryseat.ReservationConfirmed, CatalogRevision: metadata.Revision,
		CatalogSource: metadata.Source, CatalogAuthoritative: metadata.Authoritative,
		CreateIdempotencyKey: input.IdempotencyKey, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) CancelReservation(ctx context.Context, input libraryseat.CancelReservationInput, now time.Time) (_ libraryseat.Reservation, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_library_seat_reservation", started, resultErr) }()
	if err := libraryseat.RequireApp(input.AppID); err != nil {
		return libraryseat.Reservation{}, err
	}
	if err := libraryseat.RequireUser(input.UserID); err != nil {
		return libraryseat.Reservation{}, err
	}
	if input.ReservationID == "" || !id.StableMixed.MatchString(input.ReservationID) {
		return libraryseat.Reservation{}, libraryseat.ErrReservationRequired
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	defer s.finishTx(tx, &resultErr, "cancel library seat reservation")
	if err := expireLibrarySeatReservations(ctx, tx, input.AppID, now); err != nil {
		return libraryseat.Reservation{}, err
	}
	current, err := loadReservation(ctx, tx, input.AppID, input.ReservationID)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	if current.UserID != input.UserID {
		return libraryseat.Reservation{}, libraryseat.ErrNotOwner
	}
	switch current.Status {
	case libraryseat.ReservationCancelled:
		if err := tx.Commit(); err != nil {
			return libraryseat.Reservation{}, err
		}
		return current, nil
	case libraryseat.ReservationConfirmed, libraryseat.ReservationPendingConfirm:
	default:
		return libraryseat.Reservation{}, libraryseat.ErrIllegalTransition
	}
	cancelledAt := now
	if _, err := tx.ExecContext(ctx, `
UPDATE library_seat_reservations
SET status='cancelled', cancelled_at=?, updated_at=?
WHERE app_id=? AND reservation_id=? AND status IN ('pending_confirm','confirmed')`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), input.AppID, input.ReservationID); err != nil {
		if isIllegalTransition(err) {
			return libraryseat.Reservation{}, libraryseat.ErrIllegalTransition
		}
		return libraryseat.Reservation{}, err
	}
	updated, err := loadReservation(ctx, tx, input.AppID, input.ReservationID)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return libraryseat.Reservation{}, err
	}
	updated.CancelledAt = &cancelledAt
	return updated, nil
}

func (s *Store) ListMyReservations(ctx context.Context, appID, userID string, request libraryseat.MineRequest, now time.Time) (_ []libraryseat.Reservation, _ libraryseat.SnapshotMetadata, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_library_seat_reservations", started, resultErr) }()
	if err := libraryseat.RequireApp(appID); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	if err := libraryseat.RequireUser(userID); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	defer s.finishTx(tx, &resultErr, "list library seat reservations")
	if err := expireLibrarySeatReservations(ctx, tx, appID, now); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	metadata, err := readLibrarySeatMetadata(ctx, tx, appID)
	if err != nil && !errors.Is(err, contracts.ErrDataUnavailable) {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT r.reservation_id,r.user_id,r.space_id,COALESCE(sp.name,''),r.seat_id,COALESCE(st.label,''),
       r.slot_id,COALESCE(sl.name,''),r.slot_date,r.starts_at,r.ends_at,r.status,
       r.catalog_revision,r.catalog_source,r.catalog_authoritative,r.create_idempotency_key,
       r.created_at,r.updated_at,r.cancelled_at
FROM library_seat_reservations r
LEFT JOIN library_seat_spaces sp ON sp.app_id=r.app_id AND sp.space_id=r.space_id
LEFT JOIN library_seat_seats st ON st.app_id=r.app_id AND st.space_id=r.space_id AND st.seat_id=r.seat_id
LEFT JOIN library_seat_slots sl ON sl.app_id=r.app_id AND sl.slot_id=r.slot_id
WHERE r.app_id=? AND r.user_id=?
ORDER BY r.starts_at DESC, r.reservation_id
LIMIT ?`, appID, userID, request.Limit)
	if err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	defer rows.Close()
	items := make([]libraryseat.Reservation, 0)
	for rows.Next() {
		item, err := scanReservation(rows, appID)
		if err != nil {
			return nil, libraryseat.SnapshotMetadata{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	return items, metadata, nil
}

func (s *Store) beginLibrarySeatSnapshotRead(ctx context.Context, appID string) (*sql.Tx, libraryseat.SnapshotMetadata, error) {
	if err := libraryseat.RequireApp(appID); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	tx, err := s.beginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, libraryseat.SnapshotMetadata{}, fmt.Errorf("begin library seat snapshot read: %w", err)
	}
	metadata, err := readLibrarySeatMetadata(ctx, tx, appID)
	if err != nil {
		return nil, libraryseat.SnapshotMetadata{}, s.rollbackTx(tx, err, "read library seat snapshot")
	}
	return tx, metadata, nil
}

func (s *Store) beginLibrarySeatSnapshotWrite(ctx context.Context, appID string) (*sql.Tx, libraryseat.SnapshotMetadata, error) {
	if err := libraryseat.RequireApp(appID); err != nil {
		return nil, libraryseat.SnapshotMetadata{}, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return nil, libraryseat.SnapshotMetadata{}, fmt.Errorf("begin library seat snapshot write: %w", err)
	}
	metadata, err := readLibrarySeatMetadata(ctx, tx, appID)
	if err != nil {
		return nil, libraryseat.SnapshotMetadata{}, s.rollbackTx(tx, err, "write library seat snapshot")
	}
	return tx, metadata, nil
}

func readLibrarySeatMetadata(ctx context.Context, tx *sql.Tx, appID string) (libraryseat.SnapshotMetadata, error) {
	var metadata libraryseat.SnapshotMetadata
	var authoritative, complete int
	var importedAt, validUntil string
	err := tx.QueryRowContext(ctx, `
SELECT source.revision,source.source,source.authoritative,source.complete,source.imported_at,source.valid_until
FROM library_seat_current_snapshots current
JOIN library_seat_source_revisions source
  ON source.app_id=current.app_id AND source.revision=current.revision
WHERE current.app_id=?`, appID).Scan(
		&metadata.Revision, &metadata.Source, &authoritative, &complete, &importedAt, &validUntil,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.SnapshotMetadata{}, contracts.ErrDataUnavailable
	}
	if err != nil {
		return libraryseat.SnapshotMetadata{}, fmt.Errorf("read library seat catalog: %w", err)
	}
	metadata.Authoritative = authoritative == 1
	metadata.Complete = complete == 1
	metadata.ImportedAt, err = time.Parse(time.RFC3339Nano, importedAt)
	if err != nil {
		return libraryseat.SnapshotMetadata{}, fmt.Errorf("parse library seat import time: %w", err)
	}
	metadata.ValidUntil, err = time.Parse(time.RFC3339Nano, validUntil)
	if err != nil {
		return libraryseat.SnapshotMetadata{}, fmt.Errorf("parse library seat validity: %w", err)
	}
	return metadata, nil
}

func expireLibrarySeatReservations(ctx context.Context, tx *sql.Tx, appID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
UPDATE library_seat_reservations
SET status='expired', updated_at=?
WHERE app_id=? AND status IN ('pending_confirm','confirmed') AND ends_at<=?`,
		now.Format(time.RFC3339Nano), appID, now.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("expire library seat reservations: %w", err)
	}
	return nil
}

func loadSeatSnapshot(ctx context.Context, tx *sql.Tx, appID string, metadata libraryseat.SnapshotMetadata, request libraryseat.SlotSearchRequest) (libraryseat.SeatSnapshot, error) {
	var space libraryseat.Space
	err := tx.QueryRowContext(ctx, `
SELECT space_id,name,campus,building,floor,source_revision
FROM library_seat_spaces WHERE app_id=? AND space_id=? AND source_revision=?`,
		appID, request.SpaceID, metadata.Revision,
	).Scan(&space.ID, &space.Name, &space.Campus, &space.Building, &space.Floor, &space.SourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.SeatSnapshot{Metadata: metadata, Slots: []libraryseat.Slot{}, Seats: []libraryseat.Seat{}, Occupied: map[string]string{}}, nil
	}
	if err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	slotQuery := `
SELECT slot_id,name,start_minute,end_minute,source_revision
FROM library_seat_slots WHERE app_id=? AND source_revision=?`
	slotArgs := []any{appID, metadata.Revision}
	if request.SlotID != "" {
		slotQuery += ` AND slot_id=?`
		slotArgs = append(slotArgs, request.SlotID)
	}
	slotQuery += ` ORDER BY start_minute, slot_id`
	slotRows, err := tx.QueryContext(ctx, slotQuery, slotArgs...)
	if err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	slots := make([]libraryseat.Slot, 0)
	for slotRows.Next() {
		var slot libraryseat.Slot
		if err := slotRows.Scan(&slot.ID, &slot.Name, &slot.StartMinute, &slot.EndMinute, &slot.SourceRevision); err != nil {
			slotRows.Close()
			return libraryseat.SeatSnapshot{}, err
		}
		slots = append(slots, slot)
	}
	if err := slotRows.Err(); err != nil {
		slotRows.Close()
		return libraryseat.SeatSnapshot{}, err
	}
	if err := slotRows.Close(); err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	seatRows, err := tx.QueryContext(ctx, `
SELECT seat_id,space_id,label,area,source_revision
FROM library_seat_seats WHERE app_id=? AND space_id=? AND source_revision=?
ORDER BY label, seat_id`, appID, request.SpaceID, metadata.Revision)
	if err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	seats := make([]libraryseat.Seat, 0)
	for seatRows.Next() {
		var seat libraryseat.Seat
		if err := seatRows.Scan(&seat.ID, &seat.SpaceID, &seat.Label, &seat.Area, &seat.SourceRevision); err != nil {
			seatRows.Close()
			return libraryseat.SeatSnapshot{}, err
		}
		seats = append(seats, seat)
	}
	if err := seatRows.Err(); err != nil {
		seatRows.Close()
		return libraryseat.SeatSnapshot{}, err
	}
	if err := seatRows.Close(); err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	occRows, err := tx.QueryContext(ctx, `
SELECT seat_id, slot_id FROM library_seat_reservations
WHERE app_id=? AND space_id=? AND slot_date=? AND status IN ('pending_confirm','confirmed')`,
		appID, request.SpaceID, request.Date)
	if err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	occupied := make(map[string]string)
	for occRows.Next() {
		var seatID, slotID string
		if err := occRows.Scan(&seatID, &slotID); err != nil {
			occRows.Close()
			return libraryseat.SeatSnapshot{}, err
		}
		occupied[libraryseat.OccupancyKey(seatID, slotID)] = seatID
	}
	if err := occRows.Err(); err != nil {
		occRows.Close()
		return libraryseat.SeatSnapshot{}, err
	}
	if err := occRows.Close(); err != nil {
		return libraryseat.SeatSnapshot{}, err
	}
	return libraryseat.SeatSnapshot{Metadata: metadata, Space: space, Slots: slots, Seats: seats, Occupied: occupied}, nil
}

func loadCatalogRefs(ctx context.Context, tx *sql.Tx, appID, revision, spaceID, seatID, slotID string) (libraryseat.Space, libraryseat.Seat, libraryseat.Slot, error) {
	var space libraryseat.Space
	err := tx.QueryRowContext(ctx, `
SELECT space_id,name,campus,building,floor,source_revision
FROM library_seat_spaces WHERE app_id=? AND space_id=? AND source_revision=?`,
		appID, spaceID, revision,
	).Scan(&space.ID, &space.Name, &space.Campus, &space.Building, &space.Floor, &space.SourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.Space{}, libraryseat.Seat{}, libraryseat.Slot{}, libraryseat.ErrNotFound
	}
	if err != nil {
		return libraryseat.Space{}, libraryseat.Seat{}, libraryseat.Slot{}, err
	}
	var seat libraryseat.Seat
	err = tx.QueryRowContext(ctx, `
SELECT seat_id,space_id,label,area,source_revision
FROM library_seat_seats WHERE app_id=? AND space_id=? AND seat_id=? AND source_revision=?`,
		appID, spaceID, seatID, revision,
	).Scan(&seat.ID, &seat.SpaceID, &seat.Label, &seat.Area, &seat.SourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.Space{}, libraryseat.Seat{}, libraryseat.Slot{}, libraryseat.ErrNotFound
	}
	if err != nil {
		return libraryseat.Space{}, libraryseat.Seat{}, libraryseat.Slot{}, err
	}
	var slot libraryseat.Slot
	err = tx.QueryRowContext(ctx, `
SELECT slot_id,name,start_minute,end_minute,source_revision
FROM library_seat_slots WHERE app_id=? AND slot_id=? AND source_revision=?`,
		appID, slotID, revision,
	).Scan(&slot.ID, &slot.Name, &slot.StartMinute, &slot.EndMinute, &slot.SourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.Space{}, libraryseat.Seat{}, libraryseat.Slot{}, libraryseat.ErrNotFound
	}
	if err != nil {
		return libraryseat.Space{}, libraryseat.Seat{}, libraryseat.Slot{}, err
	}
	return space, seat, slot, nil
}

type reservationScanner interface {
	Scan(dest ...any) error
}

func loadReservation(ctx context.Context, tx *sql.Tx, appID, reservationID string) (libraryseat.Reservation, error) {
	row := tx.QueryRowContext(ctx, `
SELECT r.reservation_id,r.user_id,r.space_id,COALESCE(sp.name,''),r.seat_id,COALESCE(st.label,''),
       r.slot_id,COALESCE(sl.name,''),r.slot_date,r.starts_at,r.ends_at,r.status,
       r.catalog_revision,r.catalog_source,r.catalog_authoritative,r.create_idempotency_key,
       r.created_at,r.updated_at,r.cancelled_at
FROM library_seat_reservations r
LEFT JOIN library_seat_spaces sp ON sp.app_id=r.app_id AND sp.space_id=r.space_id
LEFT JOIN library_seat_seats st ON st.app_id=r.app_id AND st.space_id=r.space_id AND st.seat_id=r.seat_id
LEFT JOIN library_seat_slots sl ON sl.app_id=r.app_id AND sl.slot_id=r.slot_id
WHERE r.app_id=? AND r.reservation_id=?`, appID, reservationID)
	item, err := scanReservation(row, appID)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.Reservation{}, libraryseat.ErrNotFound
	}
	return item, err
}

func loadReservationByCreateKey(ctx context.Context, tx *sql.Tx, appID, key string) (libraryseat.Reservation, bool, error) {
	row := tx.QueryRowContext(ctx, `
SELECT r.reservation_id,r.user_id,r.space_id,COALESCE(sp.name,''),r.seat_id,COALESCE(st.label,''),
       r.slot_id,COALESCE(sl.name,''),r.slot_date,r.starts_at,r.ends_at,r.status,
       r.catalog_revision,r.catalog_source,r.catalog_authoritative,r.create_idempotency_key,
       r.created_at,r.updated_at,r.cancelled_at
FROM library_seat_reservations r
LEFT JOIN library_seat_spaces sp ON sp.app_id=r.app_id AND sp.space_id=r.space_id
LEFT JOIN library_seat_seats st ON st.app_id=r.app_id AND st.space_id=r.space_id AND st.seat_id=r.seat_id
LEFT JOIN library_seat_slots sl ON sl.app_id=r.app_id AND sl.slot_id=r.slot_id
WHERE r.app_id=? AND r.create_idempotency_key=?`, appID, key)
	item, err := scanReservation(row, appID)
	if errors.Is(err, sql.ErrNoRows) {
		return libraryseat.Reservation{}, false, nil
	}
	if err != nil {
		return libraryseat.Reservation{}, false, err
	}
	return item, true, nil
}

func scanReservation(row reservationScanner, appID string) (libraryseat.Reservation, error) {
	var item libraryseat.Reservation
	var startsAt, endsAt, createdAt, updatedAt string
	var cancelledAt sql.NullString
	var authoritative int
	err := row.Scan(
		&item.ID, &item.UserID, &item.SpaceID, &item.SpaceName, &item.SeatID, &item.SeatLabel,
		&item.SlotID, &item.SlotName, &item.Date, &startsAt, &endsAt, &item.Status,
		&item.CatalogRevision, &item.CatalogSource, &authoritative, &item.CreateIdempotencyKey,
		&createdAt, &updatedAt, &cancelledAt,
	)
	if err != nil {
		return libraryseat.Reservation{}, err
	}
	item.AppID = appID
	item.CatalogAuthoritative = authoritative == 1
	if item.StartsAt, err = time.Parse(time.RFC3339Nano, startsAt); err != nil {
		return libraryseat.Reservation{}, err
	}
	if item.EndsAt, err = time.Parse(time.RFC3339Nano, endsAt); err != nil {
		return libraryseat.Reservation{}, err
	}
	if item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return libraryseat.Reservation{}, err
	}
	if item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return libraryseat.Reservation{}, err
	}
	if cancelledAt.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, cancelledAt.String)
		if err != nil {
			return libraryseat.Reservation{}, err
		}
		item.CancelledAt = &parsed
	}
	return item, nil
}

func isIllegalTransition(err error) bool {
	return err != nil && strings.Contains(err.Error(), "illegal library seat reservation transition")
}
