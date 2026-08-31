package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/id"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/classroom"
)

// 编译期断言：sqlite.Store 必须完整实现 classroom.Store 端口。
var _ classroom.Store = (*Store)(nil)

func init() {
	registerMigration(29, `
CREATE TABLE classroom_source_revisions (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  source TEXT NOT NULL CHECK(length(source) BETWEEN 1 AND 256),
  authoritative INTEGER NOT NULL CHECK(authoritative IN (0, 1)),
  complete INTEGER NOT NULL CHECK(complete IN (0, 1)),
  imported_at TEXT NOT NULL,
  valid_until TEXT NOT NULL,
  PRIMARY KEY (app_id, revision)
);
CREATE TABLE classroom_current_snapshots (
  app_id TEXT PRIMARY KEY CHECK(length(app_id) BETWEEN 1 AND 128),
  revision TEXT NOT NULL CHECK(length(revision) BETWEEN 1 AND 128),
  activated_at TEXT NOT NULL,
  FOREIGN KEY (app_id, revision) REFERENCES classroom_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE TABLE classroom_campuses (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  id TEXT NOT NULL CHECK(length(id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, id),
  FOREIGN KEY (app_id, source_revision) REFERENCES classroom_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE INDEX classroom_campuses_name_idx ON classroom_campuses(app_id, name, id);
CREATE TABLE classroom_buildings (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  id TEXT NOT NULL CHECK(length(id) BETWEEN 1 AND 128),
  campus_id TEXT NOT NULL CHECK(length(campus_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, id),
  FOREIGN KEY (app_id, campus_id) REFERENCES classroom_campuses(app_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (app_id, source_revision) REFERENCES classroom_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE INDEX classroom_buildings_campus_idx ON classroom_buildings(app_id, campus_id, name, id);
CREATE TABLE classroom_rooms (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  id TEXT NOT NULL CHECK(length(id) BETWEEN 1 AND 128),
  campus_id TEXT NOT NULL CHECK(length(campus_id) BETWEEN 1 AND 128),
  building_id TEXT NOT NULL CHECK(length(building_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 256),
  room_type TEXT NOT NULL CHECK(length(room_type) BETWEEN 1 AND 64),
  capacity INTEGER NOT NULL CHECK(capacity >= 0 AND capacity <= 100000),
  floor TEXT NOT NULL DEFAULT '' CHECK(length(floor) <= 32),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, id),
  FOREIGN KEY (app_id, campus_id) REFERENCES classroom_campuses(app_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (app_id, building_id) REFERENCES classroom_buildings(app_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (app_id, source_revision) REFERENCES classroom_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE INDEX classroom_rooms_building_idx ON classroom_rooms(app_id, campus_id, building_id, name, id);
CREATE TABLE classroom_occupancy (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  room_id TEXT NOT NULL CHECK(length(room_id) BETWEEN 1 AND 128),
  academic_date TEXT NOT NULL CHECK(length(academic_date)=10),
  period INTEGER NOT NULL CHECK(period BETWEEN 1 AND 13),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  PRIMARY KEY (app_id, room_id, academic_date, period),
  FOREIGN KEY (app_id, room_id) REFERENCES classroom_rooms(app_id, id) ON DELETE CASCADE,
  FOREIGN KEY (app_id, source_revision) REFERENCES classroom_source_revisions(app_id, revision) ON DELETE RESTRICT
);
CREATE INDEX classroom_occupancy_lookup_idx ON classroom_occupancy(app_id, academic_date, period, room_id);
CREATE TABLE classroom_schedules (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  schedule_id TEXT NOT NULL CHECK(length(schedule_id) BETWEEN 1 AND 128),
  room_id TEXT NOT NULL CHECK(length(room_id) BETWEEN 1 AND 128),
  campus_id TEXT NOT NULL CHECK(length(campus_id) BETWEEN 1 AND 128),
  building_id TEXT NOT NULL CHECK(length(building_id) BETWEEN 1 AND 128),
  room_name TEXT NOT NULL CHECK(length(room_name) BETWEEN 1 AND 256),
  academic_date TEXT NOT NULL CHECK(length(academic_date)=10),
  period INTEGER NOT NULL CHECK(period BETWEEN 1 AND 13),
  title TEXT NOT NULL CHECK(length(title) BETWEEN 1 AND 256),
  status TEXT NOT NULL CHECK(status IN ('scheduled','cancelled')),
  source_revision TEXT NOT NULL CHECK(length(source_revision) BETWEEN 1 AND 128),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cancelled_at TEXT,
  CHECK((status='scheduled' AND cancelled_at IS NULL) OR (status='cancelled' AND cancelled_at IS NOT NULL)),
  PRIMARY KEY (app_id, user_id, schedule_id),
  FOREIGN KEY (user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX classroom_schedules_slot_idx ON classroom_schedules(app_id, user_id, room_id, academic_date, period) WHERE status='scheduled';
CREATE INDEX classroom_schedules_user_idx ON classroom_schedules(app_id, user_id, academic_date, period, schedule_id);
CREATE TRIGGER classroom_campus_revision_insert_guard
BEFORE INSERT ON classroom_campuses
WHEN NOT EXISTS (
  SELECT 1 FROM classroom_source_revisions source
  WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
)
BEGIN
  SELECT RAISE(ABORT, 'invalid classroom campus source revision');
END;
CREATE TRIGGER classroom_building_reference_insert_guard
BEFORE INSERT ON classroom_buildings
WHEN NOT EXISTS (
  SELECT 1 FROM classroom_source_revisions source
  WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
) OR NOT EXISTS (
  SELECT 1 FROM classroom_campuses campus
  WHERE campus.app_id=NEW.app_id AND campus.id=NEW.campus_id AND campus.source_revision=NEW.source_revision
)
BEGIN
  SELECT RAISE(ABORT, 'invalid classroom building references');
END;
CREATE TRIGGER classroom_room_reference_insert_guard
BEFORE INSERT ON classroom_rooms
WHEN NOT EXISTS (
  SELECT 1 FROM classroom_source_revisions source
  WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
) OR NOT EXISTS (
  SELECT 1 FROM classroom_buildings building
  WHERE building.app_id=NEW.app_id AND building.id=NEW.building_id
    AND building.campus_id=NEW.campus_id AND building.source_revision=NEW.source_revision
)
BEGIN
  SELECT RAISE(ABORT, 'invalid classroom room references');
END;
CREATE TRIGGER classroom_occupancy_reference_insert_guard
BEFORE INSERT ON classroom_occupancy
WHEN NOT EXISTS (
  SELECT 1 FROM classroom_source_revisions source
  WHERE source.app_id=NEW.app_id AND source.revision=NEW.source_revision
) OR NOT EXISTS (
  SELECT 1 FROM classroom_rooms room
  WHERE room.app_id=NEW.app_id AND room.id=NEW.room_id AND room.source_revision=NEW.source_revision
)
BEGIN
  SELECT RAISE(ABORT, 'invalid classroom occupancy references');
END;
`)
}

