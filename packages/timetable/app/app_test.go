package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/projectluojia/guestkit"
	"github.com/projectluojia/timetable/tt"
)

// memoryStore 是 ailuo.store 个人作用域的最小内存实现：与宿主一致的
// upsert/delete 语义与按 doc_id 升序分页。
type memoryStore struct {
	docs map[string]map[string]json.RawMessage
}

func newMemoryStore() *memoryStore {
	return &memoryStore{docs: map[string]map[string]json.RawMessage{}}
}

func (m *memoryStore) Get(_ guestkit.Scope, collection, id string) (json.RawMessage, bool, error) {
	collectionDocs, ok := m.docs[collection]
	if !ok {
		return nil, false, nil
	}
	payload, ok := collectionDocs[id]
	return payload, ok, nil
}

func (m *memoryStore) List(_ guestkit.Scope, collection string, limit int, afterID string) (guestkit.ListPage, error) {
	collectionDocs := m.docs[collection]
	ids := make([]string, 0, len(collectionDocs))
	for id := range collectionDocs {
		if afterID != "" && id <= afterID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	docs := make([]guestkit.Document, 0, len(ids))
	for _, id := range ids {
		docs = append(docs, guestkit.Document{ID: id, Payload: collectionDocs[id]})
	}
	return guestkit.ListPage{Docs: docs}, nil
}

func (m *memoryStore) Put(collection, id string, doc json.RawMessage) error {
	if m.docs[collection] == nil {
		m.docs[collection] = map[string]json.RawMessage{}
	}
	m.docs[collection][id] = append(json.RawMessage(nil), doc...)
	return nil
}

func (m *memoryStore) Delete(collection, id string) (bool, error) {
	collectionDocs, ok := m.docs[collection]
	if !ok {
		return false, nil
	}
	if _, ok := collectionDocs[id]; !ok {
		return false, nil
	}
	delete(collectionDocs, id)
	return true, nil
}

func newTestDispatcher() (*guestkit.Dispatcher, *memoryStore) {
	store := newMemoryStore()
	return guestkit.NewDispatcher(Handlers(store)), store
}

func dispatch(d *guestkit.Dispatcher, capabilityID string, payload any) (guestkit.ResultEnvelope, map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		tpanic(err)
	}
	envelope := d.Dispatch(capabilityID, encoded)
	if !envelope.OK {
		return envelope, nil
	}
	var result map[string]any
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		tpanic(err)
	}
	return envelope, result
}

func tpanic(err error) { panic(err) }

// mustTable 解码结果载荷中的课表视图（文档字段 + 实时 active）。
func mustTable(t *testing.T, result map[string]any) timetableView {
	t.Helper()
	encoded, err := json.Marshal(result["timetable"])
	if err != nil {
		t.Fatal(err)
	}
	var item timetableView
	if err := json.Unmarshal(encoded, &item); err != nil {
		t.Fatal(err)
	}
	return item
}

// activePointer 读取内存存储中的激活指针文档；found=false 表示无激活课表。
func activePointer(t *testing.T, store *memoryStore) (string, bool) {
	t.Helper()
	payload, found, err := store.Get(guestkit.ScopeUser, activeCollection, activePointerDocID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return "", false
	}
	var pointer activePointerDoc
	if err := json.Unmarshal(payload, &pointer); err != nil {
		t.Fatal(err)
	}
	return pointer.TimetableID, true
}

const academicPayload = `{"kbList":[{"kcmc":"高数","xqjmc":"星期三","jcs":"3-4","zcd":"1-4周","xm":"张老师","cdmc":"教一101"},{"kcmc":"物理","xqj":"5","jcs":"6","zcd":"1-16周"}]}`

