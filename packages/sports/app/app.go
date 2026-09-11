// Package app 是运动场馆预约包的协议层：8 个 Capability 的载荷分发。
// 目录/时段/WebView 读取走系统作用域快照治理（guestkit.GovernedSnapshot），
// 预约与日程写走个人作用域文档操作（宿主从治理上下文注入 UserID）。
//
// 错误面与 classroom/library 同型：ok:false 只保留 invalid_argument 与数据
// 治理闭式错误码；not_found/conflict 等业务失败在 ok:true 的结果载荷内表达。
package app

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/sports/sports"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const (
	capVenuesList         = "sports.venues.list"
	capProjectsList       = "sports.projects.list"
	capSlotsSearch        = "sports.slots.search"
	capReservationsCreate = "sports.reservations.create"
	capReservationsCancel = "sports.reservations.cancel"
	capReservationsMine   = "sports.reservations.mine"
	capOrdersWebview      = "sports.orders.webview"
	capScheduleAdd        = "sports.schedule.add"
)

var errInvalidArgument = guestkit.ErrInvalidArgument

// NewDispatcher 构造分发器：以 guestkit 共享 Dispatcher 分发（唯一实现，
// 包内只声明能力分发表）。store 为 guestkit.StoreClient（wasip1 内）或测试替身。
func NewDispatcher(client guestkit.Store) *guestkit.Dispatcher {
	return guestkit.NewDispatcher(Handlers(client))
}

// dispatcher 持有存储端口；处理函数由 Handlers 注册到 guestkit.Dispatcher。
type dispatcher struct {
	store guestkit.Store
}

// Handlers 返回能力分发表（src/main.go 装配 guestkit.Run 用）。
func Handlers(client guestkit.Store) map[string]func(json.RawMessage) (any, error) {
	d := &dispatcher{store: client}
	return map[string]func(json.RawMessage) (any, error){
		capVenuesList:         func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listVenues) },
		capProjectsList:       func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listProjects) },
		capSlotsSearch:        func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.searchSlots) },
		capReservationsCreate: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.createReservation) },
		capReservationsCancel: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.cancelReservation) },
		capReservationsMine:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listMyReservations) },
		capOrdersWebview:      func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.ordersWebview) },
		capScheduleAdd:        func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.addSchedule) },
	}
}

// ---- 快照读取（系统作用域） ----

type venuesListResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Venues     []sports.Venue      `json:"venues"`
}

type projectsListResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Projects   []sports.Project    `json:"projects"`
}

type slotsSearchResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Slots      []sports.Slot       `json:"slots"`
}

type webviewResult struct {
	DataStatus            guestkit.DataStatus     `json:"data_status"`
	EntryURL              string                  `json:"entry_url"`
	RequiredUserAgent     string                  `json:"required_user_agent"`
	RequiredHeaders       []sports.RequiredHeader `json:"required_headers"`
	RequiresDelegatedAuth bool                    `json:"requires_delegated_auth"`
}

func (d *dispatcher) listVenues(request sports.VenuesListRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[sports.Venue](d.store, sports.VenuesCollection)
	if err != nil {
		return nil, err
	}
	venues := make([]sports.Venue, 0, len(snapshot.Documents))
	venues = append(venues, snapshot.Documents...)
	sort.Slice(venues, func(i, j int) bool { return venues[i].ID < venues[j].ID })
	if len(venues) > request.Limit {
		venues = venues[:request.Limit]
	}
	return venuesListResult{DataStatus: dataStatus, Venues: venues}, nil
}