// ClassroomSnapshot 是一次原子替换的教室目录与占用快照。
type ClassroomSnapshot struct {
	AppID         string
	Revision      string
	Source        string
	Authoritative bool
	Complete      bool
	ImportedAt    time.Time
	ValidUntil    time.Time
	Campuses      []classroom.Campus
	Buildings     []classroom.Building
	Rooms         []classroom.Room
	Occupancy     []classroom.Occupancy
}

func (s *Store) ReplaceClassroomSnapshot(ctx context.Context, snapshot ClassroomSnapshot) (resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "replace_classroom_snapshot", started, resultErr) }()
	if err := validateClassroomSnapshot(ctx, snapshot); err != nil {
		return err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin classroom snapshot transaction: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "replace classroom snapshot")
	for _, table := range []string{"classroom_occupancy", "classroom_rooms", "classroom_buildings", "classroom_campuses"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE app_id = ?", snapshot.AppID); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO classroom_source_revisions(app_id,revision,source,authoritative,complete,imported_at,valid_until)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(app_id,revision) DO UPDATE SET
  source=excluded.source,
  authoritative=excluded.authoritative,
  complete=excluded.complete,
  imported_at=excluded.imported_at,
  valid_until=excluded.valid_until`,
		snapshot.AppID,
		snapshot.Revision,
		snapshot.Source,
		boolInt(snapshot.Authoritative),
		boolInt(snapshot.Complete),
		snapshot.ImportedAt.UTC().Format(time.RFC3339Nano),
		snapshot.ValidUntil.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("write classroom source revision: %w", err)
	}
	for _, campus := range snapshot.Campuses {
		if _, err := tx.ExecContext(ctx, `INSERT INTO classroom_campuses(app_id,id,name,source_revision) VALUES(?,?,?,?)`,
			snapshot.AppID, campus.ID, campus.Name, snapshot.Revision); err != nil {
			return fmt.Errorf("insert classroom campus %q: %w", campus.ID, err)
		}
	}
	for _, building := range snapshot.Buildings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO classroom_buildings(app_id,id,campus_id,name,source_revision) VALUES(?,?,?,?,?)`,
			snapshot.AppID, building.ID, building.CampusID, building.Name, snapshot.Revision); err != nil {
			return fmt.Errorf("insert classroom building %q: %w", building.ID, err)
		}
	}
	for _, room := range snapshot.Rooms {
		if _, err := tx.ExecContext(ctx, `INSERT INTO classroom_rooms(app_id,id,campus_id,building_id,name,room_type,capacity,floor,source_revision) VALUES(?,?,?,?,?,?,?,?,?)`,
			snapshot.AppID, room.ID, room.CampusID, room.BuildingID, room.Name, room.Type, room.Capacity, room.Floor, snapshot.Revision); err != nil {
			return fmt.Errorf("insert classroom room %q: %w", room.ID, err)
		}
	}
	for _, occupancy := range snapshot.Occupancy {
		if _, err := tx.ExecContext(ctx, `INSERT INTO classroom_occupancy(app_id,room_id,academic_date,period,source_revision) VALUES(?,?,?,?,?)`,
			snapshot.AppID, occupancy.RoomID, occupancy.AcademicDate, occupancy.Period, snapshot.Revision); err != nil {
			return fmt.Errorf("insert classroom occupancy %q %s %d: %w", occupancy.RoomID, occupancy.AcademicDate, occupancy.Period, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO classroom_current_snapshots(app_id,revision,activated_at) VALUES(?,?,?)
ON CONFLICT(app_id) DO UPDATE SET revision=excluded.revision,activated_at=excluded.activated_at`,
		snapshot.AppID, snapshot.Revision, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("activate classroom source revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit classroom snapshot: %w", err)
	}
	observe.Info(ctx, "教室数据快照已经原子替换",
		observe.Component("storage"),
		observe.StringAttr("app_id", snapshot.AppID),
		observe.StringAttr("source_revision", snapshot.Revision),
		observe.StringAttr("source", snapshot.Source),
		observe.BoolAttr("authoritative", snapshot.Authoritative),
		observe.IntAttr("campus_count", len(snapshot.Campuses)),
		observe.IntAttr("building_count", len(snapshot.Buildings)),
		observe.IntAttr("room_count", len(snapshot.Rooms)),
		observe.IntAttr("occupancy_count", len(snapshot.Occupancy)),
		observe.Duration(started),
	)
	return nil
}

func (s *Store) SearchRooms(ctx context.Context, appID string, request classroom.SearchRequest) (_ classroom.RoomSnapshot, resultErr error) {
	metricStarted := time.Now()
	defer func() { observeStorageOperation(ctx, "search_classroom_rooms", metricStarted, resultErr) }()
	tx, metadata, err := s.beginClassroomSnapshotRead(ctx, appID)
	if err != nil {
		return classroom.RoomSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "search classroom rooms")
	rows, err := tx.QueryContext(ctx, `
SELECT room.id,room.campus_id,campus.name,room.building_id,building.name,room.name,room.room_type,room.capacity,room.floor,room.source_revision
FROM classroom_rooms room
JOIN classroom_campuses campus
  ON campus.app_id=room.app_id AND campus.id=room.campus_id AND campus.source_revision=room.source_revision
JOIN classroom_buildings building
  ON building.app_id=room.app_id AND building.id=room.building_id AND building.source_revision=room.source_revision
WHERE room.app_id=? AND room.source_revision=? AND room.campus_id=?
  AND (?='' OR room.building_id=?)
  AND NOT EXISTS (
    SELECT 1 FROM classroom_occupancy occupancy
    WHERE occupancy.app_id=room.app_id AND occupancy.room_id=room.id
      AND occupancy.source_revision=room.source_revision
      AND occupancy.academic_date=? AND occupancy.period=?
  )
ORDER BY building.name, room.name, room.id
LIMIT ?`,
		appID, metadata.Revision, request.CampusID, request.BuildingID, request.BuildingID,
		request.Date, request.Period, request.Limit,
	)
	if err != nil {
		return classroom.RoomSnapshot{}, fmt.Errorf("query empty classrooms: %w", err)
	}
	rooms, err := scanClassroomRooms(rows)
	if err != nil {
		return classroom.RoomSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return classroom.RoomSnapshot{}, fmt.Errorf("commit classroom room snapshot read: %w", err)
	}
	return classroom.RoomSnapshot{Metadata: metadata, Rooms: rooms}, nil
}

func (s *Store) ListCampuses(ctx context.Context, appID string) (_ classroom.CampusSnapshot, resultErr error) {
	metricStarted := time.Now()
	defer func() { observeStorageOperation(ctx, "list_classroom_campuses", metricStarted, resultErr) }()
	tx, metadata, err := s.beginClassroomSnapshotRead(ctx, appID)
	if err != nil {
		return classroom.CampusSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "list classroom campuses")
	rows, err := tx.QueryContext(ctx, `
SELECT id,name,source_revision FROM classroom_campuses
WHERE app_id=? AND source_revision=? ORDER BY name,id`, appID, metadata.Revision)
	if err != nil {
		return classroom.CampusSnapshot{}, fmt.Errorf("query classroom campuses: %w", err)
	}
	campuses := []classroom.Campus{}
	for rows.Next() {
		var campus classroom.Campus
		if err := rows.Scan(&campus.ID, &campus.Name, &campus.SourceRevision); err != nil {
			return classroom.CampusSnapshot{}, closeSQLRows(rows, fmt.Errorf("scan classroom campus: %w", err))
		}
		campuses = append(campuses, campus)
	}
	if err := rows.Err(); err != nil {
		return classroom.CampusSnapshot{}, closeSQLRows(rows, err)
	}
	if err := rows.Close(); err != nil {
		return classroom.CampusSnapshot{}, fmt.Errorf("close classroom campus rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return classroom.CampusSnapshot{}, fmt.Errorf("commit classroom campus snapshot read: %w", err)
	}
	return classroom.CampusSnapshot{Metadata: metadata, Campuses: campuses}, nil
}

func (s *Store) ListBuildings(ctx context.Context, appID string, request classroom.BuildingListRequest) (_ classroom.BuildingSnapshot, resultErr error) {
	metricStarted := time.Now()
	defer func() { observeStorageOperation(ctx, "list_classroom_buildings", metricStarted, resultErr) }()
	tx, metadata, err := s.beginClassroomSnapshotRead(ctx, appID)
	if err != nil {
		return classroom.BuildingSnapshot{}, err
	}
	defer s.finishTx(tx, &resultErr, "list classroom buildings")
	rows, err := tx.QueryContext(ctx, `
SELECT id,campus_id,name,source_revision FROM classroom_buildings
WHERE app_id=? AND source_revision=? AND campus_id=? ORDER BY name,id`,
		appID, metadata.Revision, request.CampusID)
	if err != nil {
		return classroom.BuildingSnapshot{}, fmt.Errorf("query classroom buildings: %w", err)
	}
	buildings := []classroom.Building{}
	for rows.Next() {
		var building classroom.Building
		if err := rows.Scan(&building.ID, &building.CampusID, &building.Name, &building.SourceRevision); err != nil {
			return classroom.BuildingSnapshot{}, closeSQLRows(rows, fmt.Errorf("scan classroom building: %w", err))
		}
		buildings = append(buildings, building)
	}
	if err := rows.Err(); err != nil {
		return classroom.BuildingSnapshot{}, closeSQLRows(rows, err)
	}
	if err := rows.Close(); err != nil {
		return classroom.BuildingSnapshot{}, fmt.Errorf("close classroom building rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return classroom.BuildingSnapshot{}, fmt.Errorf("commit classroom building snapshot read: %w", err)
	}
	return classroom.BuildingSnapshot{Metadata: metadata, Buildings: buildings}, nil
}

func (s *Store) GetRoom(ctx context.Context, appID, roomID string) (_ classroom.Room, _ classroom.SnapshotMetadata, resultErr error) {
	metricStarted := time.Now()
	defer func() { observeStorageOperation(ctx, "get_classroom_room", metricStarted, resultErr) }()
	if !validClassroomStableID(roomID) {
		return classroom.Room{}, classroom.SnapshotMetadata{}, classroom.ErrNotFound
	}
	tx, metadata, err := s.beginClassroomSnapshotRead(ctx, appID)
	if err != nil {
		return classroom.Room{}, classroom.SnapshotMetadata{}, err
	}
	defer s.finishTx(tx, &resultErr, "get classroom room")
	room, err := scanClassroomRoom(tx.QueryRowContext(ctx, `
SELECT room.id,room.campus_id,campus.name,room.building_id,building.name,room.name,room.room_type,room.capacity,room.floor,room.source_revision
FROM classroom_rooms room
JOIN classroom_campuses campus
  ON campus.app_id=room.app_id AND campus.id=room.campus_id AND campus.source_revision=room.source_revision
JOIN classroom_buildings building
  ON building.app_id=room.app_id AND building.id=room.building_id AND building.source_revision=room.source_revision
WHERE room.app_id=? AND room.source_revision=? AND room.id=?`, appID, metadata.Revision, roomID))
	if errors.Is(err, sql.ErrNoRows) {
		return classroom.Room{}, classroom.SnapshotMetadata{}, classroom.ErrNotFound
	}
	if err != nil {
		return classroom.Room{}, classroom.SnapshotMetadata{}, err
	}
	if err := tx.Commit(); err != nil {
		return classroom.Room{}, classroom.SnapshotMetadata{}, fmt.Errorf("commit classroom room read: %w", err)
	}
	return room, metadata, nil
}

func (s *Store) CreateSchedule(ctx context.Context, item classroom.ScheduleItem) (result classroom.ScheduleItem, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "create_classroom_schedule", started, resultErr) }()
	if err := validateScheduleWrite(item); err != nil {
		return classroom.ScheduleItem{}, err
	}
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("begin classroom schedule create: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "create classroom schedule")
	byID, foundID, err := loadClassroomScheduleByID(ctx, tx, item.AppID, item.UserID, item.ID)
	if err != nil {
		return classroom.ScheduleItem{}, err
	}
	if foundID {
		if byID.Status != classroom.ScheduleStatusScheduled {
			return classroom.ScheduleItem{}, classroom.ErrIllegalState
		}
		if byID.RoomID != item.RoomID || byID.AcademicDate != item.AcademicDate || byID.Period != item.Period {
			return classroom.ScheduleItem{}, classroom.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return classroom.ScheduleItem{}, fmt.Errorf("commit classroom schedule replay: %w", err)
		}
		return byID, nil
	}
	existing, found, err := loadClassroomScheduleBySlot(ctx, tx, item.AppID, item.UserID, item.RoomID, item.AcademicDate, item.Period)
	if err != nil {
		return classroom.ScheduleItem{}, err
	}
	if found {
		if item.ID != existing.ID {
			return classroom.ScheduleItem{}, classroom.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return classroom.ScheduleItem{}, fmt.Errorf("commit classroom schedule replay: %w", err)
		}
		return existing, nil
	}
	createdAt := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO classroom_schedules(
  app_id,user_id,schedule_id,room_id,campus_id,building_id,room_name,academic_date,period,title,status,source_revision,created_at,updated_at,cancelled_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)`,
		item.AppID, item.UserID, item.ID, item.RoomID, item.CampusID, item.BuildingID, item.RoomName,
		item.AcademicDate, item.Period, item.Title, classroom.ScheduleStatusScheduled, item.SourceRevision, createdAt, createdAt,
	); err != nil {
		if isUniqueConstraint(err) {
			return classroom.ScheduleItem{}, classroom.ErrConflict
		}
		if isForeignKeyConstraint(err) {
			return classroom.ScheduleItem{}, classroom.ErrInvalidRequest
		}
		return classroom.ScheduleItem{}, fmt.Errorf("insert classroom schedule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("commit classroom schedule create: %w", err)
	}
	item.Status = classroom.ScheduleStatusScheduled
	item.CreatedAt = now
	item.UpdatedAt = now
	item.CancelledAt = nil
	return item, nil
}

func (s *Store) ListSchedules(ctx context.Context, appID, userID string, filter classroom.ScheduleListRequest) (result []classroom.ScheduleItem, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "list_classroom_schedules", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return nil, classroom.ErrInvalidRequest
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return nil, classroom.ErrUserRequired
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT app_id,user_id,schedule_id,room_id,campus_id,building_id,room_name,academic_date,period,title,status,source_revision,created_at,updated_at,cancelled_at
FROM classroom_schedules
WHERE app_id=? AND user_id=? AND (?='' OR status=?) AND (?='' OR academic_date=?)
ORDER BY academic_date, period, schedule_id`,
		appID, userID, filter.Status, filter.Status, filter.Date, filter.Date,
	)
	if err != nil {
		return nil, fmt.Errorf("query classroom schedules: %w", err)
	}
	items := []classroom.ScheduleItem{}
	for rows.Next() {
		item, err := scanClassroomSchedule(rows)
		if err != nil {
			return nil, closeSQLRows(rows, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, closeSQLRows(rows, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close classroom schedule rows: %w", err)
	}
	return items, nil
}

func (s *Store) CancelSchedule(ctx context.Context, appID, userID, scheduleID string, now time.Time) (result classroom.ScheduleItem, resultErr error) {
	started := time.Now()
	defer func() { observeStorageOperation(ctx, "cancel_classroom_schedule", started, resultErr) }()
	if err := identity.ValidateAppID(appID); err != nil {
		return classroom.ScheduleItem{}, classroom.ErrInvalidRequest
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return classroom.ScheduleItem{}, classroom.ErrUserRequired
	}
	if !validClassroomStableID(scheduleID) {
		return classroom.ScheduleItem{}, classroom.ErrInvalidRequest
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("begin classroom schedule cancel: %w", err)
	}
	defer s.finishTx(tx, &resultErr, "cancel classroom schedule")
	existing, err := scanClassroomSchedule(tx.QueryRowContext(ctx, `
SELECT app_id,user_id,schedule_id,room_id,campus_id,building_id,room_name,academic_date,period,title,status,source_revision,created_at,updated_at,cancelled_at
FROM classroom_schedules WHERE app_id=? AND user_id=? AND schedule_id=?`, appID, userID, scheduleID))
	if errors.Is(err, sql.ErrNoRows) {
		return classroom.ScheduleItem{}, classroom.ErrNotFound
	}
	if err != nil {
		return classroom.ScheduleItem{}, err
	}
	if existing.Status == classroom.ScheduleStatusCancelled {
		if err := tx.Commit(); err != nil {
			return classroom.ScheduleItem{}, fmt.Errorf("commit classroom schedule cancel replay: %w", err)
		}
		return existing, nil
	}
	if existing.Status != classroom.ScheduleStatusScheduled {
		return classroom.ScheduleItem{}, classroom.ErrIllegalState
	}
	cancelledAt := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
UPDATE classroom_schedules SET status='cancelled',cancelled_at=?,updated_at=?
WHERE app_id=? AND user_id=? AND schedule_id=? AND status='scheduled'`,
		cancelledAt, cancelledAt, appID, userID, scheduleID,
	); err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("cancel classroom schedule: %w", err)
	}
	existing.Status = classroom.ScheduleStatusCancelled
	existing.CancelledAt = &now
	existing.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("commit classroom schedule cancel: %w", err)
	}
	return existing, nil
}

