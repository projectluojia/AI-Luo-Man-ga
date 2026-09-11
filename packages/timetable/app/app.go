// Package app 是课表包的协议层：把 12 个 Capability 的调用载荷分发到
// ailuo.store 个人作用域文档操作上，并保持课表领域约束（容量上限、删除级联、
// 导入全有或全无）。分发与错误映射统一走 guestkit 共享 Dispatcher——本包只
// 声明能力分发表；本层不接触 wasmimport，可在原生 go test 下完整测试业务分发。
//
// 错误面：内核闭式错误码不含 not_found/conflict，业务级失败在 ok:true 的
// 结果载荷内以 found/conflict/capacity_exceeded/status 字段表达；ok:false
// 只保留参数非法（invalid_argument）与存储调用失败（internal）两类。
//
// 激活语义：当前激活课表记录在 active 集合的单个指针文档内——单文档写入即
// 原子生效，课表文档自身不携带激活状态，list/get 按指针实时计算 active，
// 消除多文档批量切换的非原子窗口。
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/timetable/tt"
)

// Capability 常量与清单 ailuo.toml 的 exports 一一对应。
const (
	capTimetableList     = "timetable.list"
	capTimetableGet      = "timetable.get"
	capTimetableCreate   = "timetable.create"
	capTimetableUpdate   = "timetable.update"
	capTimetableDelete   = "timetable.delete"
	capTimetableActivate = "timetable.activate"
	capCourseList        = "timetable.course.list"
	capCourseGet         = "timetable.course.get"
	capCourseCreate      = "timetable.course.create"
	capCourseUpdate      = "timetable.course.update"
	capCourseDelete      = "timetable.course.delete"
	capImport            = "timetable.import"

	tablesCollection     = "tables"
	coursesCollectionFmt = "courses-%s"
	// 激活指针：active 集合内单文档记录当前激活的课表 ID。
	activeCollection   = "active"
	activePointerDocID = "pointer"

	importCourseDocIDPrefix = "import-"
)

// NewDispatcher 构造分发器：以 guestkit 共享 Dispatcher 分发（唯一实现，
// 包内只声明能力分发表）。client 为 guestkit.StoreClient（wasip1 内）或测试替身。
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
		capTimetableList:     func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listTimetables) },
		capTimetableGet:      func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.getTimetable) },
		capTimetableCreate:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.createTimetable) },
		capTimetableUpdate:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.updateTimetable) },
		capTimetableDelete:   func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.deleteTimetable) },
		capTimetableActivate: func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.activateTimetable) },
		capCourseList:        func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.listCourses) },
		capCourseGet:         func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.getCourse) },
		capCourseCreate:      func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.createCourse) },
		capCourseUpdate:      func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.updateCourse) },
		capCourseDelete:      func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.deleteCourse) },
		capImport:            func(payload json.RawMessage) (any, error) { return guestkit.DecodeRun(payload, d.importTimetable) },
	}
}

// ---- 载荷类型（与 ailuo.toml capability schema 一一对应）----

type idInput struct {
	TimetableID string `json:"timetable_id"`
}

type timetableCreateInput struct {
	TimetableID string `json:"timetable_id"`
	Name        string `json:"name"`
	Active      bool   `json:"active"`
}

type timetableUpdateInput struct {
	TimetableID string `json:"timetable_id"`
	Name        string `json:"name"`
	Active      *bool  `json:"active"`
}

type courseInput struct {
	TimetableID  string `json:"timetable_id"`
	CourseID     string `json:"course_id"`
	Title        string `json:"title"`
	Weekday      int    `json:"weekday"`
	ClassFrom    int    `json:"class_from"`
	ClassTo      int    `json:"class_to"`
	Weeks        []int  `json:"weeks"`
	CourseNature string `json:"course_nature"`
	Instructor   string `json:"instructor"`
	Location     string `json:"location"`
	WeekMeta     string `json:"week_meta"`
	StartText    string `json:"start_text"`
	EndText      string `json:"end_text"`
	ExternalID   string `json:"external_id"`
}

