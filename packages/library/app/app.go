// Package app 是图书馆座位预约包的协议层：5 个 Capability 的载荷分发。
// 目录读取走系统作用域快照治理（guestkit.GovernedSnapshot），预约写入走个人
// 作用域文档操作（宿主从治理上下文注入 UserID，guest 不可指定）。
//
// 错误面与 classroom/sports 同型：ok:false 只保留 invalid_argument 与数据治理
// 闭式错误码；not_found/conflict 等业务失败在 ok:true 的结果载荷内表达。
package app

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/library/library"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const (
	capSpacesList         = "library.spaces.list"
	capSlotsSearch        = "library.slots.search"
	capReservationsCreate = "library.reservations.create"
	capReservationsCancel = "library.reservations.cancel"
	capReservationsMine   = "library.reservations.mine"
)

var errInvalidArgument = guestkit.ErrInvalidArgument

// dispatcher 持有存储端口；处理函数由 Handlers 注册到 guestkit.Dispatcher。
type dispatcher struct {
	store guestkit.Store
}

// Handlers 返回能力分发表（src/main.go 装配 guestkit.Run 用）。
func Handlers(client guestkit.Store) map[string]func(json.RawMessage) (any, error) {
	d := &dispatcher{store: client}
	return map[string]func(json.RawMessage) (any, error){
		capSpacesList:         func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listSpaces) },
		capSlotsSearch:        func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.searchSlots) },
		capReservationsCreate: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.createReservation) },
		capReservationsCancel: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.cancelReservation) },
		capReservationsMine:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listMyReservations) },
	}
}

// ---- 快照读取（系统作用域） ----

type spacesListResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Spaces     []library.Space     `json:"spaces"`
}

type slotSeats struct {
	SeatID string `json:"seat_id"`
	Label  string `json:"label"`
	Area   string `json:"area,omitempty"`
	SlotID string `json:"slot_id"`
	Status string `json:"status"`
}

type slotSeatsGroup struct {
	Slot  library.Slot `json:"slot"`
	Seats []slotSeats  `json:"seats"`
}

type slotsSearchResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	SpaceID    string              `json:"space_id"`
	Date       string              `json:"date"`
	Slots      []slotSeatsGroup    `json:"slots"`
}

func (d *dispatcher) listSpaces(request library.SpacesListRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[library.Space](d.store, library.SpacesCollection)
	if err != nil {
		return nil, err
	}
	spaces := make([]library.Space, 0, len(snapshot.Documents))
	spaces = append(spaces, snapshot.Documents...)
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].ID < spaces[j].ID })
	if len(spaces) > request.Limit {
		spaces = spaces[:request.Limit]
	}
	return spacesListResult{DataStatus: dataStatus, Spaces: spaces}, nil
}

func (d *dispatcher) searchSlots(request library.SlotSearchRequest) (any, error) {
	spaces, dataStatus, err := guestkit.GovernedSnapshot[library.Space](d.store, library.SpacesCollection)
	if err != nil {
		return nil, err
	}
	space := guestkit.FindSnapshotDoc(spaces, func(item *library.Space) bool { return item.ID == request.SpaceID })
	if space == nil {
		// 未知空间按带内空集应答，不泄露目录内容。
		return slotsSearchResult{DataStatus: dataStatus, SpaceID: request.SpaceID, Date: request.Date, Slots: []slotSeatsGroup{}}, nil
	}
	seats, _, err := guestkit.GovernedSnapshot[library.Seat](d.store, library.SeatsCollection)
	if err != nil {
		return nil, err
	}
	spaceSeats := make([]library.Seat, 0, len(seats.Documents))
	for _, seat := range seats.Documents {
		if seat.SpaceID == request.SpaceID {
			spaceSeats = append(spaceSeats, seat)
		}
	}
	sort.Slice(spaceSeats, func(i, j int) bool { return spaceSeats[i].ID < spaceSeats[j].ID })
	slots, _, err := guestkit.GovernedSnapshot[library.Slot](d.store, library.SlotsCollection)
	if err != nil {
		return nil, err
	}
	occupied, err := d.occupancySet(request.Date)
	if err != nil {
		return nil, err
	}
	groups := make([]slotSeatsGroup, 0, len(slots.Documents))
	remaining := request.Limit
	for _, slot := range slots.Documents {
		if request.SlotID != "" && slot.ID != request.SlotID {
			continue
		}
		group := slotSeatsGroup{Slot: slot, Seats: []slotSeats{}}
		for _, seat := range spaceSeats {
			if remaining <= 0 {
				break
			}
			status := library.SeatAvailable
			if occupied[seat.ID+"\x1f"+slot.ID] {
				status = library.SeatReserved
			}
			group.Seats = append(group.Seats, slotSeats{
				SeatID: seat.ID, Label: seat.Label, Area: seat.Area, SlotID: slot.ID, Status: status,
			})
			remaining--
		}
		groups = append(groups, group)
		if remaining <= 0 {
			break
		}
	}
	return slotsSearchResult{DataStatus: dataStatus, SpaceID: request.SpaceID, Date: request.Date, Slots: groups}, nil
}