func TestCreateListGetActivateAndDeleteTimetable(t *testing.T) {
	d, store := newTestDispatcher()

	envelope, result := dispatch(d, capTimetableCreate, timetableCreateInput{Name: "期中", Active: true})
	if envelope.Code != "" || result == nil {
		t.Fatalf("create envelope=%#v", envelope)
	}
	created := mustTable(t, result)
	if created.ID == "" || !created.Active || created.Source != tt.SourceLocal || created.CreatedAt == "" {
		t.Fatalf("created=%#v", created)
	}

	_, listResult := dispatch(d, capTimetableList, struct{}{})
	if items, ok := listResult["timetables"].([]any); !ok || len(items) != 1 {
		t.Fatalf("list result=%#v", listResult)
	}

	_, getResult := dispatch(d, capTimetableGet, idInput{TimetableID: created.ID})
	if found, _ := getResult["found"].(bool); !found || mustTable(t, getResult).ID != created.ID {
		t.Fatalf("get result=%#v", getResult)
	}

	// 第二份课表激活后指针原子切换到第二份：课表文档不携带激活状态。
	_, second := dispatch(d, capTimetableCreate, timetableCreateInput{Name: "期末"})
	secondID := mustTable(t, second).ID
	_, activated := dispatch(d, capTimetableActivate, idInput{TimetableID: secondID})
	if activated["activated"] != secondID {
		t.Fatalf("activate result=%#v", activated)
	}
	if pointerID, found := activePointer(t, store); !found || pointerID != secondID {
		t.Fatalf("active pointer=%q found=%v, want %q", pointerID, found, secondID)
	}
	firstAfter := dispatchView(t, d, capTimetableGet, idInput{TimetableID: created.ID})
	if firstAfter.Active {
		t.Fatalf("first timetable still active: %+v", firstAfter)
	}

	_, deleted := dispatch(d, capTimetableDelete, idInput{TimetableID: secondID})
	if deletedFlag, _ := deleted["deleted"].(bool); !deletedFlag {
		t.Fatalf("delete result=%#v", deleted)
	}
	if _, found, _ := store.Get(guestkit.ScopeUser, tablesCollection, secondID); found {
		t.Fatal("deleted timetable still present")
	}
	// 删除激活课表级联清空激活指针。
	if _, found := activePointer(t, store); found {
		t.Fatal("active pointer survived deleting the active timetable")
	}

	// 读取已删除课表按带内 not-found 应答。
	_, missing := dispatch(d, capTimetableGet, idInput{TimetableID: secondID})
	if found, _ := missing["found"].(bool); found {
		t.Fatalf("missing get result=%#v", missing)
	}
}

// dispatchView 是 dispatch 的强类型课表视图版本。
func dispatchView(t *testing.T, d *guestkit.Dispatcher, capabilityID string, payload any) timetableView {
	t.Helper()
	envelope, result := dispatch(d, capabilityID, payload)
	if !envelope.OK {
		t.Fatalf("%s envelope=%#v", capabilityID, envelope)
	}
	return mustTable(t, result)
}

func TestUpdateClearsActivePointerOnlyForSelf(t *testing.T) {
	d, store := newTestDispatcher()
	_, first := dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "table-1", Name: "主表", Active: true})
	if !mustTable(t, first).Active {
		t.Fatal("created active table should report active")
	}
	_, _ = dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "table-2", Name: "副表"})

	// active=false 只在指针指向本表时取消激活。
	no := false
	envelope, view := dispatch(d, capTimetableUpdate, timetableUpdateInput{TimetableID: "table-2", Name: "副表", Active: &no})
	if !envelope.OK || mustTable(t, view).Active {
		t.Fatalf("deactivate other table envelope=%#v view=%#v", envelope, view)
	}
	if pointerID, found := activePointer(t, store); !found || pointerID != "table-1" {
		t.Fatalf("pointer changed by unrelated update: %q", pointerID)
	}

	// 更新本表激活状态生效。
	envelope, view = dispatch(d, capTimetableUpdate, timetableUpdateInput{TimetableID: "table-2", Name: "副表2", Active: &no})
	if !envelope.OK {
		t.Fatalf("update envelope=%#v", envelope)
	}
	_ = view
	yes := true
	envelope, view = dispatch(d, capTimetableUpdate, timetableUpdateInput{TimetableID: "table-2", Name: "副表2", Active: &yes})
	if !envelope.OK || !mustTable(t, view).Active {
		t.Fatalf("activate via update envelope=%#v view=%#v", envelope, view)
	}
	if pointerID, found := activePointer(t, store); !found || pointerID != "table-2" {
		t.Fatalf("pointer after update=%q found=%v", pointerID, found)
	}

	// 激活本表后取消：指针被清除，课表仍存在。
	envelope, view = dispatch(d, capTimetableUpdate, timetableUpdateInput{TimetableID: "table-2", Name: "副表2", Active: &no})
	if !envelope.OK || mustTable(t, view).Active {
		t.Fatalf("deactivate envelope=%#v view=%#v", envelope, view)
	}
	if _, found := activePointer(t, store); found {
		t.Fatal("pointer should be cleared after deactivating the active table")
	}
}