type courseGetInput struct {
	TimetableID string `json:"timetable_id"`
	CourseID    string `json:"course_id"`
}

type importInput struct {
	Format      string `json:"format"`
	Content     string `json:"content"`
	FileName    string `json:"fileName"`
	Name        string `json:"name"`
	TimetableID string `json:"timetable_id"`
	Active      bool   `json:"active"`
}

// ---- 结果载荷 ----

// timetableView 是课表对外的展示视图：文档载荷加实时计算的激活状态。
type timetableView struct {
	tt.Timetable
	Active bool `json:"active"`
}

// newTimetableView 以指针记录计算课表的激活状态。
func newTimetableView(item tt.Timetable, activeID string) timetableView {
	return timetableView{Timetable: item, Active: activeID != "" && item.ID == activeID}
}

type timetableListResult struct {
	Timetables []timetableView `json:"timetables"`
}

type timetableGetResult struct {
	Found     bool           `json:"found"`
	Timetable *timetableView `json:"timetable,omitempty"`
}

type timetableMutateResult struct {
	Timetable timetableView `json:"timetable"`
}

type timetableDeleteResult struct {
	Deleted bool `json:"deleted"`
}

type activateResult struct {
	Activated string `json:"activated"`
}

type courseListResult struct {
	Courses []tt.Course `json:"courses"`
}

type courseGetResult struct {
	Found  bool       `json:"found"`
	Course *tt.Course `json:"course,omitempty"`
}

type courseMutateResult struct {
	Course tt.Course `json:"course"`
}

type courseDeleteResult struct {
	Deleted bool `json:"deleted"`
}

type importResult struct {
	Timetable timetableView `json:"timetable"`
	Courses   []tt.Course   `json:"courses"`
}

// importError 是导入业务失败的带内结果：status 是闭式集合，调用方据此
// 区分格式错误/超限/不支持格式等失败原因。
type importError struct {
	Status string `json:"status"`
}

// notFound/conflict/capacity 是读与写的带内业务失败结果。
type notFoundResult struct {
	Found bool `json:"found"`
}

type conflictResult struct {
	Conflict string `json:"conflict"`
}

type capacityResult struct {
	CapacityExceeded bool `json:"capacity_exceeded"`
}

// activePointerDoc 是激活指针文档载荷：指向当前激活课表的 ID。
type activePointerDoc struct {
	TimetableID string `json:"timetable_id"`
}

// ---- 课表文档操作 ----

