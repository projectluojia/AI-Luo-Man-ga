package academiccalendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	EventsListToolID                  = "campus.calendar.events.list"
	EventsListCapabilityID            = EventsListToolID
	AcademicCalendarQueryCapabilityID = "campus.academic_calendar.query"
	CalendarQueryCapabilityID         = "campus.calendar.query"
	InputSchemaJSON                   = `{"type":"object","properties":{"from":{"type":"string","format":"date-time"},"to":{"type":"string","format":"date-time"},"limit":{"type":"integer","minimum":1,"maximum":5000}},"required":["from","to"],"additionalProperties":false}`
)

type QueryResult struct {
	DataStatus DataStatus `json:"data_status"`
	Events     []Event    `json:"events"`
}

func ToolSpecs() []registry.ToolSpec {
	return []registry.ToolSpec{{ID: EventsListToolID, Version: "1.0.0", Description: "List authoritative Wuhan University academic calendar events.", InputSchemaJSON: InputSchemaJSON, SideEffect: registry.SideEffectRead}}
}

func ToolRegistrations(store Store) []registry.ToolRegistration {
	return []registry.ToolRegistration{{Spec: ToolSpecs()[0], Handler: eventsHandler(store)}}
}

func ToolHandlers(store Store) map[string]registry.Handler {
	return map[string]registry.Handler{EventsListToolID: eventsHandler(store)}
}

func eventsHandler(store Store) registry.Handler {
	return func(ctx context.Context, req contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if store == nil {
			return nil, errors.New("academic calendar store unavailable")
		}
		var in QueryRequest
		if err := jsonutil.DecodeStrict(payload, &in); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		if err := in.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		snap, err := store.Search(ctx, req.AppID, in)
		if err != nil {
			return nil, err
		}
		status, err := snap.Metadata.Govern(time.Now().UTC())
		if err != nil {
			return nil, fmt.Errorf("govern calendar snapshot: %w", err)
		}
		events := snap.Events
		if events == nil {
			events = []Event{}
		}
		for _, e := range events {
			if e.SourceRevision != "" && e.SourceRevision != snap.Metadata.Revision {
				return nil, ErrDataIncomplete
			}
		}
		return json.Marshal(QueryResult{DataStatus: status, Events: events})
	}
}
