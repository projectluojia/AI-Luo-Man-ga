package bus

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

var (
	ErrOriginRequired      = errors.New("origin_stop_id is required")
	ErrDestinationRequired = errors.New("destination_stop_id is required")
	ErrSameStop            = errors.New("origin and destination stops must differ")
	ErrInvalidLimit        = errors.New("limit must be between 1 and 50")
	ErrQueryRequired       = errors.New("query is required")
)

const DataStateAuthoritativeFresh = "authoritative_fresh"

type SnapshotMetadata struct {
	Revision           string    `json:"source_revision"`
	Source             string    `json:"source"`
	Authoritative      bool      `json:"authoritative"`
	Complete           bool      `json:"complete"`
	ImportedAt         time.Time `json:"imported_at"`
	ValidUntil         time.Time `json:"valid_until"`
	RealtimeAuthorized bool      `json:"realtime_authorized,omitempty"`
}

// VehiclePosition 是校方授权后才能对外提供的实时车辆位置。
type VehiclePosition struct {
	VehicleID      string    `json:"vehicle_id"`
	RouteID        string    `json:"route_id"`
	Latitude       float64   `json:"latitude"`
	Longitude      float64   `json:"longitude"`
	RecordedAt     time.Time `json:"recorded_at"`
	SourceRevision string    `json:"source_revision"`
}

type RealtimePositionRequest struct {
	RouteID string `json:"route_id,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (r *RealtimePositionRequest) NormalizeAndValidate() error {
	if r.RouteID != "" && strings.TrimSpace(r.RouteID) == "" {
		return ErrQueryRequired
	}
	r.RouteID = strings.TrimSpace(r.RouteID)
	if r.Limit == 0 {
		r.Limit = 50
	}
	if r.Limit < 1 || r.Limit > 100 {
		return ErrInvalidLimit
	}
	return nil
}

type RealtimePositionSnapshot struct {
	Metadata  SnapshotMetadata
	Positions []VehiclePosition
}

type RealtimePositionResult struct {
	DataStatus DataStatus        `json:"data_status"`
	Positions  []VehiclePosition `json:"positions"`
}

// DataStatus 是工具结果的治理状态：State 加快照元数据（嵌入展开为扁平 JSON）。
type DataStatus struct {
	State string `json:"state"`
	SnapshotMetadata
}

func (m SnapshotMetadata) Govern(now time.Time) (DataStatus, error) {
	if m.Revision == "" || m.Source == "" || !m.Complete || m.ImportedAt.IsZero() || m.ValidUntil.IsZero() ||
		!m.ValidUntil.After(m.ImportedAt) || now.Before(m.ImportedAt) {
		return DataStatus{}, contracts.ErrDataIncomplete
	}
	if !m.Authoritative {
		return DataStatus{}, contracts.ErrDataUntrusted
	}
	if !now.Before(m.ValidUntil) {
		return DataStatus{}, contracts.ErrDataExpired
	}
	return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMetadata: m}, nil
}

func (m SnapshotMetadata) GovernRealtime(now time.Time) (DataStatus, error) {
	status, err := m.Govern(now)
	if err != nil {
		return DataStatus{}, err
	}
	if !m.RealtimeAuthorized {
		return DataStatus{}, contracts.ErrRealtimeUnauthorized
	}
	return status, nil
}

type Stop struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Aliases        []string `json:"aliases,omitempty"`
	Latitude       float64  `json:"latitude,omitempty"`
	Longitude      float64  `json:"longitude,omitempty"`
	SourceRevision string   `json:"source_revision"`
}

type StopSearchRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

func (r *StopSearchRequest) NormalizeAndValidate() error {
	r.Query = strings.TrimSpace(r.Query)
	if r.Limit == 0 {
		r.Limit = 10
	}
	if r.Query == "" {
		return ErrQueryRequired
	}
	if r.Limit < 1 || r.Limit > 50 {
		return ErrInvalidLimit
	}
	return nil
}

type StopSearchResult struct {
	DataStatus DataStatus `json:"data_status"`
	Stops      []Stop     `json:"stops"`
}

type StopSnapshot struct {
	Metadata SnapshotMetadata
	Stops    []Stop
}

type Route struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Direction      string `json:"direction"`
	OriginStopID   string `json:"origin_stop_id"`
	DestinationID  string `json:"destination_stop_id"`
	SourceRevision string `json:"source_revision"`
}

type RouteListRequest struct {
	Limit int `json:"limit,omitempty"`
}

func (r *RouteListRequest) NormalizeAndValidate() error {
	if r.Limit == 0 {
		r.Limit = 50
	}
	if r.Limit < 1 || r.Limit > 50 {
		return ErrInvalidLimit
	}
	return nil
}

type RouteListResult struct {
	DataStatus DataStatus `json:"data_status"`
	Routes     []Route    `json:"routes"`
}

type RouteSnapshot struct {
	Metadata SnapshotMetadata
	Routes   []Route
}

type Journey struct {
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

type SearchRequest struct {
	OriginStopID      string    `json:"origin_stop_id"`
	DestinationStopID string    `json:"destination_stop_id"`
	DepartAfter       time.Time `json:"depart_after"`
	Limit             int       `json:"limit,omitempty"`
}

func (r *SearchRequest) NormalizeAndValidate(now time.Time) error {
	if r.Limit == 0 {
		r.Limit = 10
	}
	if r.DepartAfter.IsZero() {
		r.DepartAfter = now
	}
	switch {
	case r.OriginStopID == "":
		return ErrOriginRequired
	case r.DestinationStopID == "":
		return ErrDestinationRequired
	case r.OriginStopID == r.DestinationStopID:
		return ErrSameStop
	case r.Limit < 1 || r.Limit > 50:
		return ErrInvalidLimit
	default:
		return nil
	}
}

type SearchResult struct {
	DataStatus DataStatus `json:"data_status"`
	Journeys   []Journey  `json:"journeys"`
}

type JourneySnapshot struct {
	Metadata SnapshotMetadata
	Journeys []Journey
}

// Store 是 Go 内核注入的存储端口。具体实现持有数据库连接，并负责强制 App 数据隔离。
type Store interface {
	SearchStops(context.Context, string, StopSearchRequest) (StopSnapshot, error)
	ListRoutes(context.Context, string, RouteListRequest) (RouteSnapshot, error)
	SearchJourneys(context.Context, string, SearchRequest) (JourneySnapshot, error)
	SearchVehiclePositions(context.Context, string, RealtimePositionRequest) (RealtimePositionSnapshot, error)
}