func (s *Store) beginClassroomSnapshotRead(ctx context.Context, appID string) (*sql.Tx, classroom.SnapshotMetadata, error) {
	if err := identity.ValidateAppID(appID); err != nil {
		return nil, classroom.SnapshotMetadata{}, contracts.ErrDataUnavailable
	}
	tx, err := s.beginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, classroom.SnapshotMetadata{}, fmt.Errorf("begin classroom snapshot read: %w", err)
	}
	var metadata classroom.SnapshotMetadata
	var authoritative int
	var complete int
	var importedAt string
	var validUntil string
	err = tx.QueryRowContext(ctx, `
SELECT source.revision,source.source,source.authoritative,source.complete,source.imported_at,source.valid_until
FROM classroom_current_snapshots current
JOIN classroom_source_revisions source
  ON source.app_id=current.app_id AND source.revision=current.revision
WHERE current.app_id=?`, appID).Scan(
		&metadata.Revision,
		&metadata.Source,
		&authoritative,
		&complete,
		&importedAt,
		&validUntil,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, classroom.SnapshotMetadata{}, s.rollbackTx(tx, contracts.ErrDataUnavailable, "read classroom snapshot")
	}
	if err != nil {
		return nil, classroom.SnapshotMetadata{}, s.rollbackTx(tx, fmt.Errorf("read current classroom snapshot: %w", err), "read classroom snapshot")
	}
	metadata.Authoritative = authoritative == 1
	metadata.Complete = complete == 1
	metadata.ImportedAt, err = time.Parse(time.RFC3339Nano, importedAt)
	if err != nil {
		return nil, classroom.SnapshotMetadata{}, s.rollbackTx(tx, fmt.Errorf("parse classroom snapshot import time: %w", err), "read classroom snapshot")
	}
	metadata.ValidUntil, err = time.Parse(time.RFC3339Nano, validUntil)
	if err != nil {
		return nil, classroom.SnapshotMetadata{}, s.rollbackTx(tx, fmt.Errorf("parse classroom snapshot validity: %w", err), "read classroom snapshot")
	}
	return tx, metadata, nil
}

