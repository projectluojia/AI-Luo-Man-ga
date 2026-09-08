// Package app 是空闲教室包的协议层：6 个 Capability 的载荷分发。读取走
// 系统作用域快照治理（guestkit.GovernedSnapshot），日程写入走个人作用域
// 文档操作（宿主从治理上下文注入 UserID，guest 不可指定）。
// 本层不接触 wasmimport，可在原生 go test 下完整测试。
//
// 错误面与 library/sports/ecard 同型：ok:false 只保留 invalid_argument 与
// 数据治理闭式错误码；not_found/conflict 等业务失败在 ok:true 的结果载荷内
// 以 found/conflict 字段表达。
package app

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/projectluojia/classroom/classroom"
	"github.com/projectluojia/guestkit"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const (
	capRoomsSearch    = "classroom.rooms.search"
	capCampusesList   = "classroom.campuses.list"
	capBuildingsList  = "classroom.buildings.list"
	capScheduleCreate = "classroom.schedule.create"
	capScheduleList   = "classroom.schedule.list"
	capScheduleCancel = "classroom.schedule.cancel"
)

// capacityResult 是配额超限的带内业务失败结果。
type capacityResult struct {
	CapacityExceeded bool `json:"capacity_exceeded"`
}

// dispatcher 持有存储端口；处理函数由 Handlers 注册到 guestkit.Dispatcher。
type dispatcher struct {
	store guestkit.Store
}

// Handlers 返回能力分发表（src/main.go 装配 guestkit.Run 用）。
func Handlers(client guestkit.Store) map[string]func(json.RawMessage) (any, error) {
	d := &dispatcher{store: client}
	return map[string]func(json.RawMessage) (any, error){
		capRoomsSearch:    func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.searchRooms) },
		capCampusesList:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listCampuses) },
		capBuildingsList:  func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listBuildings) },
		capScheduleCreate: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.createSchedule) },
		capScheduleList:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listSchedules) },
		capScheduleCancel: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.cancelSchedule) },
	}
}

// ---- 快照读取（系统作用域） ----

type roomsSearchResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Rooms      []classroom.Room    `json:"rooms"`
}

type campusesListResult struct {
	DataStatus guestkit.DataStatus `json:"data_status"`
	Campuses   []classroom.Campus  `json:"campuses"`
}

type buildingsListResult struct {
	DataStatus guestkit.DataStatus  `json:"data_status"`
	Buildings  []classroom.Building `json:"buildings"`
}

