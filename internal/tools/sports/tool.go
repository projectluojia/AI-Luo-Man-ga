package sports

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
	VenueListToolID         = "sports.venues.list"
	ProjectListToolID       = "sports.projects.list"
	SlotSearchToolID        = "sports.slots.search"
	ReservationCreateToolID = "sports.reservations.create"
	ReservationCancelToolID = "sports.reservations.cancel"
	ReservationMineToolID   = "sports.reservations.mine"
	OrdersWebViewToolID     = "sports.orders.webview"
	ScheduleAddToolID       = "sports.schedule.add"

	VenueListInputSchemaJSON         = `{"type":"object","additionalProperties":false}`
	ProjectListInputSchemaJSON       = `{"type":"object","properties":{"venue_id":{"type":"string","minLength":1,"maxLength":128}},"required":["venue_id"],"additionalProperties":false}`
	SlotSearchInputSchemaJSON        = `{"type":"object","properties":{"venue_id":{"type":"string","minLength":1,"maxLength":128},"project_id":{"type":"string","minLength":1,"maxLength":128},"date":{"type":"string","minLength":10,"maxLength":10}},"required":["venue_id","project_id","date"],"additionalProperties":false}`
	ReservationCreateInputSchemaJSON = `{"type":"object","properties":{"venue_id":{"type":"string","minLength":1,"maxLength":128},"project_id":{"type":"string","minLength":1,"maxLength":128},"slot_id":{"type":"string","minLength":1,"maxLength":128},"count":{"type":"integer","minimum":1,"maximum":16}},"required":["venue_id","project_id","slot_id"],"additionalProperties":false}`
	ReservationCancelInputSchemaJSON = `{"type":"object","properties":{"reservation_id":{"type":"string","minLength":1,"maxLength":128}},"required":["reservation_id"],"additionalProperties":false}`
	ReservationMineInputSchemaJSON   = `{"type":"object","additionalProperties":false}`
	OrdersWebViewInputSchemaJSON     = `{"type":"object","additionalProperties":false}`
	ScheduleAddInputSchemaJSON       = `{"type":"object","properties":{"reservation_id":{"type":"string","minLength":1,"maxLength":128}},"required":["reservation_id"],"additionalProperties":false}`
)

// ToolSpecs 返回运动场馆原子 Tool 规格。写操作要求确认与幂等。
func ToolSpecs() []registry.ToolSpec {
	read := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectRead}
	}
	writeConfirm := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectWrite, RequiresConfirmation: true}
	}
	return []registry.ToolSpec{
		read(VenueListToolID, "List sports venues from the current governed snapshot.", VenueListInputSchemaJSON),
		read(ProjectListToolID, "List sports projects under a venue.", ProjectListInputSchemaJSON),
		read(SlotSearchToolID, "Search sports reservation slots by venue, project and local date.", SlotSearchInputSchemaJSON),
		writeConfirm(ReservationCreateToolID, "Create a sports venue reservation. Over-quota requests are rejected.", ReservationCreateInputSchemaJSON),
		writeConfirm(ReservationCancelToolID, "Cancel a sports venue reservation owned by the current user.", ReservationCancelInputSchemaJSON),
		read(ReservationMineToolID, "List sports reservations owned by the current user.", ReservationMineInputSchemaJSON),
		read(OrdersWebViewToolID, "Return a governed sports order WebView session descriptor without secrets.", OrdersWebViewInputSchemaJSON),
		writeConfirm(ScheduleAddToolID, "Add a local schedule item for a sports reservation.", ScheduleAddInputSchemaJSON),
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
		VenueListToolID:         venueListHandler(store, now),
		ProjectListToolID:       projectListHandler(store, now),
		SlotSearchToolID:        slotSearchHandler(store, now),
		ReservationCreateToolID: createReservationHandler(store, now),
		ReservationCancelToolID: cancelReservationHandler(store, now),
		ReservationMineToolID:   mineReservationsHandler(store, now),
		OrdersWebViewToolID:     ordersWebViewHandler(store, now),
		ScheduleAddToolID:       addScheduleHandler(store, now),
	}
}

func ensureStore(store Store) error {
	if store == nil {
		return errors.New("sports store is unavailable")
	}
	return nil
}

func decode(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := jsonutil.DecodeStrict(payload, target); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func marshalResult(value any) (json.RawMessage, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode sports tool result: %w", err)
	}
	return result, nil
}

func governedCatalog(ctx context.Context, kind string, metadata SnapshotMetadata, now time.Time) (DataStatus, error) {
	dataStatus, err := metadata.Govern(now)
	if err != nil {
		logRejectedSnapshot(ctx, kind, metadata, err)
		return DataStatus{}, err
	}
	return dataStatus, nil
}

