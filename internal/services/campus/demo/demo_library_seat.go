package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	libraryseat "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/libraryseat"
)

// LoadLibrarySeatData 载入图书馆座位演示目录（非权威）：空间/座位/时段模板，
// 显式标记来源 demo-fixture-not-zhihui-luojia，与生产权威目录隔离。
func LoadLibrarySeatData(ctx context.Context, store *sqlite.Store, now time.Time) error {
	location, err := libraryseat.TimeLocation()
	if err != nil {
		return err
	}
	revision := "demo-fixture-" + now.In(location).Format("20060102")
	spaces := []libraryseat.Space{
		{ID: "space-demo-main-3f", Name: "演示·总馆三楼阅览室", Campus: "文理学部", Building: "总图书馆", Floor: "3F", SourceRevision: revision},
		{ID: "space-demo-info-1f", Name: "演示·信息分馆一楼", Campus: "信息学部", Building: "信息分馆", Floor: "1F", SourceRevision: revision},
	}
	seats := make([]libraryseat.Seat, 0, 8)
	for _, space := range spaces {
		for _, label := range []string{"A01", "A02", "B01", "B02"} {
			seats = append(seats, libraryseat.Seat{
				ID: fmt.Sprintf("%s-%s", space.ID, label), SpaceID: space.ID,
				Label: label, Area: label[:1], SourceRevision: revision,
			})
		}
	}
	slots := []libraryseat.Slot{
		{ID: "slot-morning", Name: "上午 08:00-12:00", StartMinute: 8 * 60, EndMinute: 12 * 60, SourceRevision: revision},
		{ID: "slot-afternoon", Name: "下午 12:00-18:00", StartMinute: 12 * 60, EndMinute: 18 * 60, SourceRevision: revision},
		{ID: "slot-evening", Name: "晚上 18:00-22:00", StartMinute: 18 * 60, EndMinute: 22 * 60, SourceRevision: revision},
	}
	startDay := now.In(location)
	startDay = time.Date(startDay.Year(), startDay.Month(), startDay.Day(), 0, 0, 0, 0, location)
	return store.ReplaceCatalog(ctx, libraryseat.CatalogSnapshot{
		AppID:         campus.AppID,
		Revision:      revision,
		Source:        libraryseat.DemoSource,
		Authoritative: false,
		Complete:      true,
		ImportedAt:    now,
		ValidUntil:    startDay.AddDate(0, 0, 8),
		Spaces:        spaces,
		Seats:         seats,
		Slots:         slots,
	})
}