func (d *dispatcher) listProjects(request sports.ProjectsListRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[sports.Project](d.store, sports.ProjectsCollection)
	if err != nil {
		return nil, err
	}
	projects := make([]sports.Project, 0, len(snapshot.Documents))
	for _, project := range snapshot.Documents {
		if project.VenueID == request.VenueID {
			projects = append(projects, project)
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	if len(projects) > request.Limit {
		projects = projects[:request.Limit]
	}
	return projectsListResult{DataStatus: dataStatus, Projects: projects}, nil
}

func (d *dispatcher) searchSlots(request sports.SlotSearchRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[sports.Slot](d.store, sports.SlotsCollection)
	if err != nil {
		return nil, err
	}
	slots := make([]sports.Slot, 0, len(snapshot.Documents))
	for _, slot := range snapshot.Documents {
		if slot.VenueID == request.VenueID && slot.ProjectID == request.ProjectID && slot.Date == request.Date {
			slots = append(slots, slot)
		}
	}
	sort.Slice(slots, func(i, j int) bool {
		if !slots[i].StartAt.Equal(slots[j].StartAt) {
			return slots[i].StartAt.Before(slots[j].StartAt)
		}
		return slots[i].ID < slots[j].ID
	})
	if len(slots) > request.Limit {
		slots = slots[:request.Limit]
	}
	return slotsSearchResult{DataStatus: dataStatus, Slots: slots}, nil
}

func (d *dispatcher) ordersWebview(_ sports.VenuesListRequest) (any, error) {
	// 描述符以固定 ID 存放（每快照一份）；载荷治理与目录同规则。
	raw, found, err := d.store.Get(guestkit.ScopeSystem, sports.WebviewCollection, "descriptor")
	if err != nil || !found {
		return nil, guestkit.ErrDataUnavailable
	}
	var descriptor sports.WebViewDescriptor
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		return nil, guestkit.ErrDataUnavailable
	}
	snapshot, _, err := guestkit.GovernedSnapshot[sports.Venue](d.store, sports.VenuesCollection)
	if err != nil {
		return nil, err
	}
	dataStatus, err := guestkit.GovernStatus(snapshot.Meta)
	if err != nil {
		return nil, err
	}
	descriptor.SourceRevision = snapshot.Meta.SourceRevision
	normalized, err := sports.NormalizeWebViewDescriptor(descriptor)
	if err != nil {
		return nil, errInvalidArgument
	}
	headers := normalized.RequiredHeaders
	if headers == nil {
		headers = []sports.RequiredHeader{}
	}
	return webviewResult{
		DataStatus: dataStatus, EntryURL: normalized.EntryURL,
		RequiredUserAgent: normalized.RequiredUserAgent, RequiredHeaders: headers,
		RequiresDelegatedAuth: normalized.RequiresDelegatedAuth,
	}, nil
}

// ---- 个人预约与日程（个人作用域，写路径） ----

type reservationResult struct {
	Reservation sports.Reservation `json:"reservation"`
}

type reservationsResult struct {
	Reservations []sports.Reservation `json:"reservations"`
}

type scheduleResult struct {
	Schedule sports.ScheduleItem `json:"schedule"`
}

func (d *dispatcher) createReservation(request sports.ReservationCreateRequest) (any, error) {
	// 场馆/项目/时段必须存在于当前权威快照，且时段尚未结束。
	venue, err := d.governedVenue(request.VenueID)
	if err != nil {
		return nil, err
	}
	if venue == nil {
		return guestkit.NotFound{}, nil
	}
	project, err := d.governedProject(request.VenueID, request.ProjectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return guestkit.NotFound{}, nil
	}
	slot, err := d.governedSlot(request.VenueID, request.ProjectID, request.SlotID)
	if err != nil {
		return nil, err
	}
	if slot == nil {
		return guestkit.NotFound{}, nil
	}
	now := time.Now().UTC()
	if !slot.EndAt.After(now) {
		// 已结束的时段不可预约（fail-closed，拒绝过期的占用承诺）。
		return nil, guestkit.ErrDataExpired
	}
	// 剩余配额由导入方维护在快照 remaining_quota 中（预约生效时扣减）。
	if request.Count > slot.RemainingQuota {
		return guestkit.Conflict{Conflict: "quota_exceeded"}, nil
	}
	existing, err := d.listAllMyReservations()
	if err != nil {
		return nil, err
	}
	for _, item := range existing {
		if item.EffectiveStatus(now) == sports.StatusConfirmed &&
			item.VenueID == request.VenueID && item.ProjectID == request.ProjectID && item.SlotID == request.SlotID {
			// 本人已占用同一时段（幂等重复请求按带内冲突应答）。
			return guestkit.Conflict{Conflict: "slot_conflict"}, nil
		}
	}
	item := sports.Reservation{
		ReservationID: guestkit.NewDocID("sr"), VenueID: venue.ID, VenueName: venue.Name,
		ProjectID: project.ID, ProjectName: project.Name, SlotID: slot.ID,
		Count: request.Count, Status: sports.StatusConfirmed,
		StartsAt: slot.StartAt.UTC().Format(time.RFC3339), EndsAt: slot.EndAt.UTC().Format(time.RFC3339),
		CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(sports.ReservationsCollection, item.ReservationID, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	return reservationResult{Reservation: item}, nil
}

func (d *dispatcher) cancelReservation(request sports.ReservationCancelRequest) (any, error) {
	payload, found, err := d.store.Get(guestkit.ScopeUser, sports.ReservationsCollection, request.ReservationID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if !found {
		return guestkit.NotFound{}, nil
	}
	var item sports.Reservation
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, guestkit.ErrInternal
	}
	switch item.Status {
	case sports.StatusCancelled:
		return guestkit.Conflict{Conflict: request.ReservationID}, nil
	case sports.StatusConfirmed:
	default:
		return nil, errInvalidArgument
	}
	now := time.Now().UTC()
	// 已结束的时段不可取消（状态机由 EffectiveStatus 推导为 expired）。
	if item.EffectiveStatus(now) == sports.StatusExpired {
		return nil, errInvalidArgument
	}
	item.Status = sports.StatusCancelled
	item.UpdatedAt = now.Format(time.RFC3339Nano)
	item.CancelledAt = now.Format(time.RFC3339Nano)
	updated, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(sports.ReservationsCollection, item.ReservationID, updated); err != nil {
		return nil, guestkit.ErrInternal
	}
	return reservationResult{Reservation: item}, nil
}

func (d *dispatcher) listMyReservations(request sports.ReservationListRequest) (any, error) {
	items, err := d.listAllMyReservations()
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ReservationID < items[j].ReservationID
	})
	if len(items) > request.Limit {
		items = items[:request.Limit]
	}
	return reservationsResult{Reservations: items}, nil
}

func (d *dispatcher) listAllMyReservations() ([]sports.Reservation, error) {
	// UserCollection 分页遍历全集合，避免宿主单页上限截断——配额与
	// 双重预约检查依赖全量视图，截断会放过超额/撞车写入。
	return guestkit.UserCollection[sports.Reservation](userLister{client: d.store}, sports.ReservationsCollection, nil)
}

// userLister 把 guestkit.Store 投影为固定个人作用域的 Lister；集合遍历
// 辅助函数一律经该端口工作，禁止本包手写分页循环。
type userLister struct {
	client guestkit.Store
}

func (u userLister) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	return u.client.List(guestkit.ScopeUser, collection, limit, afterID)
}