func requireAuthoritativeCatalog(ctx context.Context, store Store, appID string, now time.Time) (DataStatus, error) {
	snapshot, err := store.ListVenues(ctx, appID)
	if err != nil {
		return DataStatus{}, err
	}
	return governedCatalog(ctx, "catalog", snapshot.Metadata, now)
}

func logRejectedSnapshot(ctx context.Context, dataKind string, metadata SnapshotMetadata, err error) {
	errorCode := "data_incomplete"
	switch {
	case errors.Is(err, contracts.ErrDataUntrusted):
		errorCode = "data_non_authoritative"
	case errors.Is(err, contracts.ErrDataExpired):
		errorCode = "data_expired"
	case errors.Is(err, contracts.ErrQuotaExceeded):
		errorCode = "quota_exceeded"
	case errors.Is(err, contracts.ErrDelegatedAuthRequired):
		errorCode = "delegated_auth_required"
	}
	observe.Warn(ctx, "运动场馆数据快照未通过使用治理",
		observe.StringAttr("data_kind", dataKind),
		observe.StringAttr("source_revision", metadata.Revision),
		observe.BoolAttr("authoritative", metadata.Authoritative),
		observe.BoolAttr("complete", metadata.Complete),
		observe.StringAttr("error_code", errorCode),
	)
}

func venueListHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input VenueListRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		started := time.Now()
		observe.Debug(ctx, "开始查询运动场馆",
			observe.StringAttr("app_id", request.AppID),
		)
		snapshot, err := store.ListVenues(ctx, request.AppID)
		if err != nil {
			return nil, err
		}
		dataStatus, err := governedCatalog(ctx, "venues", snapshot.Metadata, now())
		if err != nil {
			return nil, err
		}
		venues := snapshot.Venues
		if venues == nil {
			venues = []Venue{}
		}
		observe.Info(ctx, "运动场馆查询完成",
			observe.IntAttr("result_count", len(venues)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(VenueListResult{DataStatus: dataStatus, Venues: venues})
	}
}

func projectListHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input ProjectListRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		started := time.Now()
		observe.Debug(ctx, "开始查询运动项目",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("venue_id", input.VenueID),
		)
		snapshot, err := store.ListProjects(ctx, request.AppID, input)
		if err != nil {
			return nil, err
		}
		dataStatus, err := governedCatalog(ctx, "projects", snapshot.Metadata, now())
		if err != nil {
			return nil, err
		}
		projects := snapshot.Projects
		if projects == nil {
			projects = []Project{}
		}
		observe.Info(ctx, "运动项目查询完成",
			observe.IntAttr("result_count", len(projects)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(ProjectListResult{DataStatus: dataStatus, Projects: projects})
	}
}

func slotSearchHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input SlotSearchRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		started := time.Now()
		observe.Debug(ctx, "开始查询运动时段",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("venue_id", input.VenueID),
			observe.StringAttr("project_id", input.ProjectID),
			observe.StringAttr("slot_date", input.Date),
		)
		snapshot, err := store.SearchSlots(ctx, request.AppID, input)
		if err != nil {
			return nil, err
		}
		dataStatus, err := governedCatalog(ctx, "slots", snapshot.Metadata, now())
		if err != nil {
			return nil, err
		}
		slots := snapshot.Slots
		if slots == nil {
			slots = []Slot{}
		}
		observe.Info(ctx, "运动时段查询完成",
			observe.IntAttr("result_count", len(slots)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(SlotSearchResult{DataStatus: dataStatus, Slots: slots})
	}
}

func createReservationHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		var input CreateReservationRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		started := time.Now()
		dataStatus, err := requireAuthoritativeCatalog(ctx, store, request.AppID, now())
		if err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始创建运动场馆预约",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("venue_id", input.VenueID),
			observe.StringAttr("project_id", input.ProjectID),
			observe.StringAttr("slot_id", input.SlotID),
			observe.IntAttr("count", input.Count),
		)
		reservation, metadata, err := store.CreateReservation(ctx, CreateReservationInput{
			AppID: request.AppID, UserID: request.UserID,
			VenueID: input.VenueID, ProjectID: input.ProjectID, SlotID: input.SlotID,
			Count: input.Count, Now: now(), ExpectedRevision: dataStatus.Revision,
		})
		if err != nil {
			if errors.Is(err, ErrOverQuota) {
				logRejectedSnapshot(ctx, "reservation_create", metadata, err)
			}
			return nil, err
		}
		observe.Info(ctx, "运动场馆预约已确认",
			observe.StringAttr("reservation_id", reservation.ID),
			observe.StringAttr("status", reservation.Status),
			observe.IntAttr("count", reservation.Count),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.Duration(started),
		)
		return marshalResult(ReservationResult{DataStatus: dataStatus, Reservation: reservation})
	}
}

func cancelReservationHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		var input CancelReservationRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		started := time.Now()
		dataStatus, err := requireAuthoritativeCatalog(ctx, store, request.AppID, now())
		if err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始取消运动场馆预约",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("reservation_id", input.ReservationID),
		)
		reservation, _, err := store.CancelReservation(ctx, CancelReservationInput{
			AppID: request.AppID, UserID: request.UserID, ReservationID: input.ReservationID, Now: now(),
		})
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "运动场馆预约已取消",
			observe.StringAttr("reservation_id", reservation.ID),
			observe.StringAttr("status", reservation.Status),
			observe.Duration(started),
		)
		return marshalResult(ReservationResult{DataStatus: dataStatus, Reservation: reservation})
	}
}

func mineReservationsHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		var input struct{}
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		started := time.Now()
		dataStatus, err := requireAuthoritativeCatalog(ctx, store, request.AppID, now())
		if err != nil {
			return nil, err
		}
		snapshot, err := store.ListMyReservations(ctx, request.AppID, request.UserID, now())
		if err != nil {
			return nil, err
		}
		items := snapshot.Reservations
		if items == nil {
			items = []Reservation{}
		}
		observe.Info(ctx, "运动场馆预约列表查询完成",
			observe.IntAttr("result_count", len(items)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.Duration(started),
		)
		return marshalResult(ReservationListResult{DataStatus: dataStatus, Reservations: items})
	}
}

func ordersWebViewHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		var input struct{}
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		started := time.Now()
		snapshot, err := store.GetWebViewDescriptor(ctx, request.AppID)
		if err != nil {
			return nil, err
		}
		dataStatus, err := snapshot.Metadata.DemoStatus(now())
		if err != nil {
			logRejectedSnapshot(ctx, "orders_webview", snapshot.Metadata, err)
			return nil, err
		}
		if dataStatus.Authoritative {
			observe.Warn(ctx, "权威订单 WebView 缺少委托授权，已拒绝",
				observe.StringAttr("error_code", "delegated_auth_required"),
				observe.BoolAttr("requires_delegated_auth", snapshot.Descriptor.RequiresDelegatedAuth),
			)
			return nil, ErrDelegatedAuthRequired
		}
		headers := snapshot.Descriptor.RequiredHeaders
		if headers == nil {
			headers = []RequiredHeader{}
		}
		observe.Info(ctx, "已返回非权威运动订单 WebView 会话描述符",
			observe.StringAttr("data_state", dataStatus.State),
			observe.BoolAttr("requires_delegated_auth", snapshot.Descriptor.RequiresDelegatedAuth),
			observe.IntAttr("required_header_count", len(headers)),
			observe.Duration(started),
		)
		return marshalResult(WebViewResult{
			DataStatus:            dataStatus,
			EntryURL:              snapshot.Descriptor.EntryURL,
			RequiredUserAgent:     snapshot.Descriptor.RequiredUserAgent,
			RequiredHeaders:       headers,
			RequiresDelegatedAuth: snapshot.Descriptor.RequiresDelegatedAuth,
		})
	}
}

func addScheduleHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		var input AddScheduleRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, err
		}
		started := time.Now()
		dataStatus, err := requireAuthoritativeCatalog(ctx, store, request.AppID, now())
		if err != nil {
			return nil, err
		}
		item, _, err := store.AddScheduleItem(ctx, AddScheduleInput{
			AppID: request.AppID, UserID: request.UserID, ReservationID: input.ReservationID, Now: now(),
		})
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "运动预约已加入本地日程",
			observe.StringAttr("schedule_id", item.ID),
			observe.StringAttr("reservation_id", item.ReservationID),
			observe.Duration(started),
		)
		return marshalResult(ScheduleResult{DataStatus: dataStatus, Schedule: item})
	}
}

// SensitiveWebViewKeys 是 WebView 描述符禁止出现的密钥字段名，供测试复用。
func SensitiveWebViewKeys() []string {
	return []string{"cookie", "cookies", "authorization", "token", "access_token", "student_id", "student_no", "studentworkno"}
}

func ContainsSensitiveWebViewKey(raw json.RawMessage) bool {
	lower := strings.ToLower(string(raw))
	for _, key := range SensitiveWebViewKeys() {
		if strings.Contains(lower, `"`+key+`"`) {
			return true
		}
	}
	return false
}
