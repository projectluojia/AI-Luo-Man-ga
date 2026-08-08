package campus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
)

const (
	AppID     = "campus-services"
	ServiceID = "campus"

	BusStopSearchCapabilityID    = "campus.bus.stops.search"
	BusRouteListCapabilityID     = "campus.bus.routes.list"
	BusJourneySearchCapabilityID = "campus.bus.journeys.search"
)

func Register(reg *registry.Registry, tools runtime.ToolCaller, busStore bus.Store) error {
	for _, registration := range bus.ToolRegistrations(busStore) {
		if err := reg.RegisterTool(registration); err != nil {
			return fmt.Errorf("register bus tool %q: %w", registration.Spec.ID, err)
		}
	}

	toolHandler := func(toolID string) registry.Handler {
		return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
			return tools.UseTool(ctx, request, ServiceID, toolID, payload)
		}
	}

	if err := reg.RegisterService(registry.ServiceRegistration{
		Spec: registry.ServiceSpec{
			ID:               ServiceID,
			Version:          "1.0.0",
			Description:      "Campus-wide public services.",
			ToolDependencies: []string{bus.StopSearchToolID, bus.RouteListToolID, bus.JourneySearchToolID},
		},
		Capabilities: map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}{
			BusStopSearchCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID:              BusStopSearchCapabilityID,
					Version:         "1.0.0",
					Name:            "查询校巴站点",
					Description:     "Search campus bus stops by a user-provided stop name or alias.",
					ServiceID:       ServiceID,
					InputSchemaJSON: bus.StopSearchInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: toolHandler(bus.StopSearchToolID),
			},
			BusRouteListCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID:              BusRouteListCapabilityID,
					Version:         "1.0.0",
					Name:            "列出校巴线路",
					Description:     "List currently available campus bus routes and directions.",
					ServiceID:       ServiceID,
					InputSchemaJSON: bus.RouteListInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: toolHandler(bus.RouteListToolID),
			},
			BusJourneySearchCapabilityID: {
				Spec: registry.CapabilitySpec{
					ID:              BusJourneySearchCapabilityID,
					Version:         "1.0.0",
					Name:            "查询校巴行程",
					Description:     "Find direct campus bus journeys for stable origin and destination stop IDs and a departure time.",
					ServiceID:       ServiceID,
					InputSchemaJSON: bus.JourneySearchInputSchemaJSON,
					SideEffect:      registry.SideEffectRead,
				},
				Handler: toolHandler(bus.JourneySearchToolID),
			},
		},
	}); err != nil {
		return fmt.Errorf("register campus service: %w", err)
	}
	return nil
}