func TestCreateRejectsConflictCapacityAndInvalidIDs(t *testing.T) {
	d, _ := newTestDispatcher()

	envelope, first := dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "table-1", Name: "主表"})
	if !envelope.OK {
		t.Fatalf("create envelope=%#v", envelope)
	}
	_ = first
	envelope, conflict := dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "table-1", Name: "重复"})
	if !envelope.OK || conflict["conflict"] != "table-1" {
		t.Fatalf("conflict envelope=%#v result=%#v", envelope, conflict)
	}

	envelope, invalid := dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "Table-1", Name: "大写"})
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("invalid id envelope=%#v result=%#v", envelope, invalid)
	}

	// 填满容量后拒绝新建。
	for index := 1; index < tt.MaxTimetablesPerUser; index++ {
		envelope, item := dispatch(d, capTimetableCreate, timetableCreateInput{Name: fmt.Sprintf("表%d", index)})
		if !envelope.OK {
			t.Fatalf("seed create %d envelope=%#v", index, envelope)
		}
		_ = item
	}
	envelope, capacity := dispatch(d, capTimetableCreate, timetableCreateInput{Name: "超额"})
	if !envelope.OK || capacity["capacity_exceeded"] != true {
		t.Fatalf("capacity envelope=%#v result=%#v", envelope, capacity)
	}
}