func (d *dispatcher) searchRooms(request classroom.RoomsSearchRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[classroom.Room](d.store, "rooms")
	if err != nil {
		return nil, err
	}
	// 空闲 = 该日期节次无占用记录。
	busy, err := d.busyRoomIDs(request.Date, request.Period)
	if err != nil {
		return nil, err
	}
	matches := make([]classroom.Room, 0, len(snapshot.Documents))
	for _, room := range snapshot.Documents {
		if room.CampusID == request.CampusID && !busy[room.ID] &&
			(request.BuildingID == "" || room.BuildingID == request.BuildingID) {
			matches = append(matches, room)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	if len(matches) > request.Limit {
		matches = matches[:request.Limit]
	}
	return roomsSearchResult{DataStatus: dataStatus, Rooms: matches}, nil
}

func (d *dispatcher) listCampuses(request classroom.CampusListRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[classroom.Campus](d.store, "campuses")
	if err != nil {
		return nil, err
	}
	campuses := make([]classroom.Campus, 0, len(snapshot.Documents))
	campuses = append(campuses, snapshot.Documents...)
	sort.Slice(campuses, func(i, j int) bool { return campuses[i].ID < campuses[j].ID })
	if len(campuses) > request.Limit {
		campuses = campuses[:request.Limit]
	}
	return campusesListResult{DataStatus: dataStatus, Campuses: campuses}, nil
}

func (d *dispatcher) listBuildings(request classroom.BuildingListRequest) (any, error) {
	snapshot, dataStatus, err := guestkit.GovernedSnapshot[classroom.Building](d.store, "buildings")
	if err != nil {
		return nil, err
	}
	buildings := make([]classroom.Building, 0, len(snapshot.Documents))
	for _, item := range snapshot.Documents {
		if item.CampusID == request.CampusID {
			buildings = append(buildings, item)
		}
	}
	sort.Slice(buildings, func(i, j int) bool { return buildings[i].ID < buildings[j].ID })
	if len(buildings) > request.Limit {
		buildings = buildings[:request.Limit]
	}
	return buildingsListResult{DataStatus: dataStatus, Buildings: buildings}, nil
}

// busyRoomIDs 载入指定日期节次的占用房间集合。
func (d *dispatcher) busyRoomIDs(date string, period int) (map[string]bool, error) {
	snapshot, _, err := guestkit.GovernedSnapshot[classroom.Occupancy](d.store, "occupancy")
	if err != nil {
		return nil, err
	}
	busy := make(map[string]bool)
	for _, item := range snapshot.Documents {
		if item.AcademicDate == date && item.Period == period {
			busy[item.RoomID] = true
		}
	}
	return busy, nil
}

// ---- 个人日程（个人作用域，写路径） ----

type scheduleCreateResult struct {
	Schedule classroom.ScheduleItem `json:"schedule"`
}

type scheduleListResult struct {
	Schedules []classroom.ScheduleItem `json:"schedules"`
}

type scheduleCancelResult struct {
	Schedule classroom.ScheduleItem `json:"schedule"`
}

func (d *dispatcher) createSchedule(request classroom.ScheduleCreateRequest) (any, error) {
	// 日程关联的教室必须存在于当前权威快照。
	snapshot, _, err := guestkit.GovernedSnapshot[classroom.Room](d.store, "rooms")
	if err != nil {
		return nil, err
	}
	room := guestkit.FindSnapshotDoc(snapshot, func(item *classroom.Room) bool { return item.ID == request.RoomID })
	if room == nil {
		return guestkit.NotFound{}, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	item := classroom.ScheduleItem{
		ScheduleID: guestkit.NewDocID("s"), RoomID: room.ID, CampusID: room.CampusID, BuildingID: room.BuildingID,
		RoomName: room.Name, Date: request.Date, Period: request.Period, Title: request.Title,
		Status: classroom.StatusScheduled, CreatedAt: now, UpdatedAt: now,
	}
	// 配额检查与写入之间无事务边界（个人作用域同用户调用顺序到达，
	// TOCTOU 不构成实际问题）；超限按带内 capacity 拒绝。
	count, err := countAll(d.store, classroom.SchedulesCollection)
	if err != nil {
		return nil, err
	}
	if count >= classroom.MaxSchedulesPerUser {
		return capacityResult{CapacityExceeded: true}, nil
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(classroom.SchedulesCollection, item.ScheduleID, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	return scheduleCreateResult{Schedule: item}, nil
}

func (d *dispatcher) listSchedules(request classroom.ScheduleListRequest) (any, error) {
	// UserCollection 分页遍历全集合，避免宿主单页上限截断。
	items, err := guestkit.UserCollection[classroom.ScheduleItem](userLister{client: d.store}, classroom.SchedulesCollection, nil)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Date != items[j].Date {
			return items[i].Date < items[j].Date
		}
		return items[i].Period < items[j].Period
	})
	if len(items) > request.Limit {
		items = items[:request.Limit]
	}
	return scheduleListResult{Schedules: items}, nil
}

func (d *dispatcher) cancelSchedule(request classroom.ScheduleCancelRequest) (any, error) {
	payload, found, err := d.store.Get(guestkit.ScopeUser, classroom.SchedulesCollection, request.ScheduleID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if !found {
		return guestkit.NotFound{}, nil
	}
	var item classroom.ScheduleItem
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, guestkit.ErrInternal
	}
	if item.Status == classroom.StatusCancelled {
		return guestkit.Conflict{Conflict: request.ScheduleID}, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	item.Status = classroom.StatusCancelled
	item.UpdatedAt = now
	item.CancelledAt = now
	updated, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(classroom.SchedulesCollection, item.ScheduleID, updated); err != nil {
		return nil, guestkit.ErrInternal
	}
	return scheduleCancelResult{Schedule: item}, nil
}

// userLister 把 guestkit.Store 投影为固定个人作用域的 Lister（与系统作用域
// 快照读取对称）；集合遍历辅助函数一律经该端口工作。
type userLister struct {
	client guestkit.Store
}

func (u userLister) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	return u.client.List(guestkit.ScopeUser, collection, limit, afterID)
}

// countAll 分页统计个人作用域集合文档数（宿主单页上限 200）。
func countAll(client guestkit.Store, collection string) (int, error) {
	count := 0
	afterID := ""
	for {
		page, err := client.List(guestkit.ScopeUser, collection, guestkit.MaxListPageSize, afterID)
		if err != nil {
			return 0, guestkit.ErrInternal
		}
		count += len(page.Docs)
		if len(page.Docs) < guestkit.MaxListPageSize {
			return count, nil
		}
		afterID = page.Docs[len(page.Docs)-1].ID
	}
}
