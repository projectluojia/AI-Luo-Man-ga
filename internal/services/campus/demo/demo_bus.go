// Package demo 提供 campus 服务的演示数据播种：非权威、显式标记、仅开发环境
// 通过内核配置启用，永不进入生产数据面。
package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/bus"
)

// LoadBusData 载入校巴演示快照（非权威）：八天班次、显式标记来源与有效期。
func LoadBusData(ctx context.Context, store *sqlite.Store, now time.Time) error {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return err
	}
	revision := "demo-fixture-" + now.In(location).Format("20060102")
	stops := []bus.Stop{
		{ID: "stop-wenli", Name: "文理学部", Aliases: []string{"文理学部站", "本部"}, SourceRevision: revision},
		{ID: "stop-gongxue", Name: "工学部", Aliases: []string{"工学部站"}, SourceRevision: revision},
		{ID: "stop-xinxi", Name: "信息学部", Aliases: []string{"信息学部站", "信部"}, SourceRevision: revision},
		{ID: "stop-yixue", Name: "医学部", Aliases: []string{"医学部站"}, SourceRevision: revision},
	}
	routes := []bus.Route{
		{ID: "route-wenli-xinxi", Name: "文理学部—信息学部", Direction: "文理学部至信息学部", OriginStopID: "stop-wenli", DestinationID: "stop-xinxi", SourceRevision: revision},
		{ID: "route-xinxi-wenli", Name: "信息学部—文理学部", Direction: "信息学部至文理学部", OriginStopID: "stop-xinxi", DestinationID: "stop-wenli", SourceRevision: revision},
		{ID: "route-gongxue-yixue", Name: "工学部—医学部", Direction: "工学部至医学部", OriginStopID: "stop-gongxue", DestinationID: "stop-yixue", SourceRevision: revision},
	}
	journeys := make([]bus.Journey, 0, len(routes)*8*8)
	startDay := now.In(location)
	startDay = time.Date(startDay.Year(), startDay.Month(), startDay.Day(), 0, 0, 0, 0, location)
	for day := 0; day < 8; day++ {
		date := startDay.AddDate(0, 0, day)
		for _, route := range routes {
			for _, hour := range []int{7, 9, 11, 13, 15, 17, 19, 21} {
				departure := time.Date(date.Year(), date.Month(), date.Day(), hour, 30, 0, 0, location)
				origin := findStop(stops, route.OriginStopID)
				destination := findStop(stops, route.DestinationID)
				journeys = append(journeys, bus.Journey{
					TripID:            fmt.Sprintf("%s-%s-%02d30", route.ID, date.Format("20060102"), hour),
					RouteID:           route.ID,
					RouteName:         route.Name,
					Direction:         route.Direction,
					OriginStopID:      origin.ID,
					OriginStopName:    origin.Name,
					DestinationStopID: destination.ID,
					DestinationName:   destination.Name,
					DepartureAt:       departure,
					ArrivalAt:         departure.Add(25 * time.Minute),
					SourceRevision:    revision,
				})
			}
		}
	}
	return store.ReplaceBusSnapshot(ctx, sqlite.BusSnapshot{
		AppID:         campus.AppID,
		Revision:      revision,
		Source:        "demo-fixture-not-zhihui-luojia",
		Authoritative: false,
		Complete:      true,
		ImportedAt:    now,
		ValidUntil:    startDay.AddDate(0, 0, 8),
		Stops:         stops,
		Routes:        routes,
		Journeys:      journeys,
	})
}

func findStop(stops []bus.Stop, id string) bus.Stop {
	for _, stop := range stops {
		if stop.ID == id {
			return stop
		}
	}
	panic("demo route references unknown stop: " + id)
}
