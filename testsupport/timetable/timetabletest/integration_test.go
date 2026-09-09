//go:build integration

// 课表 hosted 包端到端：真实 ailuo.toml 清单 + 真实 packages/timetable guest
// 源码现场编译 + packstore 个人作用域。覆盖创建/列表/课程写入/删除与
// 幂等键/确认门槛治理，全部经真实 Dispatcher 驱动。
package timetabletest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/hostedtest"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/timetable/timetabletest"
)

func newTimetableDispatcher(t *testing.T) *runtime.Dispatcher {
	t.Helper()
	reg := registry.New()
	store := hostedtest.MemoryStore()
	timetabletest.RegisterHosted(t, reg, store)
	return hostedtest.NewDispatcher(t, reg, timetabletest.PackageID, timetabletest.CapabilityIDs())
}

func invoke(t *testing.T, d *runtime.Dispatcher, capabilityID, payload, idempotencyKey, confirmationID string) (bool, json.RawMessage, string) {
	t.Helper()
	return hostedtest.Invoke(t, d, timetabletest.PackageID, capabilityID, payload, idempotencyKey, confirmationID)
}

// TestHostedTimetableLifecycle 经真实 wasm guest 走通课表生命周期：
// 创建 → 课程写入 → 列表 → 读取 → 激活 → 删除。
func TestHostedTimetableLifecycle(t *testing.T) {
	d := newTimetableDispatcher(t)

	ok, result, errText := invoke(t, d, timetabletest.CapabilityTimetableCreate,
		`{"name":"期中","active":true}`, "idem-create", "")
	if !ok {
		t.Fatalf("create failed: %s", errText)
	}
	var created struct {
		Timetable struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Source string `json:"source"`
			Active bool   `json:"active"`
		} `json:"timetable"`
	}
	if err := json.Unmarshal(result, &created); err != nil {
		t.Fatalf("create result %s: %v", result, err)
	}
	if created.Timetable.ID == "" || created.Timetable.Source != "local" || !created.Timetable.Active {
		t.Fatalf("created = %#v", created.Timetable)
	}
	tableID := created.Timetable.ID

	ok, result, errText = invoke(t, d, timetabletest.CapabilityCourseCreate,
		`{"timetable_id":"`+tableID+`","title":"高数","weekday":1,"class_from":1,"class_to":2,"weeks":[1,2,3]}`, "idem-course", "")
	if !ok {
		t.Fatalf("course create failed: %s", errText)
	}
	var courseResult struct {
		Course struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"course"`
	}
	if err := json.Unmarshal(result, &courseResult); err != nil {
		t.Fatalf("course create result %s: %v", result, err)
	}
	if courseResult.Course.ID == "" || courseResult.Course.Title != "高数" {
		t.Fatalf("course = %#v", courseResult.Course)
	}

	ok, result, errText = invoke(t, d, timetabletest.CapabilityCourseList,
		`{"timetable_id":"`+tableID+`"}`, "", "")
	if !ok {
		t.Fatalf("course list failed: %s", errText)
	}
	var listResult struct {
		Courses []struct {
			Title string `json:"title"`
		} `json:"courses"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		t.Fatalf("course list result %s: %v", result, err)
	}
	if len(listResult.Courses) != 1 || listResult.Courses[0].Title != "高数" {
		t.Fatalf("courses = %#v", listResult.Courses)
	}

	ok, _, errText = invoke(t, d, timetabletest.CapabilityTimetableDelete,
		`{"timetable_id":"`+tableID+`"}`, "idem-delete", "")
	if ok {
		t.Fatalf("delete without confirmation should fail, got success")
	}
	if !strings.Contains(errText, runtime.ErrConfirmationRequired.Error()) {
		t.Fatalf("delete error = %q, want confirmation required", errText)
	}

	ok, _, errText = invoke(t, d, timetabletest.CapabilityTimetableDelete,
		`{"timetable_id":"`+tableID+`"}`, "idem-delete", "confirm-1")
	if !ok {
		t.Fatalf("delete with confirmation failed: %s", errText)
	}

	// 删除后读取按带内 not-found 应答。
	ok, result, errText = invoke(t, d, timetabletest.CapabilityTimetableGet,
		`{"timetable_id":"`+tableID+`"}`, "", "")
	if !ok {
		t.Fatalf("get after delete failed: %s", errText)
	}
	var getResult struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(result, &getResult); err != nil {
		t.Fatalf("get result %s: %v", result, err)
	}
	if getResult.Found {
		t.Fatalf("deleted timetable still readable: %s", result)
	}
}

// TestHostedTimetableImportAcademic 经真实 wasm guest 导入武大教务契约内容。
func TestHostedTimetableImportAcademic(t *testing.T) {
	d := newTimetableDispatcher(t)
	payload := `{"format":"wuda","content":"{\"kbList\":[{\"kcmc\":\"高数\",\"xqjmc\":\"星期三\",\"jcs\":\"3-4\",\"zcd\":\"1-4周\",\"xm\":\"张老师\",\"cdmc\":\"教一101\"}]}","fileName":"教务.csv"}`
	ok, result, errText := invoke(t, d, timetabletest.CapabilityImport, payload, "idem-import", "")
	if !ok {
		t.Fatalf("import failed: %s", errText)
	}
	var importResult struct {
		Timetable struct {
			ID     string `json:"id"`
			Source string `json:"source"`
			Name   string `json:"name"`
		} `json:"timetable"`
		Courses []struct {
			Title string `json:"title"`
			Weeks []int  `json:"weeks"`
		} `json:"courses"`
	}
	if err := json.Unmarshal(result, &importResult); err != nil {
		t.Fatalf("import result %s: %v", result, err)
	}
	if importResult.Timetable.Source != "wuda" || importResult.Timetable.Name != "教务" || len(importResult.Courses) != 1 {
		t.Fatalf("import result = %s", result)
	}
	if course := importResult.Courses[0]; course.Title != "高数" || len(course.Weeks) != 4 {
		t.Fatalf("course = %#v", course)
	}
}

// TestHostedTimetableWriteRequiresIdempotencyKey 验证用户作用域写入的治理
// 门槛在端到端链路生效：无幂等键的写经 packstore 宿主函数被拒绝。
func TestHostedTimetableWriteRequiresIdempotencyKey(t *testing.T) {
	d := newTimetableDispatcher(t)
	_, _, createErr := invoke(t, d, timetabletest.CapabilityTimetableCreate, `{"name":"期中"}`, "", "")
	if createErr == "" {
		t.Fatal("write without idempotency key should fail")
	}
	// dispatcher 层先拦截（idempotency key is required for side effects），不经 guest。
	if !strings.Contains(createErr, runtime.ErrIdempotencyKeyRequired.Error()) {
		t.Fatalf("error = %q, want idempotency key required", createErr)
	}
}
