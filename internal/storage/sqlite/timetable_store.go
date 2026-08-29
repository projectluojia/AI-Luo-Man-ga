package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	timetable "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/timetable"
)

var _ timetable.Store = (*Store)(nil)

func init() {
	registerMigration(26, `
CREATE TABLE timetables (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  timetable_id TEXT NOT NULL CHECK(length(timetable_id) BETWEEN 1 AND 128),
  name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 2048),
  source TEXT NOT NULL CHECK(source IN ('local','wuda','wakeup')),
  active INTEGER NOT NULL CHECK(active IN (0,1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(app_id,user_id,timetable_id),
  FOREIGN KEY(user_id) REFERENCES users(user_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX timetables_active_idx ON timetables(app_id,user_id) WHERE active=1;
CREATE INDEX timetables_user_idx ON timetables(app_id,user_id,updated_at,timetable_id);
CREATE TABLE timetable_courses (
  app_id TEXT NOT NULL CHECK(length(app_id) BETWEEN 1 AND 128),
  user_id TEXT NOT NULL CHECK(length(user_id) BETWEEN 1 AND 128),
  timetable_id TEXT NOT NULL CHECK(length(timetable_id) BETWEEN 1 AND 128),
  course_id TEXT NOT NULL CHECK(length(course_id) BETWEEN 1 AND 128),
  title TEXT NOT NULL CHECK(length(title) BETWEEN 1 AND 1024),
  title_raw TEXT NOT NULL DEFAULT '' CHECK(length(title_raw) <= 2048),
  weekday INTEGER NOT NULL CHECK(weekday BETWEEN 1 AND 7),
  class_from INTEGER NOT NULL CHECK(class_from BETWEEN 1 AND 64),
  class_to INTEGER NOT NULL CHECK(class_to BETWEEN 1 AND 64 AND class_to>=class_from),
  weeks TEXT NOT NULL CHECK(json_valid(weeks) AND json_type(weeks)='array' AND length(weeks)<=4096),
  course_nature TEXT NOT NULL DEFAULT '' CHECK(length(course_nature)<=2048),
  instructor TEXT NOT NULL DEFAULT '' CHECK(length(instructor)<=2048),
  instructor_raw TEXT NOT NULL DEFAULT '' CHECK(length(instructor_raw)<=2048),
  location TEXT NOT NULL DEFAULT '' CHECK(length(location)<=2048),
  week_meta TEXT NOT NULL DEFAULT '' CHECK(length(week_meta)<=2048),
  start_text TEXT NOT NULL DEFAULT '' CHECK(length(start_text)<=512),
  end_text TEXT NOT NULL DEFAULT '' CHECK(length(end_text)<=512),
  external_id TEXT NOT NULL DEFAULT '' CHECK(length(external_id)<=2048),
  PRIMARY KEY(app_id,user_id,timetable_id,course_id),
  FOREIGN KEY(app_id,user_id,timetable_id) REFERENCES timetables(app_id,user_id,timetable_id) ON DELETE CASCADE
);
CREATE INDEX timetable_courses_list_idx ON timetable_courses(app_id,user_id,timetable_id,weekday,class_from,course_id);
`)
}

func (s *Store) CreateTimetable(ctx context.Context, value timetable.Timetable) (result timetable.Timetable, resultErr error) {
	var err error
	value, err = normalizeTimetableStoreInput(value)
	if err != nil {
		return timetable.Timetable{}, err
	}
	if value.ID == "" {
		return timetable.Timetable{}, timetable.ErrInvalid
	}
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return timetable.Timetable{}, err
	}
	defer s.finishTx(tx, &resultErr, "create timetable")
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM timetables WHERE app_id=? AND user_id=?`, value.AppID, value.UserID).Scan(&count); err != nil {
		return timetable.Timetable{}, err
	}
	if count >= timetable.MaxTimetablesPerUser {
		return timetable.Timetable{}, timetable.ErrCapacity
	}
	if value.Active {
		if _, err := tx.ExecContext(ctx, `UPDATE timetables SET active=0,updated_at=? WHERE app_id=? AND user_id=?`, now.Format(time.RFC3339Nano), value.AppID, value.UserID); err != nil {
			return timetable.Timetable{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO timetables(app_id,user_id,timetable_id,name,source,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, value.AppID, value.UserID, value.ID, value.Name, value.Source, value.Active, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		if isUniqueError(err) {
			return timetable.Timetable{}, timetable.ErrConflict
		}
		return timetable.Timetable{}, err
	}
	if err := tx.Commit(); err != nil {
		return timetable.Timetable{}, err
	}
	value.CreatedAt, value.UpdatedAt = now, now
	return value, nil
}

func (s *Store) ListTimetables(ctx context.Context, appID, userID string) (result []timetable.Timetable, resultErr error) {
	if err := validateOwner(appID, userID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT timetable_id,name,source,active,created_at,updated_at FROM timetables WHERE app_id=? AND user_id=? ORDER BY active DESC,updated_at DESC,timetable_id`, appID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanTimetable(rows, appID, userID)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetTimetable(ctx context.Context, appID, userID, timetableID string) (timetable.Timetable, error) {
	if err := validateOwner(appID, userID); err != nil {
		return timetable.Timetable{}, err
	}
	if timetableID == "" {
		return timetable.Timetable{}, timetable.ErrInvalid
	}
	item, err := scanTimetable(s.db.QueryRowContext(ctx, `SELECT timetable_id,name,source,active,created_at,updated_at FROM timetables WHERE app_id=? AND user_id=? AND timetable_id=?`, appID, userID, timetableID), appID, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return timetable.Timetable{}, timetable.ErrNotFound
	}
	return item, err
}

