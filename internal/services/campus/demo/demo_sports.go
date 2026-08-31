package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/sports"
)

// LoadSportsData 载入运动场馆演示快照（非权威）：场馆/项目/时段与订单 WebView 描述符。
// 不包含真实校方 URL、Cookie、学号或 DOM 选择器。
func LoadSportsData(ctx context.Context, store *sqlite.Store, now time.Time) error {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return err
	}
	revision := "demo-fixture-" + now.In(location).Format("20060102")
	startDay := now.In(location)
	startDay = time.Date(startDay.Year(), startDay.Month(), startDay.Day(), 0, 0, 0, 0, location)
	venues := []sports.Venue{
		{ID: "venue-info-gym", Name: "信息学部体育馆", Campus: "信息学部", Address: "演示地址（非官方）", SourceRevision: revision},
		{ID: "venue-wenli-field", Name: "文理学部操场", Campus: "文理学部", Address: "演示地址（非官方）", SourceRevision: revision},
	}
	projects := []sports.Project{
		{ID: "project-badminton", VenueID: "venue-info-gym", Name: "羽毛球", SourceRevision: revision},
		{ID: "project-table-tennis", VenueID: "venue-info-gym", Name: "乒乓球", SourceRevision: revision},
		{ID: "project-basketball", VenueID: "venue-wenli-field", Name: "篮球", SourceRevision: revision},
	}
	slots := make([]sports.Slot, 0, 18)
	for day := 0; day < 3; day++ {
		date := startDay.AddDate(0, 0, day)
		dateText := date.Format("2006-01-02")
		for _, spec := range []struct {
			projectID string
			venueID   string
			hour      int
			capacity  int
		}{
			{projectID: "project-badminton", venueID: "venue-info-gym", hour: 9, capacity: 4},
			{projectID: "project-badminton", venueID: "venue-info-gym", hour: 14, capacity: 1},
			{projectID: "project-table-tennis", venueID: "venue-info-gym", hour: 10, capacity: 2},
			{projectID: "project-basketball", venueID: "venue-wenli-field", hour: 16, capacity: 8},
		} {
			start := time.Date(date.Year(), date.Month(), date.Day(), spec.hour, 0, 0, 0, location)
			slots = append(slots, sports.Slot{
				ID:             fmt.Sprintf("slot-%s-%s-%02d00", spec.projectID, dateText, spec.hour),
				VenueID:        spec.venueID,
				ProjectID:      spec.projectID,
				Date:           dateText,
				StartAt:        start,
				EndAt:          start.Add(90 * time.Minute),
				Capacity:       spec.capacity,
				RemainingQuota: spec.capacity,
				SourceRevision: revision,
			})
		}
	}
	return store.ReplaceSportsSnapshot(ctx, sports.CatalogSnapshot{
		AppID:         campus.AppID,
		Revision:      revision,
		Source:        "demo-fixture-not-zhihui-luojia",
		Authoritative: false,
		Complete:      true,
		ImportedAt:    now,
		ValidUntil:    startDay.AddDate(0, 0, 8),
		Venues:        venues,
		Projects:      projects,
		Slots:         slots,
		WebView: sports.WebViewDescriptor{
			EntryURL:          "https://demo.ailuo.invalid/sports/orders",
			RequiredUserAgent: "AiluoCampusClient/1.0 (demo; non-authoritative)",
			RequiredHeaders: []sports.RequiredHeader{
				{Name: "X-Requested-With", Purpose: "Identify the governed campus client runtime"},
				{Name: "Referer", Purpose: "Satisfy same-site navigation checks without carrying secrets"},
			},
			RequiresDelegatedAuth: false,
			SourceRevision:        revision,
		},
	})
}