func TestCourseLifecycleAndUnknownCourse(t *testing.T) {
	d, _ := newTestDispatcher()
	_, created := dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "table-1", Name: "主表"})
	_ = created

	coursePayload := courseInput{TimetableID: "table-1", Title: "高数", Weekday: 1, ClassFrom: 1, ClassTo: 2, Weeks: []int{1, 2, 3}, Instructor: " 张老师 "}
	envelope, course := dispatch(d, capCourseCreate, coursePayload)
	if !envelope.OK {
		t.Fatalf("course create envelope=%#v", envelope)
	}
	encoded, _ := json.Marshal(course["course"])
	var item tt.Course
	if err := json.Unmarshal(encoded, &item); err != nil {
		t.Fatal(err)
	}
	if item.ID == "" || item.Instructor != "张老师" || !sameInts(item.Weeks, []int{1, 2, 3}) {
		t.Fatalf("course=%#v", item)
	}

	// 同 ID 再建是冲突；更新替换字段；删除幂等返回 deleted=false。
	envelope, _ = dispatch(d, capCourseCreate, courseInput{TimetableID: "table-1", CourseID: item.ID, Title: "重复", Weekday: 1, ClassFrom: 1, ClassTo: 2, Weeks: []int{1}})
	if !envelope.OK {
		t.Fatalf("conflict envelope=%#v", envelope)
	}
	envelope, updated := dispatch(d, capCourseUpdate, courseInput{TimetableID: "table-1", CourseID: item.ID, Title: "线性代数", Weekday: 2, ClassFrom: 3, ClassTo: 4, Weeks: []int{5}})
	if !envelope.OK {
		t.Fatalf("update envelope=%#v", envelope)
	}
	encoded, _ = json.Marshal(updated["course"])
	if err := json.Unmarshal(encoded, &item); err != nil {
		t.Fatal(err)
	}
	if item.Title != "线性代数" || item.Weekday != 2 {
		t.Fatalf("updated course=%#v", item)
	}
	envelope, deleted := dispatch(d, capCourseDelete, courseGetInput{TimetableID: "table-1", CourseID: item.ID})
	if !envelope.OK || deleted["deleted"] != true {
		t.Fatalf("delete envelope=%#v result=%#v", envelope, deleted)
	}
	envelope, deleted = dispatch(d, capCourseDelete, courseGetInput{TimetableID: "table-1", CourseID: item.ID})
	if !envelope.OK || deleted["deleted"] != false {
		t.Fatalf("idempotent delete envelope=%#v result=%#v", envelope, deleted)
	}

	// 不存在的课表：课程读/list 均带内 not found；非法 ID 是 invalid_argument。
	envelope, missing := dispatch(d, capCourseGet, courseGetInput{TimetableID: "table-1", CourseID: "ghost"})
	if !envelope.OK || missing["found"] != false {
		t.Fatalf("missing course envelope=%#v result=%#v", envelope, missing)
	}
	envelope, missing = dispatch(d, capCourseList, idInput{TimetableID: "ghost"})
	if !envelope.OK || missing["found"] != false {
		t.Fatalf("missing table envelope=%#v result=%#v", envelope, missing)
	}
	envelope = d.Dispatch(capCourseGet, []byte(`{"timetable_id":"BAD ID","course_id":"x"}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("invalid ids envelope=%#v", envelope)
	}
}

func TestImportAcademicAllOrNothing(t *testing.T) {
	d, store := newTestDispatcher()

	envelope, result := dispatch(d, capImport, importInput{Format: "wuda", Content: academicPayload, FileName: "教务.csv"})
	if !envelope.OK {
		t.Fatalf("import envelope=%#v", envelope)
	}
	table := mustTable(t, result)
	if table.Source != tt.SourceWUDA || table.Name != "教务" {
		t.Fatalf("imported table=%#v", table)
	}
	courses, ok := result["courses"].([]any)
	if !ok || len(courses) != 2 {
		t.Fatalf("imported courses=%#v", result["courses"])
	}
	collection := fmt.Sprintf(coursesCollectionFmt, table.ID)
	if count := countDocs(store, collection); count != 2 {
		t.Fatalf("collection count=%d", count)
	}
	// 课程 doc ID 是确定性的：同载荷重放应生成同一集合内容。
	firstCourseID := docID(t, courses[0])
	if firstCourseID != "import-1" {
		t.Fatalf("first course id=%q", firstCourseID)
	}
}

func TestImportErrorsMapToClosedStatusSet(t *testing.T) {
	d, store := newTestDispatcher()
	cases := []struct {
		name   string
		input  importInput
		status string
	}{
		{"malformed", importInput{Format: "wuda", Content: `{"kbList":[{"kcmc":"高数"}]}`}, "malformed_data"},
		{"unsupported", importInput{Format: "ics", Content: "x"}, "unsupported_format"},
		{"no_courses", importInput{Format: "csv", Content: "name,day,startNode,endNode,teacher,location,weekMeta"}, "no_courses"},
		{"invalid", importInput{Format: "legacy", Content: "only\nthree\nlines"}, "malformed_data"},
	}
	for _, tc := range cases {
		envelope, result := dispatch(d, capImport, tc.input)
		if !envelope.OK {
			t.Fatalf("%s envelope=%#v", tc.name, envelope)
		}
		if result["status"] != tc.status {
			t.Fatalf("%s status=%v, want %s", tc.name, result["status"], tc.status)
		}
	}
	// 超限在分发前拒绝（非格式错误）。
	envelope, result := dispatch(d, capImport, importInput{Format: "csv", Content: strings.Repeat("a", tt.MaxImportBytes+1)})
	if !envelope.OK || result["status"] != "too_large" {
		t.Fatalf("too_large envelope=%#v result=%#v", envelope, result)
	}
	// 失败导入不得留下任何文档。
	if docs := len(store.docs); docs != 0 {
		t.Fatalf("failed import left %d docs", docs)
	}
}

func TestImportConflictKeepsExistingTable(t *testing.T) {
	d, store := newTestDispatcher()
	_, created := dispatch(d, capTimetableCreate, timetableCreateInput{TimetableID: "table-1", Name: "手动"})
	_ = created
	envelope, result := dispatch(d, capImport, importInput{Format: "wuda", Content: academicPayload, TimetableID: "table-1"})
	if !envelope.OK || result["status"] != "conflict" {
		t.Fatalf("conflict import envelope=%#v result=%#v", envelope, result)
	}
	if count := countDocs(store, "tables"); count != 1 {
		t.Fatalf("tables count=%d", count)
	}
}

func TestDispatchRejectsUnknownCapabilityAndMalformedPayload(t *testing.T) {
	d, _ := newTestDispatcher()
	envelope := d.Dispatch("timetable.drop", []byte(`{}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("unknown capability envelope=%#v", envelope)
	}
	envelope = d.Dispatch(capTimetableGet, []byte(`{"timetable_id":1}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("malformed payload envelope=%#v", envelope)
	}
	envelope = d.Dispatch(capTimetableGet, []byte(`{"timetable_id":"t1","extra":true}`))
	if envelope.OK || envelope.Code != guestkit.CodeInvalidArgument {
		t.Fatalf("unknown field envelope=%#v", envelope)
	}
}

func countDocs(store *memoryStore, collection string) int {
	return len(store.docs[collection])
}

func sameInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func docID(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var item tt.Course
	if err := json.Unmarshal(encoded, &item); err != nil {
		t.Fatal(err)
	}
	return item.ID
}
