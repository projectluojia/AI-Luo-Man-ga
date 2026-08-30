// Package demo 提供 campus 服务的演示数据播种：非权威、显式标记、仅开发环境
// 通过内核配置启用，永不进入生产数据面。数据经通用包文档端口写入 campus/bus
// namespace——与未来真实授权 ingestion（学校 API 经 OAuth 后写入）完全同构，
// 仅元数据中的权威性与来源标记不同。文档 JSON 形状是 campus-bus 包的领域
// 契约（权威来源在包仓库），本包保持一致的播种副本。
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
)

type stopDoc struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Aliases        []string `json:"aliases,omitempty"`
	Latitude       float64  `json:"latitude,omitempty"`
	Longitude      float64  `json:"longitude,omitempty"`
	SourceRevision string   `json:"source_revision"`
}

type routeDoc struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Direction      string `json:"direction"`
	OriginStopID   string `json:"origin_stop_id"`
	DestinationID  string `json:"destination_stop_id"`
	SourceRevision string `json:"source_revision"`
}

type journeyDoc struct {
	TripID            string    `json:"trip_id"`
	RouteID           string    `json:"route_id"`
	RouteName         string    `json:"route_name"`
	Direction         string    `json:"direction"`
	OriginStopID      string    `json:"origin_stop_id"`
	OriginStopName    string    `json:"origin_stop_name"`
	DestinationStopID string    `json:"destination_stop_id"`
	DestinationName   string    `json:"destination_stop_name"`
	DepartureAt       time.Time `json:"departure_at"`
	ArrivalAt         time.Time `json:"arrival_at"`
	SourceRevision    string    `json:"source_revision"`
}

// LoadBusData 载入校巴演示快照（非权威）：八天班次、显式标记来源与有效期。
func LoadBusData(ctx context.Context, docs packstore.Store, now time.Time) error {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return err
	}
	revision := "demo-fixture-" + now.In(location).Format("20060102")
	startDay := startOfDay(now, location)
	scope := packstore.Scope{AppID: campus.AppID, Namespace: campus.StorageNamespace}
	meta := packstore.SnapshotMeta{
		Revision: revision, Source: "demo-fixture-not-zhihui-luojia",
		Authoritative: false, Complete: true,
		ImportedAt: now, ValidUntil: startDay.AddDate(0, 0, 8),
	}

	stops := []stopDoc{
		{ID: "stop-wenli", Name: "文理学部", Aliases: []string{"文理学部站", "本部"}, SourceRevision: revision},
		{ID: "stop-gongxue", Name: "工学部", Aliases: []string{"工学部站"}, SourceRevision: revision},
		{ID: "stop-xinxi", Name: "信息学部", Aliases: []string{"信息学部站", "信部"}, SourceRevision: revision},
		{ID: "stop-yixue", Name: "医学部", Aliases: []string{"医学部站"}, SourceRevision: revision},
	}
	routes := []routeDoc{
		{ID: "route-wenli-xinxi", Name: "文理学部—信息学部", Direction: "文理学部至信息学部", OriginStopID: "stop-wenli", DestinationID: "stop-xinxi", SourceRevision: revision},
		{ID: "route-xinxi-wenli", Name: "信息学部—文理学部", Direction: "信息学部至文理学部", OriginStopID: "stop-xinxi", DestinationID: "stop-wenli", SourceRevision: revision},
		{ID: "route-gongxue-yixue", Name: "工学部—医学部", Direction: "工学部至医学部", OriginStopID: "stop-gongxue", DestinationID: "stop-yixue", SourceRevision: revision},
	}
	journeys := make([]journeyDoc, 0, len(routes)*8*8)
	for day := 0; day < 8; day++ {
		date := startDay.AddDate(0, 0, day)
		for _, route := range routes {
			origin := mustStop(stops, route.OriginStopID)
			destination := mustStop(stops, route.DestinationID)
			for _, hour := range []int{7, 9, 11, 13, 15, 17, 19, 21} {
				departure := time.Date(date.Year(), date.Month(), date.Day(), hour, 30, 0, 0, location)
				journeys = append(journeys, journeyDoc{
					TripID:            fmt.Sprintf("%s-%s-%02d30", route.ID, date.Format("20060102"), hour),
					RouteID:           route.ID,
					RouteName:         route.Name,
					Direction:         route.Direction,
					OriginStopID:      route.OriginStopID,
					OriginStopName:    origin.Name,
					DestinationStopID: route.DestinationID,
					DestinationName:   destination.Name,
					DepartureAt:       departure,
					ArrivalAt:         departure.Add(25 * time.Minute),
					SourceRevision:    revision,
				})
			}
		}
	}

	collections := map[string][]packstore.Document{
		"stops":    encodeDocuments(stops),
		"routes":   encodeDocuments(routes),
		"journeys": encodeDocuments(journeys),
	}
	return docs.ReplaceSnapshot(ctx, scope, meta, collections)
}

// startOfDay 返回 location 时区的当天零点。
func startOfDay(value time.Time, location *time.Location) time.Time {
	local := value.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

// mustStop 查找站点；演示数据是本包内字面量，引用缺失属于编程错误。
func mustStop(stops []stopDoc, id string) stopDoc {
	for _, stop := range stops {
		if stop.ID == id {
			return stop
		}
	}
	panic("demo route references unknown stop: " + id)
}

// identified 是演示领域文档的稳定文档 ID 抽象（stop/route 用 ID，journey 用 TripID）。
type identified interface{ docID() string }

func (s stopDoc) docID() string    { return s.ID }
func (r routeDoc) docID() string   { return r.ID }
func (j journeyDoc) docID() string { return j.TripID }

// encodeDocuments 序列化一组领域文档为 packstore 集合。
func encodeDocuments[T identified](documents []T) []packstore.Document {
	encoded := make([]packstore.Document, 0, len(documents))
	for _, document := range documents {
		payload, err := json.Marshal(document)
		if err != nil {
			panic(fmt.Sprintf("encode demo document %s: %v", document.docID(), err))
		}
		encoded = append(encoded, packstore.Document{ID: document.docID(), Payload: payload})
	}
	return encoded
}
