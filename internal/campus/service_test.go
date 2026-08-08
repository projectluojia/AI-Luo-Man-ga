package campus_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/campus/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
)

func TestBusJourneyCapability(t *testing.T) {
	t.Parallel()

	baseTime := time.Date(2026, time.July, 24, 8, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	store := memory.NewBusStore()
	store.Replace(campus.AppID, []bus.Journey{
		journey("trip-late", "stop-a", "stop-b", baseTime.Add(90*time.Minute)),
		journey("trip-before", "stop-a", "stop-b", baseTime.Add(-time.Minute)),
		journey("trip-early", "stop-a", "stop-b", baseTime.Add(30*time.Minute)),
		journey("trip-other-destination", "stop-a", "stop-c", baseTime.Add(10*time.Minute)),
	})

	dispatcher := newCampusDispatcher(t, store, campus.AppID)
	payload := mustJSON(t, bus.SearchRequest{
		OriginStopID:      "stop-a",
		DestinationStopID: "stop-b",
		DepartAfter:       baseTime,
		Limit:             10,
	})

	resultPayload, err := dispatcher.InvokeCapability(context.Background(), requestContext(campus.AppID), campus.BusJourneySearchCapabilityID, payload)
	if err != nil {
		t.Fatalf("invoke bus journey capability: %v", err)
	}
	var result bus.SearchResult
	if err := json.Unmarshal(resultPayload, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Journeys) != 2 {
		t.Fatalf("got %d journeys, want 2", len(result.Journeys))
	}
	if result.Journeys[0].TripID != "trip-early" || result.Journeys[1].TripID != "trip-late" {
		t.Fatalf("journeys are not ordered by departure: %#v", result.Journeys)
	}
}

func TestBusJourneyCapabilityReturnsEmptyArray(t *testing.T) {
	t.Parallel()

	store := memory.NewBusStore()
	store.Replace(campus.AppID, []bus.Journey{})
	dispatcher := newCampusDispatcher(t, store, campus.AppID)
	payload := mustJSON(t, bus.SearchRequest{
		OriginStopID:      "stop-a",
		DestinationStopID: "stop-b",
		DepartAfter:       time.Now(),
	})
	resultPayload, err := dispatcher.InvokeCapability(context.Background(), requestContext(campus.AppID), campus.BusJourneySearchCapabilityID, payload)
	if err != nil {
		t.Fatalf("invoke bus journey capability: %v", err)
	}
	var result bus.SearchResult
	if err := json.Unmarshal(resultPayload, &result); err != nil || len(result.Journeys) != 0 ||
		result.DataStatus.State != bus.DataStateAuthoritativeFresh {
		t.Fatalf("got %s err=%v, want a governed empty array", resultPayload, err)
	}
}

func TestBusJourneyCapabilityEnforcesAppPolicy(t *testing.T) {
	t.Parallel()

	dispatcher := newCampusDispatcher(t, memory.NewBusStore())
	_, err := dispatcher.InvokeCapability(context.Background(), requestContext(campus.AppID), campus.BusJourneySearchCapabilityID, validPayload(t))
	if !errors.Is(err, runtime.ErrCapabilityDisabled) {
		t.Fatalf("got error %v, want ErrCapabilityDisabled", err)
	}
}

func TestBusJourneyStoreIsIsolatedByApp(t *testing.T) {
	t.Parallel()

	baseTime := time.Now().Add(time.Hour)
	store := memory.NewBusStore()
	store.Replace(campus.AppID, []bus.Journey{journey("campus-trip", "stop-a", "stop-b", baseTime)})
	dispatcher := newCampusDispatcher(t, store, "another-app")

	_, err := dispatcher.InvokeCapability(context.Background(), requestContext("another-app"), campus.BusJourneySearchCapabilityID, validPayload(t))
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("cross-App query error=%v, want ErrDataUnavailable", err)
	}
}

func TestBusCapabilityRejectsNonAuthoritativeAndExpiredSnapshots(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	tests := []struct {
		name     string
		metadata bus.SnapshotMetadata
		target   error
	}{
		{
			name: "non-authoritative",
			metadata: bus.SnapshotMetadata{
				Revision: "demo", Source: "demo-copy", ImportedAt: now.Add(-time.Hour),
				ValidUntil: now.Add(time.Hour), Complete: true,
			},
			target: contracts.ErrDataUntrusted,
		},
		{
			name: "expired",
			metadata: bus.SnapshotMetadata{
				Revision: "expired", Source: "zhihui-luojia", Authoritative: true,
				ImportedAt: now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Hour), Complete: true,
			},
			target: contracts.ErrDataExpired,
		},
		{
			name: "incomplete",
			metadata: bus.SnapshotMetadata{
				Revision: "incomplete", Source: "zhihui-luojia", Authoritative: true,
				ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
			},
			target: contracts.ErrDataIncomplete,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			store := memory.NewBusStore()
			store.Replace(campus.AppID, []bus.Journey{})
			store.SetSnapshotMetadata(campus.AppID, test.metadata)
			dispatcher := newCampusDispatcher(t, store, campus.AppID)
			_, err := dispatcher.InvokeCapability(
				context.Background(),
				requestContext(campus.AppID),
				campus.BusJourneySearchCapabilityID,
				validPayload(t),
			)
			if !errors.Is(err, test.target) {
				t.Fatalf("error=%v, want %v", err, test.target)
			}
		})
	}
}

