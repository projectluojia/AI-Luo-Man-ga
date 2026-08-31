package demo

import (
	"context"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/classroom"
)

// LoadClassroomData 载入空闲教室演示快照（非权威）：四校区教学楼、节次占用、
// 显式标记来源与有效期。不得在生产环境当作当前空闲教室事实。
func LoadClassroomData(ctx context.Context, store *sqlite.Store, now time.Time) error {
	location, err := time.LoadLocation(classroom.AcademicTimezone)
	if err != nil {
		return err
	}
	localNow := now.In(location)
	startDay := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	revision := "demo-fixture-" + startDay.Format("20060102")
	campuses := []classroom.Campus{
		{ID: "campus-wenli", Name: "文理学部", SourceRevision: revision},
		{ID: "campus-xinxi", Name: "信息学部", SourceRevision: revision},
		{ID: "campus-gongxue", Name: "工学部", SourceRevision: revision},
		{ID: "campus-yixue", Name: "医学部", SourceRevision: revision},
	}
	buildings := []classroom.Building{
		{ID: "bld-wenli-jiao5", CampusID: "campus-wenli", Name: "教五", SourceRevision: revision},
		{ID: "bld-wenli-yijiao", CampusID: "campus-wenli", Name: "一教", SourceRevision: revision},
		{ID: "bld-xinxi-jisuanji", CampusID: "campus-xinxi", Name: "计算机大楼", SourceRevision: revision},
		{ID: "bld-gongxue-gongjiao", CampusID: "campus-gongxue", Name: "工教", SourceRevision: revision},
		{ID: "bld-yixue-yixue", CampusID: "campus-yixue", Name: "医学楼", SourceRevision: revision},
	}
	rooms := []classroom.Room{
		{ID: "room-wenli-jiao5-101", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-101", Type: "多媒体教室", Capacity: 120, Floor: "3", SourceRevision: revision},
		{ID: "room-wenli-jiao5-102", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-102", Type: "普通教室", Capacity: 80, Floor: "3", SourceRevision: revision},
		{ID: "room-wenli-jiao5-201", CampusID: "campus-wenli", BuildingID: "bld-wenli-jiao5", Name: "教五-201", Type: "机房", Capacity: 60, Floor: "2", SourceRevision: revision},
		{ID: "room-wenli-yijiao-101", CampusID: "campus-wenli", BuildingID: "bld-wenli-yijiao", Name: "一教-101", Type: "多媒体教室", Capacity: 200, Floor: "1", SourceRevision: revision},
		{ID: "room-xinxi-jisuanji-201", CampusID: "campus-xinxi", BuildingID: "bld-xinxi-jisuanji", Name: "计算机大楼-201", Type: "机房", Capacity: 90, Floor: "2", SourceRevision: revision},
		{ID: "room-xinxi-jisuanji-301", CampusID: "campus-xinxi", BuildingID: "bld-xinxi-jisuanji", Name: "计算机大楼-301", Type: "多媒体教室", Capacity: 150, Floor: "3", SourceRevision: revision},
		{ID: "room-gongxue-gongjiao-301", CampusID: "campus-gongxue", BuildingID: "bld-gongxue-gongjiao", Name: "工教-301", Type: "普通教室", Capacity: 100, Floor: "3", SourceRevision: revision},
		{ID: "room-yixue-yixue-401", CampusID: "campus-yixue", BuildingID: "bld-yixue-yixue", Name: "医学楼-401", Type: "多媒体教室", Capacity: 80, Floor: "4", SourceRevision: revision},
	}
	busy := []struct {
		roomID  string
		periods []int
	}{
		{roomID: "room-wenli-jiao5-101", periods: []int{1, 2, 3, 4}},
		{roomID: "room-wenli-jiao5-201", periods: []int{5, 6, 7, 8}},
		{roomID: "room-xinxi-jisuanji-201", periods: []int{3, 4, 5}},
		{roomID: "room-gongxue-gongjiao-301", periods: []int{1, 2}},
		{roomID: "room-yixue-yixue-401", periods: []int{6, 7, 8}},
	}
	occupancy := make([]classroom.Occupancy, 0, len(busy)*8*4)
	for day := 0; day < 8; day++ {
		date := startDay.AddDate(0, 0, day).Format(classroom.AcademicDateLayout)
		for _, item := range busy {
			for _, period := range item.periods {
				occupancy = append(occupancy, classroom.Occupancy{
					RoomID: item.roomID, AcademicDate: date, Period: period, SourceRevision: revision,
				})
			}
		}
	}
	return store.ReplaceClassroomSnapshot(ctx, sqlite.ClassroomSnapshot{
		AppID:         campus.AppID,
		Revision:      revision,
		Source:        classroom.DemoSource,
		Authoritative: false,
		Complete:      true,
		ImportedAt:    now,
		ValidUntil:    startDay.AddDate(0, 0, 8),
		Campuses:      campuses,
		Buildings:     buildings,
		Rooms:         rooms,
		Occupancy:     occupancy,
	})
}