func (d *dispatcher) listTimetables(_ struct{}) (any, error) {
	items, err := guestkit.UserCollection[tt.Timetable](d.store, tablesCollection, nil)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	activeID, err := d.activePointerID()
	if err != nil {
		return nil, err
	}
	views := make([]timetableView, 0, len(items))
	for _, item := range items {
		views = append(views, newTimetableView(item, activeID))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return timetableListResult{Timetables: views}, nil
}

func (d *dispatcher) getTimetable(input idInput) (any, error) {
	table, found, err := d.loadTable(input.TimetableID)
	if err != nil {
		return nil, err
	}
	if !found {
		return notFoundResult{Found: false}, nil
	}
	activeID, err := d.activePointerID()
	if err != nil {
		return nil, err
	}
	view := newTimetableView(table, activeID)
	return timetableGetResult{Found: true, Timetable: &view}, nil
}

func (d *dispatcher) createTimetable(input timetableCreateInput) (any, error) {
	id := strings.TrimSpace(input.TimetableID)
	if id == "" {
		id = guestkit.NewDocID("t")
	}
	if !tt.ValidTimetableID(id) {
		return nil, guestkit.ErrInvalidArgument
	}
	now := nowRFC3339()
	item, err := tt.NormalizeNewTimetable(tt.Timetable{ID: id, Name: input.Name, Source: tt.SourceLocal, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return nil, guestkit.ErrInvalidArgument
	}
	if _, found, err := d.loadTable(id); err != nil {
		return nil, err
	} else if found {
		return conflictResult{Conflict: id}, nil
	}
	// TOCTOU 说明：count 检查与写入之间没有事务边界，并发写可能突破容量
	// 上限；个人作用域按用户物理隔离、同用户调用顺序到达，不构成实际问题，
	// 宿主侧闭式校验兜底键与载荷合法性。
	count, err := d.countAll(tablesCollection)
	if err != nil {
		return nil, err
	}
	if count >= tt.MaxTimetablesPerUser {
		return capacityResult{CapacityExceeded: true}, nil
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(tablesCollection, id, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	if input.Active {
		if err := d.putActivePointer(id); err != nil {
			return nil, err
		}
	}
	activeID, err := d.activePointerID()
	if err != nil {
		return nil, err
	}
	return timetableMutateResult{Timetable: newTimetableView(item, activeID)}, nil
}

func (d *dispatcher) updateTimetable(input timetableUpdateInput) (any, error) {
	item, found, err := d.loadTable(input.TimetableID)
	if err != nil {
		return nil, err
	}
	if !found {
		return notFoundResult{Found: false}, nil
	}
	item.Name = input.Name
	item, err = tt.NormalizeTimetable(item)
	if err != nil {
		return nil, guestkit.ErrInvalidArgument
	}
	item.UpdatedAt = nowRFC3339()
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(tablesCollection, item.ID, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	if input.Active != nil {
		if *input.Active {
			if err := d.putActivePointer(item.ID); err != nil {
				return nil, err
			}
		} else if pointerID, err := d.activePointerID(); err != nil {
			return nil, err
		} else if pointerID == item.ID {
			// 取消激活仅当指针当前指向本表。
			if _, err := d.store.Delete(activeCollection, activePointerDocID); err != nil {
				return nil, guestkit.ErrInternal
			}
		}
	}
	activeID, err := d.activePointerID()
	if err != nil {
		return nil, err
	}
	return timetableMutateResult{Timetable: newTimetableView(item, activeID)}, nil
}

func (d *dispatcher) deleteTimetable(input idInput) (any, error) {
	if !tt.ValidTimetableID(input.TimetableID) {
		return nil, guestkit.ErrInvalidArgument
	}
	// 先清激活指针，再级联清课程集合，最后删课表文档：中断重放时用户最多
	// 看到无课程的课表或无激活课表，不会出现悬挂引用。
	pointerID, err := d.activePointerID()
	if err != nil {
		return nil, err
	}
	if pointerID == input.TimetableID {
		if _, err := d.store.Delete(activeCollection, activePointerDocID); err != nil {
			return nil, guestkit.ErrInternal
		}
	}
	if err := d.deleteCollection(fmt.Sprintf(coursesCollectionFmt, input.TimetableID)); err != nil {
		return nil, err
	}
	deleted, err := d.store.Delete(tablesCollection, input.TimetableID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	return timetableDeleteResult{Deleted: deleted}, nil
}

func (d *dispatcher) activateTimetable(input idInput) (any, error) {
	target, found, err := d.loadTable(input.TimetableID)
	if err != nil {
		return nil, err
	}
	if !found {
		return notFoundResult{Found: false}, nil
	}
	// 单文档指针写入即原子切换当前激活课表。
	if err := d.putActivePointer(target.ID); err != nil {
		return nil, err
	}
	return activateResult{Activated: target.ID}, nil
}

// ---- 课程文档操作 ----

func (d *dispatcher) listCourses(input idInput) (any, error) {
	if _, found, err := d.loadTable(input.TimetableID); err != nil {
		return nil, err
	} else if !found {
		return notFoundResult{Found: false}, nil
	}
	items, err := guestkit.UserCollection[tt.Course](d.store, fmt.Sprintf(coursesCollectionFmt, input.TimetableID), nil)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return courseListResult{Courses: items}, nil
}

func (d *dispatcher) getCourse(input courseGetInput) (any, error) {
	if !tt.ValidTimetableID(input.TimetableID) || !tt.ValidDocID(input.CourseID) {
		return nil, guestkit.ErrInvalidArgument
	}
	payload, found, err := d.store.Get(guestkit.ScopeUser, fmt.Sprintf(coursesCollectionFmt, input.TimetableID), input.CourseID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if !found {
		return courseGetResult{Found: false}, nil
	}
	var item tt.Course
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, guestkit.ErrInternal
	}
	return courseGetResult{Found: true, Course: &item}, nil
}

func (d *dispatcher) createCourse(input courseInput) (any, error) {
	if _, found, err := d.loadTable(input.TimetableID); err != nil {
		return nil, err
	} else if !found {
		return notFoundResult{Found: false}, nil
	}
	collection := fmt.Sprintf(coursesCollectionFmt, input.TimetableID)
	id := strings.TrimSpace(input.CourseID)
	if id == "" {
		id = guestkit.NewDocID("c")
	}
	item, err := normalizeCourseInput(input, id)
	if err != nil {
		return nil, err
	}
	if _, found, err := d.store.Get(guestkit.ScopeUser, collection, id); err != nil {
		return nil, guestkit.ErrInternal
	} else if found {
		return conflictResult{Conflict: id}, nil
	}
	count, err := d.countAll(collection)
	if err != nil {
		return nil, err
	}
	if count >= tt.MaxCoursesPerTable {
		return capacityResult{CapacityExceeded: true}, nil
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(collection, id, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	return courseMutateResult{Course: item}, nil
}

func (d *dispatcher) updateCourse(input courseInput) (any, error) {
	if !tt.ValidTimetableID(input.TimetableID) || !tt.ValidDocID(input.CourseID) {
		return nil, guestkit.ErrInvalidArgument
	}
	collection := fmt.Sprintf(coursesCollectionFmt, input.TimetableID)
	if _, found, err := d.store.Get(guestkit.ScopeUser, collection, input.CourseID); err != nil {
		return nil, guestkit.ErrInternal
	} else if !found {
		return notFoundResult{Found: false}, nil
	}
	item, err := normalizeCourseInput(input, input.CourseID)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(collection, item.ID, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	return courseMutateResult{Course: item}, nil
}

func (d *dispatcher) deleteCourse(input courseGetInput) (any, error) {
	if !tt.ValidTimetableID(input.TimetableID) || !tt.ValidDocID(input.CourseID) {
		return nil, guestkit.ErrInvalidArgument
	}
	deleted, err := d.store.Delete(fmt.Sprintf(coursesCollectionFmt, input.TimetableID), input.CourseID)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	return courseDeleteResult{Deleted: deleted}, nil
}

// ---- 导入（全有或全无）----

func (d *dispatcher) importTimetable(input importInput) (any, error) {
	data, err := parseImport(input)
	switch {
	case err != nil:
		return importStatusResult(err), nil
	case len(data.Courses) == 0:
		return importStatusResult(tt.ErrNoCourses), nil
	}
	name := tt.SanitizeDisplay(input.Name)
	if name == "" {
		name = tt.SanitizeDisplay(data.Name)
	}
	if name == "" {
		name = "导入课表"
	}
	id := strings.TrimSpace(input.TimetableID)
	if id == "" {
		id = guestkit.NewDocID("t")
	}
	if !tt.ValidTimetableID(id) {
		return importStatusResult(tt.ErrInvalid), nil
	}
	collection := fmt.Sprintf(coursesCollectionFmt, id)
	if _, found, err := d.loadTable(id); err != nil {
		return nil, err
	} else if found {
		return importStatusResult(tt.ErrConflict), nil
	}
	courseCount, err := d.countAll(collection)
	if err != nil {
		return nil, err
	}
	if courseCount > 0 {
		// 上一次全有或全无导入中断的残留：先清空再继续，保证重放安全。
		if err := d.deleteCollection(collection); err != nil {
			return nil, err
		}
	}
	tableCount, err := d.countAll(tablesCollection)
	if err != nil {
		return nil, err
	}
	if tableCount >= tt.MaxTimetablesPerUser || len(data.Courses) > tt.MaxCoursesPerTable {
		return importStatusResult(tt.ErrCapacity), nil
	}
	now := nowRFC3339()
	table := tt.Timetable{ID: id, Name: name, Source: data.Source, CreatedAt: now, UpdatedAt: now}
	if _, err := tt.NormalizeTimetable(table); err != nil {
		return importStatusResult(tt.ErrInvalid), nil
	}
	courses := make([]tt.Course, 0, len(data.Courses))
	for index := range data.Courses {
		course := data.Courses[index]
		course.TimetableID = id
		// 确定性 ID：同一导入载荷重放时生成同一 ID，中断恢复后可安全重放。
		course.ID = fmt.Sprintf("%s%d", importCourseDocIDPrefix, index+1)
		course, err = tt.NormalizeCourse(course)
		if err != nil {
			return importStatusResult(tt.ErrInvalid), nil
		}
		courses = append(courses, course)
	}
	for _, course := range courses {
		payload, err := json.Marshal(course)
		if err != nil {
			return nil, guestkit.ErrInternal
		}
		if err := d.store.Put(collection, course.ID, payload); err != nil {
			return nil, guestkit.ErrInternal
		}
	}
	// 课表文档最后写入：它存在 ⇔ 课程集合完整，读者以此判断导入完整性。
	// 激活指针在课表文档之后写入，保证指针指向的课表必然完整。
	payload, err := json.Marshal(table)
	if err != nil {
		return nil, guestkit.ErrInternal
	}
	if err := d.store.Put(tablesCollection, id, payload); err != nil {
		return nil, guestkit.ErrInternal
	}
	if input.Active {
		if err := d.putActivePointer(id); err != nil {
			return nil, err
		}
	}
	activeID, err := d.activePointerID()
	if err != nil {
		return nil, err
	}
	return importResult{Timetable: newTimetableView(table, activeID), Courses: courses}, nil
}

// parseImport 是导入格式的唯一分发入口，别名集合必须与 ailuo.toml
// timetable.import schema 的 enum 保持一致。内容按 UTF-8 字节预算先行校验，
// 超限返回 ErrTooLarge 而不是格式错误；分发直达具体解析器，不经 envelope
// 重编码，避免转义开销使大小判断失真。
func parseImport(input importInput) (tt.ImportData, error) {
	if len(input.Content) > tt.MaxImportBytes {
		return tt.ImportData{}, tt.ErrTooLarge
	}
	format := strings.ToLower(strings.TrimSpace(input.Format))
	switch format {
	case "wuda", "academic", "whu":
		courses, err := tt.ParseAcademic([]byte(input.Content))
		return tt.ImportData{Name: tt.FileBase(input.FileName), Source: tt.SourceWUDA, Courses: courses}, err
	case "legacy":
		return tt.ParseWakeUpLegacy(input.Content)
	case "wakeup", "csv":
		return tt.ParseWakeUpCSV(input.Content, input.FileName)
	default:
		return tt.ImportData{}, tt.ErrUnsupported
	}
}

// importStatusResult 把导入领域错误映射为闭式 status 集合。
func importStatusResult(err error) importError {
	switch {
	case errors.Is(err, tt.ErrMalformedData):
		return importError{Status: "malformed_data"}
	case errors.Is(err, tt.ErrTooLarge):
		return importError{Status: "too_large"}
	case errors.Is(err, tt.ErrUnsupported):
		return importError{Status: "unsupported_format"}
	case errors.Is(err, tt.ErrNoCourses):
		return importError{Status: "no_courses"}
	case errors.Is(err, tt.ErrCapacity):
		return importError{Status: "capacity_exceeded"}
	case errors.Is(err, tt.ErrConflict):
		return importError{Status: "conflict"}
	case errors.Is(err, tt.ErrInvalid):
		return importError{Status: "invalid_input"}
	default:
		return importError{Status: "internal"}
	}
}

// ---- 公共小工具 ----

// loadTable 读取课表文档；ID 校验是全部课表路由的统一入口规则
// （ValidTimetableID 同时约束派生课程集合名的合法性）。
func (d *dispatcher) loadTable(id string) (tt.Timetable, bool, error) {
	if !tt.ValidTimetableID(id) {
		return tt.Timetable{}, false, guestkit.ErrInvalidArgument
	}
	payload, found, err := d.store.Get(guestkit.ScopeUser, tablesCollection, id)
	if err != nil {
		return tt.Timetable{}, false, guestkit.ErrInternal
	}
	if !found {
		return tt.Timetable{}, false, nil
	}
	var item tt.Timetable
	if err := json.Unmarshal(payload, &item); err != nil {
		return tt.Timetable{}, false, guestkit.ErrInternal
	}
	return item, true, nil
}

// activePointerID 读取当前激活课表 ID；无指针文档表示没有激活课表。
func (d *dispatcher) activePointerID() (string, error) {
	payload, found, err := d.store.Get(guestkit.ScopeUser, activeCollection, activePointerDocID)
	if err != nil {
		return "", guestkit.ErrInternal
	}
	if !found {
		return "", nil
	}
	var pointer activePointerDoc
	if err := json.Unmarshal(payload, &pointer); err != nil {
		return "", guestkit.ErrInternal
	}
	return pointer.TimetableID, nil
}

// putActivePointer 单文档写入切换当前激活课表（原子生效）。
func (d *dispatcher) putActivePointer(id string) error {
	payload, err := json.Marshal(activePointerDoc{TimetableID: id})
	if err != nil {
		return guestkit.ErrInternal
	}
	if err := d.store.Put(activeCollection, activePointerDocID, payload); err != nil {
		return guestkit.ErrInternal
	}
	return nil
}

// countAll 分页统计集合文档数（宿主单页上限 200）。
func (d *dispatcher) countAll(collection string) (int, error) {
	count := 0
	afterID := ""
	for {
		page, err := d.store.List(guestkit.ScopeUser, collection, guestkit.MaxListPageSize, afterID)
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

func (d *dispatcher) deleteCollection(collection string) error {
	for {
		page, err := d.store.List(guestkit.ScopeUser, collection, guestkit.MaxListPageSize, "")
		if err != nil {
			return guestkit.ErrInternal
		}
		if len(page.Docs) == 0 {
			return nil
		}
		for _, doc := range page.Docs {
			if _, err := d.store.Delete(collection, doc.ID); err != nil {
				return guestkit.ErrInternal
			}
		}
		if len(page.Docs) < guestkit.MaxListPageSize {
			return nil
		}
	}
}

// normalizeCourseInput 把调用载荷归一化为课程模型；ID 由调用方决定
// （生成或载荷携带）。
func normalizeCourseInput(input courseInput, id string) (tt.Course, error) {
	item, err := tt.NormalizeCourse(tt.Course{
		TimetableID: input.TimetableID, ID: id, Title: input.Title, Weekday: input.Weekday,
		ClassFrom: input.ClassFrom, ClassTo: input.ClassTo, Weeks: input.Weeks,
		CourseNature: input.CourseNature, Instructor: input.Instructor, Location: input.Location,
		WeekMeta: input.WeekMeta, StartText: input.StartText, EndText: input.EndText,
		ExternalID: input.ExternalID,
	})
	if err != nil {
		return tt.Course{}, guestkit.ErrInvalidArgument
	}
	return item, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