func TestBusJourneyCapabilityValidatesInput(t *testing.T) {
	t.Parallel()

	dispatcher := newCampusDispatcher(t, memory.NewBusStore(), campus.AppID)
	payload := mustJSON(t, bus.SearchRequest{DestinationStopID: "stop-b"})
	_, err := dispatcher.InvokeCapability(context.Background(), requestContext(campus.AppID), campus.BusJourneySearchCapabilityID, payload)
	if !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("got error %v, want ErrSchemaValidation", err)
	}
}

func TestBusJourneyCapabilityPropagatesCancellation(t *testing.T) {
	t.Parallel()

	dispatcher := newCampusDispatcher(t, memory.NewBusStore(), campus.AppID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dispatcher.InvokeCapability(ctx, requestContext(campus.AppID), campus.BusJourneySearchCapabilityID, validPayload(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want context.Canceled", err)
	}
}

func TestBusJourneyCapabilityEnforcesCallDepth(t *testing.T) {
	t.Parallel()

	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusJourneySearchCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.WithMaxCallDepth(2))
	if err := campus.Register(reg, dispatcher, memory.NewBusStore()); err != nil {
		t.Fatalf("register campus service: %v", err)
	}
	request := requestContext(campus.AppID)
	request.CallDepth = 1

	_, err := dispatcher.InvokeCapability(context.Background(), request, campus.BusJourneySearchCapabilityID, validPayload(t))
	if !errors.Is(err, runtime.ErrCallDepthExceeded) {
		t.Fatalf("got error %v, want ErrCallDepthExceeded", err)
	}
}

func BenchmarkBusJourneyCapability(b *testing.B) {
	baseTime := time.Date(2026, time.July, 25, 8, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	store := memory.NewBusStore()
	store.Replace(campus.AppID, []bus.Journey{
		journey("trip-1", "stop-a", "stop-b", baseTime.Add(time.Hour)),
		journey("trip-2", "stop-a", "stop-b", baseTime.Add(2*time.Hour)),
	})
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusJourneySearchCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy)
	if err := campus.Register(reg, dispatcher, store); err != nil {
		b.Fatal(err)
	}
	payload, err := json.Marshal(bus.SearchRequest{
		OriginStopID: "stop-a", DestinationStopID: "stop-b", DepartAfter: baseTime, Limit: 10,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		request := requestContext(campus.AppID)
		if _, err := dispatcher.InvokeCapability(context.Background(), request, campus.BusJourneySearchCapabilityID, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func newCampusDispatcher(t *testing.T, store bus.Store, enabledApps ...string) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	policy := runtime.NewStaticAppPolicy()
	for _, appID := range enabledApps {
		policy.Enable(appID, campus.BusJourneySearchCapabilityID)
	}
	dispatcher := runtime.NewDispatcher(reg, policy)
	if err := campus.Register(reg, dispatcher, store); err != nil {
		t.Fatalf("register campus service: %v", err)
	}
	return dispatcher
}

func requestContext(appID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID:           appID,
		EchoID:          "echo-test",
		RequestID:       "request-test",
		TraceID:         "trace-test",
		Deadline:        time.Now().Add(time.Minute),
		ProtocolVersion: "1.0",
	}
}

func validPayload(t *testing.T) json.RawMessage {
	t.Helper()
	return mustJSON(t, bus.SearchRequest{
		OriginStopID:      "stop-a",
		DestinationStopID: "stop-b",
		DepartAfter:       time.Now(),
	})
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

func journey(id, origin, destination string, departure time.Time) bus.Journey {
	return bus.Journey{
		TripID:            id,
		RouteID:           "route-1",
		RouteName:         "文理学部-信息学部",
		Direction:         "outbound",
		OriginStopID:      origin,
		OriginStopName:    origin,
		DestinationStopID: destination,
		DestinationName:   destination,
		DepartureAt:       departure,
		ArrivalAt:         departure.Add(20 * time.Minute),
		SourceRevision:    "test-revision",
	}
}