func (s *Store) UpdateTimetable(ctx context.Context, value timetable.Timetable) (result timetable.Timetable, resultErr error) {
	var err error
	value, err = normalizeTimetableStoreInput(value)
	if err != nil {
		return timetable.Timetable{}, err
	}
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return timetable.Timetable{}, err
	}
	defer s.finishTx(tx, &resultErr, "update timetable")
	if value.Active {
		if _, err := tx.ExecContext(ctx, `UPDATE timetables SET active=0,updated_at=? WHERE app_id=? AND user_id=?`, now.Format(time.RFC3339Nano), value.AppID, value.UserID); err != nil {
			return timetable.Timetable{}, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE timetables SET name=?,source=?,active=?,updated_at=? WHERE app_id=? AND user_id=? AND timetable_id=?`, value.Name, value.Source, value.Active, now.Format(time.RFC3339Nano), value.AppID, value.UserID, value.ID)
	if err != nil {
		return timetable.Timetable{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return timetable.Timetable{}, timetable.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return timetable.Timetable{}, err
	}
	return s.GetTimetable(ctx, value.AppID, value.UserID, value.ID)
}

func (s *Store) SetTimetableActive(ctx context.Context, appID, userID, timetableID string) (result timetable.Timetable, resultErr error) {
	if err := validateOwner(appID, userID); err != nil {
		return timetable.Timetable{}, err
	}
	if timetableID == "" {
		return timetable.Timetable{}, timetable.ErrInvalid
	}
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return timetable.Timetable{}, err
	}
	defer s.finishTx(tx, &resultErr, "activate timetable")
	res, err := tx.ExecContext(ctx, `UPDATE timetables SET active=0,updated_at=? WHERE app_id=? AND user_id=? AND active=1`, now.Format(time.RFC3339Nano), appID, userID)
	if err != nil {
		return timetable.Timetable{}, err
	}
	_ = res
	res, err = tx.ExecContext(ctx, `UPDATE timetables SET active=1,updated_at=? WHERE app_id=? AND user_id=? AND timetable_id=?`, now.Format(time.RFC3339Nano), appID, userID, timetableID)
	if err != nil {
		return timetable.Timetable{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return timetable.Timetable{}, timetable.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return timetable.Timetable{}, err
	}
	return s.GetTimetable(ctx, appID, userID, timetableID)
}

func (s *Store) DeleteTimetable(ctx context.Context, appID, userID, timetableID string) (resultErr error) {
	if err := validateOwner(appID, userID); err != nil {
		return err
	}
	if timetableID == "" {
		return timetable.ErrInvalid
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer s.finishTx(tx, &resultErr, "delete timetable")
	res, err := tx.ExecContext(ctx, `DELETE FROM timetables WHERE app_id=? AND user_id=? AND timetable_id=?`, appID, userID, timetableID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return timetable.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) ListCourses(ctx context.Context, appID, userID, timetableID string) (result []timetable.Course, resultErr error) {
	if err := validateOwner(appID, userID); err != nil {
		return nil, err
	}
	if timetableID == "" {
		return nil, timetable.ErrInvalid
	}
	if _, err := s.GetTimetable(ctx, appID, userID, timetableID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT course_id,title,title_raw,weekday,class_from,class_to,weeks,course_nature,instructor,instructor_raw,location,week_meta,start_text,end_text,external_id FROM timetable_courses WHERE app_id=? AND user_id=? AND timetable_id=? ORDER BY weekday,class_from,class_to,course_id`, appID, userID, timetableID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanCourse(rows, appID, userID, timetableID)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetCourse(ctx context.Context, appID, userID, timetableID, courseID string) (timetable.Course, error) {
	if err := validateOwner(appID, userID); err != nil {
		return timetable.Course{}, err
	}
	if timetableID == "" || courseID == "" {
		return timetable.Course{}, timetable.ErrInvalid
	}
	item, err := scanCourse(s.db.QueryRowContext(ctx, `SELECT course_id,title,title_raw,weekday,class_from,class_to,weeks,course_nature,instructor,instructor_raw,location,week_meta,start_text,end_text,external_id FROM timetable_courses WHERE app_id=? AND user_id=? AND timetable_id=? AND course_id=?`, appID, userID, timetableID, courseID), appID, userID, timetableID)
	if errors.Is(err, sql.ErrNoRows) {
		return timetable.Course{}, timetable.ErrNotFound
	}
	return item, err
}

func (s *Store) CreateCourse(ctx context.Context, value timetable.Course) (result timetable.Course, resultErr error) {
	var err error
	value, err = normalizeCourseStoreInput(value)
	if err != nil {
		return timetable.Course{}, err
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return timetable.Course{}, err
	}
	defer s.finishTx(tx, &resultErr, "create timetable course")
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM timetables WHERE app_id=? AND user_id=? AND timetable_id=?`, value.AppID, value.UserID, value.TimetableID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return timetable.Course{}, timetable.ErrNotFound
	} else if err != nil {
		return timetable.Course{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM timetable_courses WHERE app_id=? AND user_id=? AND timetable_id=?`, value.AppID, value.UserID, value.TimetableID).Scan(&count); err != nil {
		return timetable.Course{}, err
	}
	if count >= timetable.MaxCoursesPerTable {
		return timetable.Course{}, timetable.ErrCapacity
	}
	if _, err := tx.ExecContext(ctx, courseInsertSQL, argsCourse(value)...); err != nil {
		if isUniqueError(err) {
			return timetable.Course{}, timetable.ErrConflict
		}
		return timetable.Course{}, err
	}
	if err := tx.Commit(); err != nil {
		return timetable.Course{}, err
	}
	return value, nil
}

func (s *Store) UpdateCourse(ctx context.Context, value timetable.Course) (result timetable.Course, resultErr error) {
	var err error
	value, err = normalizeCourseStoreInput(value)
	if err != nil {
		return timetable.Course{}, err
	}
	weeks, _ := json.Marshal(value.Weeks)
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return timetable.Course{}, err
	}
	defer s.finishTx(tx, &resultErr, "update timetable course")
	res, err := tx.ExecContext(ctx, `UPDATE timetable_courses SET title=?,title_raw=?,weekday=?,class_from=?,class_to=?,weeks=?,course_nature=?,instructor=?,instructor_raw=?,location=?,week_meta=?,start_text=?,end_text=?,external_id=? WHERE app_id=? AND user_id=? AND timetable_id=? AND course_id=?`, value.Title, value.TitleRaw, value.Weekday, value.ClassFrom, value.ClassTo, string(weeks), value.CourseNature, value.Instructor, value.InstructorRaw, value.Location, value.WeekMeta, value.StartText, value.EndText, value.ExternalID, value.AppID, value.UserID, value.TimetableID, value.ID)
	if err != nil {
		return timetable.Course{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return timetable.Course{}, timetable.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return timetable.Course{}, err
	}
	return value, nil
}

func (s *Store) DeleteCourse(ctx context.Context, appID, userID, timetableID, courseID string) (resultErr error) {
	if err := validateOwner(appID, userID); err != nil {
		return err
	}
	if timetableID == "" || courseID == "" {
		return timetable.ErrInvalid
	}
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer s.finishTx(tx, &resultErr, "delete timetable course")
	res, err := tx.ExecContext(ctx, `DELETE FROM timetable_courses WHERE app_id=? AND user_id=? AND timetable_id=? AND course_id=?`, appID, userID, timetableID, courseID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return timetable.ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) ImportTimetable(ctx context.Context, value timetable.Timetable, courses []timetable.Course) (result timetable.Timetable, resultCourses []timetable.Course, resultErr error) {
	var err error
	value, err = normalizeTimetableStoreInput(value)
	if err != nil {
		return timetable.Timetable{}, nil, err
	}
	if len(courses) == 0 {
		return timetable.Timetable{}, nil, timetable.ErrNoCourses
	}
	if len(courses) > timetable.MaxCoursesPerTable {
		return timetable.Timetable{}, nil, timetable.ErrCapacity
	}
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, nil)
	if err != nil {
		return timetable.Timetable{}, nil, err
	}
	defer s.finishTx(tx, &resultErr, "import timetable")
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM timetables WHERE app_id=? AND user_id=?`, value.AppID, value.UserID).Scan(&count); err != nil {
		return timetable.Timetable{}, nil, err
	}
	if count >= timetable.MaxTimetablesPerUser {
		return timetable.Timetable{}, nil, timetable.ErrCapacity
	}
	if value.Active {
		if _, err := tx.ExecContext(ctx, `UPDATE timetables SET active=0,updated_at=? WHERE app_id=? AND user_id=?`, now.Format(time.RFC3339Nano), value.AppID, value.UserID); err != nil {
			return timetable.Timetable{}, nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO timetables(app_id,user_id,timetable_id,name,source,active,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, value.AppID, value.UserID, value.ID, value.Name, value.Source, value.Active, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		if isUniqueError(err) {
			return timetable.Timetable{}, nil, timetable.ErrConflict
		}
		return timetable.Timetable{}, nil, err
	}
	normalizedCourses := make([]timetable.Course, 0, len(courses))
	for _, course := range courses {
		course, err = normalizeCourseStoreInput(course)
		if err != nil {
			return timetable.Timetable{}, nil, err
		}
		if course.AppID != value.AppID || course.UserID != value.UserID || course.TimetableID != value.ID {
			return timetable.Timetable{}, nil, timetable.ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, courseInsertSQL, argsCourse(course)...); err != nil {
			if isUniqueError(err) {
				return timetable.Timetable{}, nil, timetable.ErrConflict
			}
			return timetable.Timetable{}, nil, err
		}
		normalizedCourses = append(normalizedCourses, course)
	}
	if err := tx.Commit(); err != nil {
		return timetable.Timetable{}, nil, err
	}
	value.CreatedAt, value.UpdatedAt = now, now
	return value, normalizedCourses, nil
}

const courseInsertSQL = `INSERT INTO timetable_courses(app_id,user_id,timetable_id,course_id,title,title_raw,weekday,class_from,class_to,weeks,course_nature,instructor,instructor_raw,location,week_meta,start_text,end_text,external_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

func argsCourse(value timetable.Course) []any {
	weeks, _ := json.Marshal(value.Weeks)
	return []any{value.AppID, value.UserID, value.TimetableID, value.ID, value.Title, value.TitleRaw, value.Weekday, value.ClassFrom, value.ClassTo, string(weeks), value.CourseNature, value.Instructor, value.InstructorRaw, value.Location, value.WeekMeta, value.StartText, value.EndText, value.ExternalID}
}

func normalizeTimetableStoreInput(value timetable.Timetable) (timetable.Timetable, error) {
	normalized, err := timetable.NormalizeTimetable(value)
	if err != nil {
		return timetable.Timetable{}, err
	}
	return normalized, nil
}
func normalizeCourseStoreInput(value timetable.Course) (timetable.Course, error) {
	normalized, err := timetable.NormalizeCourse(value)
	if err != nil {
		return timetable.Course{}, err
	}
	return normalized, nil
}
func validateOwner(appID, userID string) error {
	if err := identity.ValidateAppID(appID); err != nil {
		return timetable.ErrInvalid
	}
	if err := identity.ValidateUserID(userID); err != nil {
		return timetable.ErrInvalid
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanTimetable(row scanner, appID, userID string) (timetable.Timetable, error) {
	var item timetable.Timetable
	var active bool
	var created, updated string
	if err := row.Scan(&item.ID, &item.Name, &item.Source, &active, &created, &updated); err != nil {
		return timetable.Timetable{}, err
	}
	item.AppID, item.UserID, item.Active = appID, userID, active
	var err error
	item.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return timetable.Timetable{}, err
	}
	item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return item, err
}
func scanCourse(row scanner, appID, userID, timetableID string) (timetable.Course, error) {
	var item timetable.Course
	var weeks string
	if err := row.Scan(&item.ID, &item.Title, &item.TitleRaw, &item.Weekday, &item.ClassFrom, &item.ClassTo, &weeks, &item.CourseNature, &item.Instructor, &item.InstructorRaw, &item.Location, &item.WeekMeta, &item.StartText, &item.EndText, &item.ExternalID); err != nil {
		return timetable.Course{}, err
	}
	if len(weeks) > 4096 || json.Unmarshal([]byte(weeks), &item.Weeks) != nil {
		return timetable.Course{}, timetable.ErrInvalid
	}
	item.AppID, item.UserID, item.TimetableID = appID, userID, timetableID
	if _, err := timetable.NormalizeCourse(item); err != nil {
		return timetable.Course{}, err
	}
	return item, nil
}

// isUniqueError 判定 SQLITE_CONSTRAINT_UNIQUE（扩展码 2067）与
// SQLITE_CONSTRAINT_PRIMARYKEY（扩展码 1555）两类约束冲突，其余错误一律不
// 视为冲突，避免错误文本匹配误伤无关错误。
func isUniqueError(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	return coded.Code() == 2067 || coded.Code() == 1555
}