func (d *dispatcher) addSchedule(request sports.ScheduleAddRequest) (any, error) {
	payload, found, err := d.store.Get(guestkit.ScopeUser, sports.ReservationsCollection, request.ReservationID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if !found {
		return guestkit.NotFound{}, nil
	}
	var item sports.Reservation
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, guestkit.ErrInternal
	}
	now := time.Now().UTC()
	status := item.EffectiveStatus(now)
	if status != sports.StatusConfirmed && status != sports.StatusExpired {
		// 已取消的预约不再建日程。
		return nil, errInvalidArgument
	}
	// 每预约至多一条日程（幂等：重复添加返回既有日程）。查重走全量分页
	// 读取，避免宿主单页上限截断后重复建日程。
	schedules, err := guestkit.UserCollection[sports.ScheduleItem](userLister{client: d.store}, sports.ScheduleCollection, nil)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	for _, existing := range schedules {
		if existing.ReservationID == request.ReservationID {
			return scheduleResult{Schedule: existing}, nil
		}
	}
	title := buildScheduleTitle(item.VenueName, item.ProjectName)
	entry := sports.ScheduleItem{
		ScheduleID: guestkit.NewDocID("ss"), ReservationID: item.ReservationID, Title: title,
		StartsAt: item.StartsAt, EndsAt: item.EndsAt, CreatedAt: now.Format(time.RFC3339Nano),
	}
	created, err := json.Marshal(entry)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(sports.ScheduleCollection, entry.ScheduleID, created); err != nil {
		return nil, guestkit.ErrInternal
	}
	return scheduleResult{Schedule: entry}, nil
}

// buildScheduleTitle 由场馆与项目名拼日程标题，超限截断。
func buildScheduleTitle(venueName, projectName string) string {
	title := strings.TrimSpace(venueName + " " + projectName)
	if title == "" {
		title = "运动场馆预约"
	}
	runes := []rune(title)
	if len(runes) > sports.MaxTextChars {
		title = string(runes[:sports.MaxTextChars])
	}
	return title
}

// ---- 快照定位辅助 ----

func (d *dispatcher) governedVenue(venueID string) (*sports.Venue, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[sports.Venue](d.store, sports.VenuesCollection)
	if err != nil {
		return nil, err
	}
	return guestkit.FindSnapshotDoc(snapshot, func(item *sports.Venue) bool { return item.ID == venueID }), nil
}

func (d *dispatcher) governedProject(venueID, projectID string) (*sports.Project, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[sports.Project](d.store, sports.ProjectsCollection)
	if err != nil {
		return nil, err
	}
	return guestkit.FindSnapshotDoc(snapshot, func(item *sports.Project) bool {
		return item.ID == projectID && item.VenueID == venueID
	}), nil
}

func (d *dispatcher) governedSlot(venueID, projectID, slotID string) (*sports.Slot, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[sports.Slot](d.store, sports.SlotsCollection)
	if err != nil {
		return nil, err
	}
	return guestkit.FindSnapshotDoc(snapshot, func(item *sports.Slot) bool {
		return item.ID == slotID && item.ProjectID == projectID && item.VenueID == venueID
	}), nil
}
