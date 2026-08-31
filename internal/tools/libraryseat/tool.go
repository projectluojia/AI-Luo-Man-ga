package libraryseat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	SpacesListToolID         = "library.spaces.list"
	SlotsSearchToolID        = "library.slots.search"
	ReservationsCreateToolID = "library.reservations.create"
	ReservationsCancelToolID = "library.reservations.cancel"
	ReservationsMineToolID   = "library.reservations.mine"

	SpacesListInputSchemaJSON         = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
	SlotsSearchInputSchemaJSON        = `{"type":"object","properties":{"space_id":{"type":"string","minLength":1,"maxLength":128},"date":{"type":"string","minLength":10,"maxLength":10,"pattern":"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"},"slot_id":{"type":"string","minLength":1,"maxLength":128},"limit":{"type":"integer","minimum":1,"maximum":200}},"required":["space_id","date"],"additionalProperties":false}`
	ReservationsCreateInputSchemaJSON = `{"type":"object","properties":{"space_id":{"type":"string","minLength":1,"maxLength":128},"seat_id":{"type":"string","minLength":1,"maxLength":128},"slot_id":{"type":"string","minLength":1,"maxLength":128},"date":{"type":"string","minLength":10,"maxLength":10,"pattern":"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"}},"required":["space_id","seat_id","slot_id","date"],"additionalProperties":false}`
	ReservationsCancelInputSchemaJSON = `{"type":"object","properties":{"reservation_id":{"type":"string","minLength":1,"maxLength":128}},"required":["reservation_id"],"additionalProperties":false}`
	ReservationsMineInputSchemaJSON   = `{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`
)

// ToolSpecs 返回座位预约原子 Tool 规格。写操作要求确认；读操作禁止确认。
func ToolSpecs() []registry.ToolSpec {
	read := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectRead}
	}
	writeConfirm := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectWrite, RequiresConfirmation: true}
	}
	return []registry.ToolSpec{
		read(SpacesListToolID, "List library spaces in the current catalog snapshot.", SpacesListInputSchemaJSON),
		read(SlotsSearchToolID, "Search seats by space, Asia/Shanghai date and optional slot.", SlotsSearchInputSchemaJSON),
		writeConfirm(ReservationsCreateToolID, "Create a library seat reservation for the current user.", ReservationsCreateInputSchemaJSON),
		writeConfirm(ReservationsCancelToolID, "Cancel a library seat reservation owned by the current user.", ReservationsCancelInputSchemaJSON),
		read(ReservationsMineToolID, "List library seat reservations owned by the current user.", ReservationsMineInputSchemaJSON),
	}
}

// ToolRegistrations 返回由统一存储端口驱动的 Tool 注册项。
func ToolRegistrations(store Store, now func() time.Time) []registry.ToolRegistration {
	handlers := ToolHandlers(store, now)
	registrations := make([]registry.ToolRegistration, 0, len(ToolSpecs()))
	for _, spec := range ToolSpecs() {
		registrations = append(registrations, registry.ToolRegistration{Spec: spec, Handler: handlers[spec.ID]})
	}
	return registrations
}

// ToolHandlers 构造原子 Tool 处理器。
func ToolHandlers(store Store, now func() time.Time) map[string]registry.Handler {
	if now == nil {
		now = time.Now
	}
	return map[string]registry.Handler{
		SpacesListToolID:         listSpacesHandler(store, now),
		SlotsSearchToolID:        searchSlotsHandler(store, now),
		ReservationsCreateToolID: createReservationHandler(store, now),
		ReservationsCancelToolID: cancelReservationHandler(store, now),
		ReservationsMineToolID:   mineReservationsHandler(store, now),
	}
}

func listSpacesHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input SpaceListRequest
		if err := decodeOptional(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, invalid(err)
		}
		observe.Debug(ctx, "开始查询图书馆空间",
			observe.StringAttr("app_id", request.AppID),
			observe.IntAttr("limit", input.Limit),
		)
		snapshot, err := store.ListSpaces(ctx, request.AppID, input)
		if err != nil {
			return nil, err
		}
		dataStatus, err := governCatalog(ctx, "spaces", snapshot.Metadata, now())
		if err != nil {
			return nil, err
		}
		spaces := snapshot.Spaces
		if spaces == nil {
			spaces = []Space{}
		}
		observe.Info(ctx, "图书馆空间查询完成",
			observe.IntAttr("result_count", len(spaces)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(SpaceListResult{DataStatus: dataStatus, Spaces: spaces})
	}
}

func searchSlotsHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input SlotSearchRequest
		if err := decodeRequired(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, invalid(err)
		}
		observe.Debug(ctx, "开始查询图书馆座位时段",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("space_id", input.SpaceID),
			observe.StringAttr("slot_date", input.Date),
			observe.IntAttr("limit", input.Limit),
		)
		snapshot, err := store.SearchSeats(ctx, request.AppID, input)
		if err != nil {
			return nil, err
		}
		dataStatus, err := governCatalog(ctx, "slots", snapshot.Metadata, now())
		if err != nil {
			return nil, err
		}
		result := SlotSearchResult{DataStatus: dataStatus, Space: snapshot.Space, Date: input.Date, Slots: assembleSlotSeats(snapshot, input.Limit)}
		observe.Info(ctx, "图书馆座位时段查询完成",
			observe.IntAttr("slot_count", len(result.Slots)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(result)
	}
}

func createReservationHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request.UserID); err != nil {
			return nil, invalid(err)
		}
		var input CreateReservationRequest
		if err := decodeRequired(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, invalid(err)
		}
		observe.Debug(ctx, "开始创建图书馆座位预约",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("space_id", input.SpaceID),
			observe.StringAttr("seat_id", input.SeatID),
			observe.StringAttr("slot_id", input.SlotID),
			observe.StringAttr("slot_date", input.Date),
		)
		reservation, err := store.CreateReservation(ctx, CreateReservationInput{
			AppID: request.AppID, UserID: request.UserID,
			SpaceID: input.SpaceID, SeatID: input.SeatID, SlotID: input.SlotID, Date: input.Date,
			IdempotencyKey: request.IdempotencyKey,
		}, now())
		if err != nil {
			return nil, err
		}
		dataStatus := reservationDataStatus(reservation)
		observe.Info(ctx, "图书馆座位预约已创建",
			observe.StringAttr("reservation_status", reservation.Status),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(ReservationResult{DataStatus: dataStatus, Reservation: reservation})
	}
}

func cancelReservationHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request.UserID); err != nil {
			return nil, invalid(err)
		}
		var input struct {
			ReservationID string `json:"reservation_id"`
		}
		if err := decodeRequired(payload, &input); err != nil {
			return nil, err
		}
		input.ReservationID = strings.TrimSpace(input.ReservationID)
		if input.ReservationID == "" || !validStableID(input.ReservationID) {
			return nil, invalid(ErrReservationRequired)
		}
		observe.Debug(ctx, "开始取消图书馆座位预约",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("reservation_id", input.ReservationID),
		)
		reservation, err := store.CancelReservation(ctx, CancelReservationInput{
			AppID: request.AppID, UserID: request.UserID, ReservationID: input.ReservationID,
		}, now())
		if err != nil {
			return nil, err
		}
		dataStatus := reservationDataStatus(reservation)
		observe.Info(ctx, "图书馆座位预约已取消",
			observe.StringAttr("reservation_status", reservation.Status),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(ReservationResult{DataStatus: dataStatus, Reservation: reservation})
	}
}

func mineReservationsHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request.UserID); err != nil {
			return nil, invalid(err)
		}
		var input MineRequest
		if err := decodeOptional(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, invalid(err)
		}
		observe.Debug(ctx, "开始查询我的图书馆座位预约",
			observe.StringAttr("app_id", request.AppID),
			observe.IntAttr("limit", input.Limit),
		)
		items, metadata, err := store.ListMyReservations(ctx, request.AppID, request.UserID, input, now())
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []Reservation{}
		}
		dataStatus := DataStatus{SnapshotMetadata: metadata}
		if metadata.Revision != "" {
			if governed, err := metadata.Govern(now()); err == nil {
				dataStatus = governed
			} else if metadata.Source == DemoSource {
				dataStatus.State = DataStateDemoNonAuthoritative
			} else {
				dataStatus.State = "reservation_local"
			}
		} else {
			dataStatus.State = "reservation_local"
			dataStatus.Source = DemoSource
			dataStatus.Authoritative = false
			dataStatus.Complete = true
		}
		observe.Info(ctx, "我的图书馆座位预约查询完成",
			observe.IntAttr("result_count", len(items)),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(MineResult{DataStatus: dataStatus, Reservations: items})
	}
}

func assembleSlotSeats(snapshot SeatSnapshot, limit int) []SlotSeats {
	result := make([]SlotSeats, 0, len(snapshot.Slots))
	remaining := limit
	for _, slot := range snapshot.Slots {
		item := SlotSeats{Slot: slot, Seats: []SeatAvailability{}}
		for _, seat := range snapshot.Seats {
			if remaining <= 0 {
				break
			}
			status := SeatStatusAvailable
			if snapshot.Occupied != nil {
				if occupied, ok := snapshot.Occupied[OccupancyKey(seat.ID, slot.ID)]; ok && occupied != "" {
					status = SeatStatusReserved
				}
			}
			item.Seats = append(item.Seats, SeatAvailability{
				SeatID: seat.ID, Label: seat.Label, Area: seat.Area, SlotID: slot.ID, Status: status,
			})
			remaining--
		}
		if item.Seats == nil {
			item.Seats = []SeatAvailability{}
		}
		result = append(result, item)
		if remaining <= 0 {
			break
		}
	}
	if result == nil {
		result = []SlotSeats{}
	}
	return result
}

func governCatalog(ctx context.Context, kind string, metadata SnapshotMetadata, now time.Time) (DataStatus, error) {
	dataStatus, err := metadata.Govern(now)
	if err != nil {
		errorCode := "data_incomplete"
		switch {
		case errors.Is(err, contracts.ErrDataUntrusted):
			errorCode = "data_non_authoritative"
		case errors.Is(err, contracts.ErrDataExpired):
			errorCode = "data_expired"
		case errors.Is(err, contracts.ErrDataUnavailable):
			errorCode = "data_unavailable"
		}
		observe.Warn(ctx, "图书馆座位目录未通过使用治理",
			observe.StringAttr("data_kind", kind),
			observe.StringAttr("source_revision", metadata.Revision),
			observe.BoolAttr("authoritative", metadata.Authoritative),
			observe.BoolAttr("complete", metadata.Complete),
			observe.StringAttr("error_code", errorCode),
		)
		return DataStatus{}, err
	}
	return dataStatus, nil
}

func reservationDataStatus(reservation Reservation) DataStatus {
	metadata := SnapshotMetadata{
		Revision: reservation.CatalogRevision, Source: reservation.CatalogSource,
		Authoritative: reservation.CatalogAuthoritative, Complete: true,
		ImportedAt: reservation.CreatedAt, ValidUntil: reservation.EndsAt.Add(time.Hour),
	}
	if reservation.CatalogSource == DemoSource || !reservation.CatalogAuthoritative {
		return DataStatus{State: DataStateDemoNonAuthoritative, SnapshotMetadata: metadata}
	}
	return DataStatus{State: DataStateAuthoritativeFresh, SnapshotMetadata: metadata}
}

func decodeOptional(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	return decodeRequired(payload, target)
}

func decodeRequired(payload json.RawMessage, target any) error {
	if err := jsonutil.DecodeStrict(payload, target); err != nil {
		return errors.Join(registry.ErrSchemaValidation, err)
	}
	return nil
}

func invalid(err error) error {
	return errors.Join(registry.ErrSchemaValidation, err)
}

func ensureStore(store Store) error {
	if store == nil {
		return fmt.Errorf("%w: library seat store", contracts.ErrDataUnavailable)
	}
	return nil
}

func marshalResult(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode library seat tool result: %w", err)
	}
	return encoded, nil
}