func validateClassroomSnapshot(ctx context.Context, snapshot ClassroomSnapshot) error {
	if err := identity.ValidateAppID(snapshot.AppID); err != nil ||
		!validClassroomStableID(snapshot.Revision) ||
		!validClassroomText(snapshot.Source, 256) || !snapshot.Complete || snapshot.ImportedAt.IsZero() ||
		snapshot.ValidUntil.IsZero() || !snapshot.ValidUntil.After(snapshot.ImportedAt) ||
		len(snapshot.Campuses) == 0 || len(snapshot.Campuses) > 64 ||
		len(snapshot.Buildings) == 0 || len(snapshot.Buildings) > 1024 ||
		len(snapshot.Rooms) == 0 || len(snapshot.Rooms) > 100_000 ||
		len(snapshot.Occupancy) > 1_000_000 {
		return fmt.Errorf("invalid classroom snapshot metadata")
	}
	campusIDs := make(map[string]struct{}, len(snapshot.Campuses))
	for _, campus := range snapshot.Campuses {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validClassroomStableID(campus.ID) || !validClassroomText(campus.Name, 256) ||
			(campus.SourceRevision != "" && campus.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid classroom campus %q", campus.ID)
		}
		if _, duplicate := campusIDs[campus.ID]; duplicate {
			return fmt.Errorf("duplicate classroom campus %q", campus.ID)
		}
		campusIDs[campus.ID] = struct{}{}
	}
	buildingIDs := make(map[string]classroom.Building, len(snapshot.Buildings))
	for _, building := range snapshot.Buildings {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validClassroomStableID(building.ID) || !validClassroomText(building.Name, 256) ||
			(building.SourceRevision != "" && building.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid classroom building %q", building.ID)
		}
		if _, exists := campusIDs[building.CampusID]; !exists {
			return fmt.Errorf("classroom building %q references unknown campus", building.ID)
		}
		if _, duplicate := buildingIDs[building.ID]; duplicate {
			return fmt.Errorf("duplicate classroom building %q", building.ID)
		}
		buildingIDs[building.ID] = building
	}
	roomIDs := make(map[string]struct{}, len(snapshot.Rooms))
	for _, room := range snapshot.Rooms {
		if err := ctx.Err(); err != nil {
			return err
		}
		building, exists := buildingIDs[room.BuildingID]
		if !validClassroomStableID(room.ID) || !validClassroomText(room.Name, 256) ||
			!validClassroomText(room.Type, 64) || room.Capacity < 0 || room.Capacity > 100000 ||
			len(room.Floor) > 32 || !utf8.ValidString(room.Floor) ||
			(room.SourceRevision != "" && room.SourceRevision != snapshot.Revision) || !exists ||
			building.CampusID != room.CampusID {
			return fmt.Errorf("invalid classroom room %q", room.ID)
		}
		if _, duplicate := roomIDs[room.ID]; duplicate {
			return fmt.Errorf("duplicate classroom room %q", room.ID)
		}
		roomIDs[room.ID] = struct{}{}
	}
	occupancyKeys := make(map[string]struct{}, len(snapshot.Occupancy))
	for _, occupancy := range snapshot.Occupancy {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := roomIDs[occupancy.RoomID]; !exists {
			return fmt.Errorf("classroom occupancy references unknown room %q", occupancy.RoomID)
		}
		if _, err := classroom.ParseAcademicDate(occupancy.AcademicDate); err != nil ||
			occupancy.Period < classroom.MinPeriod || occupancy.Period > classroom.MaxPeriod ||
			(occupancy.SourceRevision != "" && occupancy.SourceRevision != snapshot.Revision) {
			return fmt.Errorf("invalid classroom occupancy %q", occupancy.RoomID)
		}
		key := occupancy.RoomID + "\x1f" + occupancy.AcademicDate + "\x1f" + fmt.Sprintf("%d", occupancy.Period)
		if _, duplicate := occupancyKeys[key]; duplicate {
			return fmt.Errorf("duplicate classroom occupancy %q", occupancy.RoomID)
		}
		occupancyKeys[key] = struct{}{}
	}
	return nil
}