// occupancySet 载入指定日期的占用键集合（seat_id \x1f slot_id）。
func (d *dispatcher) occupancySet(date string) (map[string]bool, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[library.Occupancy](d.store, library.OccupancyCollection)
	if err != nil {
		return nil, err
	}
	occupied := make(map[string]bool)
	for _, item := range snapshot.Documents {
		if item.Date == date {
			occupied[item.SeatID+"\x1f"+item.SlotID] = true
		}
	}
	return occupied, nil
}

// ---- 个人预约（个人作用域，写路径） ----

type reservationResult struct {
	Reservation library.Reservation `json:"reservation"`
}

type reservationsResult struct {
	Reservations []library.Reservation `json:"reservations"`
}

func (d *dispatcher) createReservation(request library.ReservationCreateRequest) (any, error) {
	// 空间/座位/时段必须存在于当前权威快照，且时段尚未结束。
	space, _, err := d.governedSpace(request.SpaceID)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return guestkit.NotFound{}, nil
	}
	seat, err := d.governedSeat(request.SpaceID, request.SeatID)
	if err != nil {
		return nil, err
	}
	if seat == nil {
		return guestkit.NotFound{}, nil
	}
	slot, err := d.governedSlot(request.SlotID)
	if err != nil {
		return nil, err
	}
	if slot == nil {
		return guestkit.NotFound{}, nil
	}
	startsAt, endsAt, err := library.SlotBounds(request.Date, *slot)
	if err != nil {
		return nil, errInvalidArgument
	}
	now := time.Now().UTC()
	if !now.Before(endsAt) {
		// 已结束的时段不可预约（fail-closed，拒绝过期的占用承诺）。
		return nil, guestkit.ErrDataExpired
	}
	// 活跃预约配额（同用户 confirmed 计数）。
	existing, err := d.listAllMyReservations()
	if err != nil {
		return nil, err
	}
	active := 0
	for _, item := range existing {
		if item.EffectiveStatus(now) == library.ReservationConfirmed {
			active++
		}
	}
	if active >= library.MaxActiveReservationsPerUser {
		return guestkit.Conflict{Conflict: "quota_exceeded"}, nil
	}
	// 时段占用冲突：同一座位同一时段已被占用（快照占用或本人/他人预约）。
	free, err := d.seatFree(request, now)
	if err != nil {
		return nil, err
	}
	if !free {
		return guestkit.Conflict{Conflict: "seat_conflict"}, nil
	}
	item := library.Reservation{
		ReservationID: guestkit.NewDocID("r"), SpaceID: space.ID, SpaceName: space.Name,
		SeatID: seat.ID, SeatLabel: seat.Label, SlotID: slot.ID, SlotName: slot.Name,
		Date: request.Date, StartsAt: startsAt.Format(time.RFC3339), EndsAt: endsAt.Format(time.RFC3339),
		Status:    library.ReservationConfirmed,
		CreatedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(library.ReservationsCollection, item.ReservationID, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	return reservationResult{Reservation: item}, nil
}

// seatFree 判断座位在指定日期时段是否空闲：快照无占用，且本人无 confirmed
// 预约。个人预约是个人作用域文档：本 guest 只能看到当前用户的预约，跨用户
// 冲突由宿主侧快照占用（occupancy）承载——导入方在用户预约生效时写入。
func (d *dispatcher) seatFree(request library.ReservationCreateRequest, now time.Time) (bool, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[library.Occupancy](d.store, library.OccupancyCollection)
	if err != nil {
		return false, err
	}
	for _, item := range snapshot.Documents {
		if item.Date == request.Date && item.SeatID == request.SeatID && item.SlotID == request.SlotID {
			return false, nil
		}
	}
	page, err := d.store.List(guestkit.ScopeUser, library.ReservationsCollection, guestkit.MaxListPageSize, "")
	if err != nil {
		return false, guestkit.ErrInternal
	}
	for _, doc := range page.Docs {
		var item library.Reservation
		if err := json.Unmarshal(doc.Payload, &item); err != nil {
			return false, guestkit.ErrInternal
		}
		if item.EffectiveStatus(now) == library.ReservationConfirmed &&
			item.Date == request.Date && item.SeatID == request.SeatID && item.SlotID == request.SlotID {
			return false, nil
		}
	}
	return true, nil
}

func (d *dispatcher) cancelReservation(request library.ReservationCancelRequest) (any, error) {
	payload, found, err := d.store.Get(guestkit.ScopeUser, library.ReservationsCollection, request.ReservationID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if !found {
		return guestkit.NotFound{}, nil
	}
	var item library.Reservation
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, guestkit.ErrInternal
	}
	switch item.Status {
	case library.ReservationCancelled:
		return guestkit.Conflict{Conflict: request.ReservationID}, nil
	case library.ReservationConfirmed:
	default:
		return nil, errInvalidArgument
	}
	now := time.Now().UTC()
	// 已结束的时段不可取消（状态机由 EffectiveStatus 推导为 expired）。
	if item.EffectiveStatus(now) == library.ReservationExpired {
		return nil, errInvalidArgument
	}
	item.Status = library.ReservationCancelled
	item.UpdatedAt = now.Format(time.RFC3339Nano)
	item.CancelledAt = now.Format(time.RFC3339Nano)
	updated, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(library.ReservationsCollection, item.ReservationID, updated); err != nil {
		return nil, guestkit.ErrInternal
	}
	return reservationResult{Reservation: item}, nil
}

func (d *dispatcher) listMyReservations(request library.ReservationListRequest) (any, error) {
	items, err := d.listAllMyReservations()
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Date != items[j].Date {
			return items[i].Date < items[j].Date
		}
		return items[i].StartsAt < items[j].StartsAt
	})
	if len(items) > request.Limit {
		items = items[:request.Limit]
	}
	return reservationsResult{Reservations: items}, nil
}

