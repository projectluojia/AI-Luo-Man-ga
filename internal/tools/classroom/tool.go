package classroom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

const (
	RoomsSearchToolID    = "classroom.rooms.search"
	CampusesListToolID   = "classroom.campuses.list"
	BuildingsListToolID  = "classroom.buildings.list"
	ScheduleCreateToolID = "classroom.schedule.create"
	ScheduleListToolID   = "classroom.schedule.list"
	ScheduleCancelToolID = "classroom.schedule.cancel"

	RoomsSearchInputSchemaJSON    = `{"type":"object","properties":{"date":{"type":"string","format":"date"},"campus_id":{"type":"string","minLength":1,"maxLength":128},"building_id":{"type":"string","minLength":1,"maxLength":128},"period":{"type":"integer","minimum":1,"maximum":13},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["date","campus_id","period"],"additionalProperties":false}`
	CampusesListInputSchemaJSON   = `{"type":"object","additionalProperties":false}`
	BuildingsListInputSchemaJSON  = `{"type":"object","properties":{"campus_id":{"type":"string","minLength":1,"maxLength":128}},"required":["campus_id"],"additionalProperties":false}`
	ScheduleCreateInputSchemaJSON = `{"type":"object","properties":{"schedule_id":{"type":"string","minLength":1,"maxLength":128},"room_id":{"type":"string","minLength":1,"maxLength":128},"date":{"type":"string","format":"date"},"period":{"type":"integer","minimum":1,"maximum":13},"title":{"type":"string","minLength":1,"maxLength":256}},"required":["room_id","date","period"],"additionalProperties":false}`
	ScheduleListInputSchemaJSON   = `{"type":"object","properties":{"status":{"type":"string","enum":["scheduled","cancelled"]},"date":{"type":"string","format":"date"}},"additionalProperties":false}`
	ScheduleCancelInputSchemaJSON = `{"type":"object","properties":{"schedule_id":{"type":"string","minLength":1,"maxLength":128}},"required":["schedule_id"],"additionalProperties":false}`
)

// ToolSpecs 返回教室原子 Tool 规格。写操作中创建/取消日程要求显式确认且幂等。
func ToolSpecs() []registry.ToolSpec {
	read := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectRead}
	}
	writeConfirm := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectWrite, RequiresConfirmation: true}
	}
	return []registry.ToolSpec{
		read(RoomsSearchToolID, "Search empty classrooms by academic date, campus, optional building and period.", RoomsSearchInputSchemaJSON),
		read(CampusesListToolID, "List campuses in the current classroom snapshot.", CampusesListInputSchemaJSON),
		read(BuildingsListToolID, "List buildings for a campus in the current classroom snapshot.", BuildingsListInputSchemaJSON),
		writeConfirm(ScheduleCreateToolID, "Create a local classroom schedule item for the current user.", ScheduleCreateInputSchemaJSON),
		read(ScheduleListToolID, "List classroom schedule items owned by the current user.", ScheduleListInputSchemaJSON),
		writeConfirm(ScheduleCancelToolID, "Cancel a classroom schedule item owned by the current user.", ScheduleCancelInputSchemaJSON),
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

// ToolHandlers 构造原子 Tool 处理器。now 为空时使用 time.Now。
func ToolHandlers(store Store, now func() time.Time) map[string]registry.Handler {
	if now == nil {
		now = time.Now
	}
	return map[string]registry.Handler{
		RoomsSearchToolID:    roomsSearchHandler(store, now),
		CampusesListToolID:   campusesListHandler(store, now),
		BuildingsListToolID:  buildingsListHandler(store, now),
		ScheduleCreateToolID: scheduleCreateHandler(store, now),
		ScheduleListToolID:   scheduleListHandler(store),
		ScheduleCancelToolID: scheduleCancelHandler(store, now),
	}
}

func roomsSearchHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input SearchRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		observe.Debug(ctx, "开始查询空闲教室",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("campus_id", input.CampusID),
			observe.StringAttr("building_id", input.BuildingID),
			observe.IntAttr("period", input.Period),
			observe.IntAttr("limit", input.Limit),
		)
		snapshot, err := store.SearchRooms(ctx, request.AppID, input)
		if err != nil {
			return nil, fmt.Errorf("search empty classrooms: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "rooms", snapshot.Metadata, now())
		if err != nil {
			return nil, fmt.Errorf("govern classroom room snapshot: %w", err)
		}
		rooms := snapshot.Rooms
		for _, room := range rooms {
			if room.SourceRevision != snapshot.Metadata.Revision {
				return nil, fmt.Errorf("classroom room snapshot revision mismatch: %w", contracts.ErrDataIncomplete)
			}
		}
		if rooms == nil {
			rooms = []Room{}
		}
		observe.Info(ctx, "空闲教室查询完成",
			observe.IntAttr("result_count", len(rooms)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.StringAttr("valid_until", dataStatus.ValidUntil.Format(time.RFC3339)),
			observe.Duration(started),
		)
		return marshalResult(SearchResult{DataStatus: dataStatus, Rooms: rooms})
	}
}

func campusesListHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input struct{}
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		observe.Debug(ctx, "开始列出教室校区", observe.StringAttr("app_id", request.AppID))
		snapshot, err := store.ListCampuses(ctx, request.AppID)
		if err != nil {
			return nil, fmt.Errorf("list classroom campuses: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "campuses", snapshot.Metadata, now())
		if err != nil {
			return nil, fmt.Errorf("govern classroom campus snapshot: %w", err)
		}
		campuses := snapshot.Campuses
		if campuses == nil {
			campuses = []Campus{}
		}
		observe.Info(ctx, "教室校区列表查询完成",
			observe.IntAttr("result_count", len(campuses)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(CampusListResult{DataStatus: dataStatus, Campuses: campuses})
	}
}

func buildingsListHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input BuildingListRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		observe.Debug(ctx, "开始列出教学楼",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("campus_id", input.CampusID),
		)
		snapshot, err := store.ListBuildings(ctx, request.AppID, input)
		if err != nil {
			return nil, fmt.Errorf("list classroom buildings: %w", err)
		}
		dataStatus, err := governedStatus(ctx, "buildings", snapshot.Metadata, now())
		if err != nil {
			return nil, fmt.Errorf("govern classroom building snapshot: %w", err)
		}
		buildings := snapshot.Buildings
		if buildings == nil {
			buildings = []Building{}
		}
		observe.Info(ctx, "教学楼列表查询完成",
			observe.IntAttr("result_count", len(buildings)),
			observe.StringAttr("source_revision", dataStatus.Revision),
			observe.StringAttr("data_state", dataStatus.State),
			observe.Duration(started),
		)
		return marshalResult(BuildingListResult{DataStatus: dataStatus, Buildings: buildings})
	}
}

func scheduleCreateHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input ScheduleCreateRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		room, metadata, err := store.GetRoom(ctx, request.AppID, input.RoomID)
		if err != nil {
			return nil, err
		}
		if _, err := governedStatus(ctx, "schedule_create", metadata, now()); err != nil {
			return nil, fmt.Errorf("govern classroom snapshot for schedule: %w", err)
		}
		if input.ScheduleID == "" {
			input.ScheduleID = uuid.NewString()
		}
		title := input.Title
		if title == "" {
			title = fmt.Sprintf("自习 %s 第%d节", room.Name, input.Period)
		}
		item, err := store.CreateSchedule(ctx, ScheduleItem{
			AppID:          request.AppID,
			UserID:         request.UserID,
			ID:             input.ScheduleID,
			RoomID:         room.ID,
			CampusID:       room.CampusID,
			BuildingID:     room.BuildingID,
			RoomName:       room.Name,
			AcademicDate:   input.Date,
			Period:         input.Period,
			Title:          title,
			Status:         ScheduleStatusScheduled,
			SourceRevision: metadata.Revision,
		})
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "教室日程事项已创建",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("schedule_id", item.ID),
			observe.StringAttr("room_id", item.RoomID),
			observe.IntAttr("period", item.Period),
			observe.Duration(started),
		)
		return marshalResult(ScheduleCreateResult{Schedule: publicSchedule(item)})
	}
}

func scheduleListHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input ScheduleListRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		items, err := store.ListSchedules(ctx, request.AppID, request.UserID, input)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []ScheduleItem{}
		}
		public := make([]ScheduleItem, 0, len(items))
		for _, item := range items {
			public = append(public, publicSchedule(item))
		}
		observe.Info(ctx, "教室日程事项列表查询完成",
			observe.StringAttr("app_id", request.AppID),
			observe.IntAttr("result_count", len(public)),
			observe.Duration(started),
		)
		return marshalResult(ScheduleListResult{Schedules: public})
	}
}

func scheduleCancelHandler(store Store, now func() time.Time) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		started := time.Now()
		if err := RequireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input ScheduleCancelRequest
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := input.NormalizeAndValidate(); err != nil {
			return nil, errors.Join(registry.ErrSchemaValidation, err)
		}
		item, err := store.CancelSchedule(ctx, request.AppID, request.UserID, input.ScheduleID, now())
		if err != nil {
			return nil, err
		}
		observe.Info(ctx, "教室日程事项已取消",
			observe.StringAttr("app_id", request.AppID),
			observe.StringAttr("schedule_id", item.ID),
			observe.StringAttr("status", item.Status),
			observe.Duration(started),
		)
		return marshalResult(ScheduleCancelResult{Schedule: publicSchedule(item)})
	}
}

func governedStatus(ctx context.Context, kind string, metadata SnapshotMetadata, now time.Time) (DataStatus, error) {
	dataStatus, err := metadata.Govern(now)
	if err != nil {
		logRejectedSnapshot(ctx, kind, metadata, err)
		return DataStatus{}, err
	}
	return dataStatus, nil
}

func logRejectedSnapshot(ctx context.Context, dataKind string, metadata SnapshotMetadata, err error) {
	errorCode := "data_incomplete"
	switch {
	case errors.Is(err, contracts.ErrDataUntrusted):
		errorCode = "data_non_authoritative"
	case errors.Is(err, contracts.ErrDataExpired):
		errorCode = "data_expired"
	case errors.Is(err, contracts.ErrDataUnavailable):
		errorCode = "data_unavailable"
	}
	observe.Warn(ctx, "教室数据快照未通过使用治理",
		observe.StringAttr("data_kind", dataKind),
		observe.StringAttr("source_revision", metadata.Revision),
		observe.BoolAttr("authoritative", metadata.Authoritative),
		observe.BoolAttr("complete", metadata.Complete),
		observe.StringAttr("error_code", errorCode),
	)
}

func decode(payload json.RawMessage, target any) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if err := jsonutil.DecodeStrict(payload, target); err != nil {
		return errors.Join(registry.ErrSchemaValidation, err)
	}
	return nil
}

func marshalResult(value any) (json.RawMessage, error) {
	result, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode classroom tool result: %w", err)
	}
	return result, nil
}

func ensureStore(store Store) error {
	if store == nil {
		return ErrStoreUnavailable
	}
	return nil
}

// publicSchedule 去掉内部隔离字段，避免把 app_id/user_id 泄漏到 Capability 结果。
func publicSchedule(item ScheduleItem) ScheduleItem {
	item.AppID = ""
	item.UserID = ""
	return item
}