func validateScheduleWrite(item classroom.ScheduleItem) error {
	if err := identity.ValidateAppID(item.AppID); err != nil {
		return classroom.ErrInvalidRequest
	}
	if err := identity.ValidateUserID(item.UserID); err != nil {
		return classroom.ErrUserRequired
	}
	if !validClassroomStableID(item.ID) || !validClassroomStableID(item.RoomID) ||
		!validClassroomStableID(item.CampusID) || !validClassroomStableID(item.BuildingID) ||
		!validClassroomText(item.RoomName, 256) || !validClassroomText(item.Title, 256) ||
		!validClassroomStableID(item.SourceRevision) {
		return classroom.ErrInvalidRequest
	}
	if _, err := classroom.ParseAcademicDate(item.AcademicDate); err != nil {
		return classroom.ErrInvalidDate
	}
	if item.Period < classroom.MinPeriod || item.Period > classroom.MaxPeriod {
		return classroom.ErrInvalidPeriod
	}
	return nil
}

func loadClassroomScheduleByID(ctx context.Context, tx *sql.Tx, appID, userID, scheduleID string) (classroom.ScheduleItem, bool, error) {
	item, err := scanClassroomSchedule(tx.QueryRowContext(ctx, `
SELECT app_id,user_id,schedule_id,room_id,campus_id,building_id,room_name,academic_date,period,title,status,source_revision,created_at,updated_at,cancelled_at
FROM classroom_schedules WHERE app_id=? AND user_id=? AND schedule_id=?`,
		appID, userID, scheduleID))
	if errors.Is(err, sql.ErrNoRows) {
		return classroom.ScheduleItem{}, false, nil
	}
	if err != nil {
		return classroom.ScheduleItem{}, false, err
	}
	return item, true, nil
}