func (d *dispatcher) listAllMyReservations() ([]library.Reservation, error) {
	// UserCollection 分页遍历全集合，避免宿主单页上限截断——配额与
	// 双重预约检查依赖全量视图，截断会放过超额/撞车写入。
	return guestkit.UserCollection[library.Reservation](userLister{client: d.store}, library.ReservationsCollection, nil)
}

// userLister 把 guestkit.Store 投影为固定个人作用域的 Lister；集合遍历
// 辅助函数一律经该端口工作，禁止本包手写分页循环。
type userLister struct {
	client guestkit.Store
}

func (u userLister) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	return u.client.List(guestkit.ScopeUser, collection, limit, afterID)
}

// ---- 快照定位辅助 ----

func (d *dispatcher) governedSpace(spaceID string) (*library.Space, guestkit.DataStatus, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[library.Space](d.store, library.SpacesCollection)
	if err != nil {
		return nil, dataStatus, err
	}
	return guestkit.FindSnapshotDoc(snapshot, func(item *library.Space) bool { return item.ID == spaceID }), dataStatus, nil
}

func (d *dispatcher) governedSeat(spaceID, seatID string) (*library.Seat, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[library.Seat](d.store, library.SeatsCollection)
	if err != nil {
		return nil, err
	}
	return guestkit.FindSnapshotDoc(snapshot, func(item *library.Seat) bool {
		return item.ID == seatID && item.SpaceID == spaceID
	}), nil
}

func (d *dispatcher) governedSlot(slotID string) (*library.Slot, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[library.Slot](d.store, library.SlotsCollection)
	if err != nil {
		return nil, err
	}
	return guestkit.FindSnapshotDoc(snapshot, func(item *library.Slot) bool { return item.ID == slotID }), nil
}
