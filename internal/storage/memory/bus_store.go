package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/bus"
)

// BusStore 是用于测试和本地开发的并发安全参考适配器。
// 生产数据库适配器必须实现相同的 bus.Store 端口。
type BusStore struct {
	mu        sync.RWMutex
	journeys  map[string][]bus.Journey
	stops     map[string][]bus.Stop
	routes    map[string][]bus.Route
	metadata  map[string]bus.SnapshotMetadata
	positions map[string][]bus.VehiclePosition
}

func NewBusStore() *BusStore {
	return &BusStore{
		journeys:  make(map[string][]bus.Journey),
		stops:     make(map[string][]bus.Stop),
		routes:    make(map[string][]bus.Route),
		metadata:  make(map[string]bus.SnapshotMetadata),
		positions: make(map[string][]bus.VehiclePosition),
	}
}

func (s *BusStore) ReplacePositions(appID string, positions []bus.VehiclePosition) {
	s.mu.Lock()
	metadata := s.ensureMetadataLocked(appID)
	for i := range positions {
		positions[i].SourceRevision = metadata.Revision
	}
	s.positions[appID] = append([]bus.VehiclePosition(nil), positions...)
	s.mu.Unlock()
}

func (s *BusStore) ReplaceCatalog(appID string, stops []bus.Stop, routes []bus.Route) {
	s.mu.Lock()
	metadata := s.ensureMetadataLocked(appID)
	for index := range stops {
		stops[index].SourceRevision = metadata.Revision
	}
	for index := range routes {
		routes[index].SourceRevision = metadata.Revision
	}
	s.stops[appID] = append([]bus.Stop(nil), stops...)
	s.routes[appID] = append([]bus.Route(nil), routes...)
	s.mu.Unlock()
}

func (s *BusStore) Replace(appID string, journeys []bus.Journey) {
	copyOfJourneys := append([]bus.Journey(nil), journeys...)
	sort.Slice(copyOfJourneys, func(i, j int) bool {
		return copyOfJourneys[i].DepartureAt.Before(copyOfJourneys[j].DepartureAt)
	})
	s.mu.Lock()
	metadata := s.ensureMetadataLocked(appID)
	for index := range copyOfJourneys {
		copyOfJourneys[index].SourceRevision = metadata.Revision
	}
	s.journeys[appID] = copyOfJourneys
	s.mu.Unlock()
}

func (s *BusStore) SetSnapshotMetadata(appID string, metadata bus.SnapshotMetadata) {
	s.mu.Lock()
	s.metadata[appID] = metadata
	s.mu.Unlock()
}

func (s *BusStore) SearchStops(ctx context.Context, appID string, request bus.StopSearchRequest) (bus.StopSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return bus.StopSnapshot{}, err
	}
	query := strings.ToLower(request.Query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, exists := s.metadata[appID]
	if !exists {
		return bus.StopSnapshot{}, contracts.ErrDataUnavailable
	}
	result := make([]bus.Stop, 0, min(request.Limit, len(s.stops[appID])))
	for _, stop := range s.stops[appID] {
		matched := strings.Contains(strings.ToLower(stop.Name), query)
		for _, alias := range stop.Aliases {
			matched = matched || strings.Contains(strings.ToLower(alias), query)
		}
		if matched {
			result = append(result, stop)
		}
		if len(result) == request.Limit {
			break
		}
	}
	return bus.StopSnapshot{Metadata: metadata, Stops: result}, nil
}

func (s *BusStore) ListRoutes(ctx context.Context, appID string, request bus.RouteListRequest) (bus.RouteSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return bus.RouteSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, exists := s.metadata[appID]
	if !exists {
		return bus.RouteSnapshot{}, contracts.ErrDataUnavailable
	}
	limit := min(request.Limit, len(s.routes[appID]))
	return bus.RouteSnapshot{
		Metadata: metadata,
		Routes:   append([]bus.Route(nil), s.routes[appID][:limit]...),
	}, nil
}

func (s *BusStore) SearchJourneys(ctx context.Context, appID string, request bus.SearchRequest) (bus.JourneySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return bus.JourneySnapshot{}, err
	}
	s.mu.RLock()
	metadata, exists := s.metadata[appID]
	if !exists {
		s.mu.RUnlock()
		return bus.JourneySnapshot{}, contracts.ErrDataUnavailable
	}
	items := s.journeys[appID]
	result := make([]bus.Journey, 0, min(request.Limit, len(items)))
	for _, journey := range items {
		if journey.OriginStopID != request.OriginStopID || journey.DestinationStopID != request.DestinationStopID {
			continue
		}
		if journey.DepartureAt.Before(request.DepartAfter) {
			continue
		}
		result = append(result, journey)
		if len(result) == request.Limit {
			break
		}
	}
	s.mu.RUnlock()
	return bus.JourneySnapshot{Metadata: metadata, Journeys: result}, nil
}

func (s *BusStore) SearchVehiclePositions(ctx context.Context, appID string, request bus.RealtimePositionRequest) (bus.RealtimePositionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return bus.RealtimePositionSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, exists := s.metadata[appID]
	if !exists {
		return bus.RealtimePositionSnapshot{}, contracts.ErrDataUnavailable
	}
	result := make([]bus.VehiclePosition, 0, min(request.Limit, len(s.positions[appID])))
	for _, p := range s.positions[appID] {
		if request.RouteID != "" && p.RouteID != request.RouteID {
			continue
		}
		result = append(result, p)
		if len(result) == request.Limit {
			break
		}
	}
	return bus.RealtimePositionSnapshot{Metadata: metadata, Positions: result}, nil
}

func (s *BusStore) ensureMetadataLocked(appID string) bus.SnapshotMetadata {
	if metadata, exists := s.metadata[appID]; exists {
		return metadata
	}
	now := time.Now().UTC()
	metadata := bus.SnapshotMetadata{
		Revision:      "memory-test-authoritative",
		Source:        "memory-test-authoritative",
		Authoritative: true,
		Complete:      true,
		ImportedAt:    now.Add(-time.Minute),
		ValidUntil:    now.Add(24 * time.Hour),
	}
	s.metadata[appID] = metadata
	return metadata
}