func loadClassroomScheduleBySlot(ctx context.Context, tx *sql.Tx, appID, userID, roomID, academicDate string, period int) (classroom.ScheduleItem, bool, error) {
	item, err := scanClassroomSchedule(tx.QueryRowContext(ctx, `
SELECT app_id,user_id,schedule_id,room_id,campus_id,building_id,room_name,academic_date,period,title,status,source_revision,created_at,updated_at,cancelled_at
FROM classroom_schedules WHERE app_id=? AND user_id=? AND room_id=? AND academic_date=? AND period=? AND status=?`,
		appID, userID, roomID, academicDate, period, classroom.ScheduleStatusScheduled))
	if errors.Is(err, sql.ErrNoRows) {
		return classroom.ScheduleItem{}, false, nil
	}
	if err != nil {
		return classroom.ScheduleItem{}, false, err
	}
	return item, true, nil
}

type classroomScheduleScanner interface {
	Scan(dest ...any) error
}

func scanClassroomSchedule(row classroomScheduleScanner) (classroom.ScheduleItem, error) {
	var item classroom.ScheduleItem
	var createdAt, updatedAt string
	var cancelledAt sql.NullString
	if err := row.Scan(
		&item.AppID, &item.UserID, &item.ID, &item.RoomID, &item.CampusID, &item.BuildingID, &item.RoomName,
		&item.AcademicDate, &item.Period, &item.Title, &item.Status, &item.SourceRevision,
		&createdAt, &updatedAt, &cancelledAt,
	); err != nil {
		return classroom.ScheduleItem{}, err
	}
	var err error
	item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("parse classroom schedule created_at: %w", err)
	}
	item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return classroom.ScheduleItem{}, fmt.Errorf("parse classroom schedule updated_at: %w", err)
	}
	if cancelledAt.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, cancelledAt.String)
		if err != nil {
			return classroom.ScheduleItem{}, fmt.Errorf("parse classroom schedule cancelled_at: %w", err)
		}
		item.CancelledAt = &parsed
	}
	return item, nil
}

func scanClassroomRooms(rows *sql.Rows) ([]classroom.Room, error) {
	rooms := []classroom.Room{}
	for rows.Next() {
		room, err := scanClassroomRoom(rows)
		if err != nil {
			return nil, closeSQLRows(rows, err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, closeSQLRows(rows, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close classroom room rows: %w", err)
	}
	return rooms, nil
}

func closeSQLRows(rows *sql.Rows, primary error) error {
	return errors.Join(primary, rows.Close())
}

func scanClassroomRoom(row classroomScheduleScanner) (classroom.Room, error) {
	var room classroom.Room
	if err := row.Scan(
		&room.ID, &room.CampusID, &room.CampusName, &room.BuildingID, &room.BuildingName,
		&room.Name, &room.Type, &room.Capacity, &room.Floor, &room.SourceRevision,
	); err != nil {
		return classroom.Room{}, err
	}
	return room, nil
}

func validClassroomStableID(value string) bool {
	return value != "" && len(value) <= 128 && utf8.ValidString(value) && id.StableMixed.MatchString(value)
}

func validClassroomText(value string, maximum int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maximum && utf8.ValidString(value)
}
