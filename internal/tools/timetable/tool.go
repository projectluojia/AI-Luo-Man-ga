package timetable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/jsonutil"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const (
	TimetableListToolID     = "timetable.list"
	TimetableGetToolID      = "timetable.get"
	TimetableCreateToolID   = "timetable.create"
	TimetableUpdateToolID   = "timetable.update"
	TimetableDeleteToolID   = "timetable.delete"
	TimetableActivateToolID = "timetable.activate"
	CourseListToolID        = "timetable.courses.list"
	CourseGetToolID         = "timetable.course.get"
	CourseCreateToolID      = "timetable.course.create"
	CourseUpdateToolID      = "timetable.course.update"
	CourseDeleteToolID      = "timetable.course.delete"
	TimetableImportToolID   = "timetable.import"

	TimetableListInputSchemaJSON   = `{"type":"object","additionalProperties":false}`
	TimetableIDInputSchemaJSON     = `{"type":"object","properties":{"timetable_id":{"type":"string","minLength":1,"maxLength":128}},"required":["timetable_id"],"additionalProperties":false}`
	TimetableCreateInputSchemaJSON = `{"type":"object","properties":{"timetable_id":{"type":"string","minLength":1,"maxLength":128},"name":{"type":"string","minLength":1,"maxLength":512},"active":{"type":"boolean"}},"required":["name"],"additionalProperties":false}`
	TimetableUpdateInputSchemaJSON = `{"type":"object","properties":{"timetable_id":{"type":"string","minLength":1,"maxLength":128},"name":{"type":"string","minLength":1,"maxLength":512},"active":{"type":"boolean"}},"required":["timetable_id","name"],"additionalProperties":false}`
	CourseInputSchemaJSON          = `{"type":"object","properties":{"timetable_id":{"type":"string","minLength":1,"maxLength":128},"course_id":{"type":"string","minLength":1,"maxLength":128},"title":{"type":"string","minLength":1,"maxLength":256},"weekday":{"type":"integer","minimum":1,"maximum":7},"class_from":{"type":"integer","minimum":1,"maximum":64},"class_to":{"type":"integer","minimum":1,"maximum":64},"weeks":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"integer","minimum":1,"maximum":64}},"course_nature":{"type":"string","maxLength":512},"instructor":{"type":"string","maxLength":512},"location":{"type":"string","maxLength":512},"week_meta":{"type":"string","maxLength":512},"start_text":{"type":"string","maxLength":512},"end_text":{"type":"string","maxLength":512},"external_id":{"type":"string","maxLength":512}},"required":["timetable_id","title","weekday","class_from","class_to","weeks"],"additionalProperties":false}`
	CourseGetInputSchemaJSON       = `{"type":"object","properties":{"timetable_id":{"type":"string","minLength":1,"maxLength":128},"course_id":{"type":"string","minLength":1,"maxLength":128}},"required":["timetable_id","course_id"],"additionalProperties":false}`
	ImportInputSchemaJSON          = `{"type":"object","properties":{"format":{"type":"string","enum":["wuda","academic","whu","wakeup","csv","legacy"]},"content":{"type":"string","minLength":1,"maxLength":65536},"fileName":{"type":"string","maxLength":512},"name":{"type":"string","maxLength":512},"timetable_id":{"type":"string","maxLength":128},"active":{"type":"boolean"}},"required":["format","content"],"additionalProperties":false}`
)

// 导出的 Schema 别名供 Service 注册使用，避免 Tool 与 Capability 元数据漂移。
const (
	TimetableListInputSchemaJSONExport   = TimetableListInputSchemaJSON
	TimetableIDInputSchemaJSONExport     = TimetableIDInputSchemaJSON
	TimetableCreateInputSchemaJSONExport = TimetableCreateInputSchemaJSON
	TimetableUpdateInputSchemaJSONExport = TimetableUpdateInputSchemaJSON
	CourseInputSchemaJSONExport          = CourseInputSchemaJSON
	CourseGetInputSchemaJSONExport       = CourseGetInputSchemaJSON
	ImportInputSchemaJSONExport          = ImportInputSchemaJSON
	CourseUpdateInputSchemaJSONExport    = CourseInputSchemaJSON
)

// ToolSpecs 返回课表原子 Tool 规格。Tool 与 Capability 的 JSON Schema 保持一致，
// Dispatcher 在两个边界分别复验参数。
func ToolSpecs() []registry.ToolSpec {
	read := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectRead}
	}
	write := func(id, description, schema string) registry.ToolSpec {
		return registry.ToolSpec{ID: id, Version: "1.0.0", Description: description, InputSchemaJSON: schema, SideEffect: registry.SideEffectWrite}
	}
	return []registry.ToolSpec{
		read(TimetableListToolID, "List the current user's timetables.", TimetableListInputSchemaJSON),
		read(TimetableGetToolID, "Get one timetable owned by the current user.", TimetableIDInputSchemaJSON),
		write(TimetableCreateToolID, "Create a local timetable for the current user.", TimetableCreateInputSchemaJSON),
		write(TimetableUpdateToolID, "Rename or activate a timetable owned by the current user.", TimetableUpdateInputSchemaJSON),
		write(TimetableDeleteToolID, "Delete a timetable and its courses.", TimetableIDInputSchemaJSON),
		write(TimetableActivateToolID, "Make one timetable active for the current user.", TimetableIDInputSchemaJSON),
		read(CourseListToolID, "List courses in one owned timetable.", TimetableIDInputSchemaJSON),
		read(CourseGetToolID, "Get one course in an owned timetable.", CourseGetInputSchemaJSON),
		write(CourseCreateToolID, "Create a course in an owned timetable.", CourseInputSchemaJSON),
		write(CourseUpdateToolID, "Replace one course in an owned timetable.", CourseInputSchemaJSON),
		write(CourseDeleteToolID, "Delete one course from an owned timetable.", CourseGetInputSchemaJSON),
		write(TimetableImportToolID, "Import a WuDa academic or WakeUp timetable.", ImportInputSchemaJSON),
	}
}

// ToolRegistrations 返回由统一存储端口驱动的 Tool 注册项。
func ToolRegistrations(store Store) []registry.ToolRegistration {
	handlers := ToolHandlers(store)
	registrations := make([]registry.ToolRegistration, 0, len(ToolSpecs()))
	for _, spec := range ToolSpecs() {
		registrations = append(registrations, registry.ToolRegistration{Spec: spec, Handler: handlers[spec.ID]})
	}
	return registrations
}

// ToolHandlers 构造原子 Tool 处理器；store 为空时仍返回处理器，但执行会安全失败。
func ToolHandlers(store Store) map[string]registry.Handler {
	return map[string]registry.Handler{
		TimetableListToolID:     listTimetablesHandler(store),
		TimetableGetToolID:      getTimetableHandler(store),
		TimetableCreateToolID:   createTimetableHandler(store),
		TimetableUpdateToolID:   updateTimetableHandler(store),
		TimetableDeleteToolID:   deleteTimetableHandler(store),
		TimetableActivateToolID: activateTimetableHandler(store),
		CourseListToolID:        listCoursesHandler(store),
		CourseGetToolID:         getCourseHandler(store),
		CourseCreateToolID:      createCourseHandler(store),
		CourseUpdateToolID:      updateCourseHandler(store),
		CourseDeleteToolID:      deleteCourseHandler(store),
		TimetableImportToolID:   importTimetableHandler(store),
	}
}

type timetableIDInput struct {
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

func requireUser(request contracts.RequestContext) error {
	if request.UserID == "" {
		return ErrUserRequired
	}
	if err := identity.ValidateUserID(request.UserID); err != nil {
		return ErrUserRequired
	}
	return nil
}

func decode(payload json.RawMessage, target any) error {
	if err := jsonutil.DecodeStrict(payload, target); err != nil {
		return errors.Join(registry.ErrSchemaValidation, err)
	}
	return nil
}

func ensureStore(store Store) error {
	if store == nil {
		return errors.New("timetable store is unavailable")
	}
	return nil
}

func listTimetablesHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input struct{}
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		items, err := store.ListTimetables(ctx, request.AppID, request.UserID)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []Timetable{}
		}
		return json.Marshal(map[string]any{"timetables": items})
	}
}

func getTimetableHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input timetableIDInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		item, err := store.GetTimetable(ctx, request.AppID, request.UserID, input.TimetableID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"timetable": item})
	}
}

func createTimetableHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input timetableCreateInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if input.TimetableID == "" {
			input.TimetableID = uuid.NewString()
		}
		item, err := store.CreateTimetable(ctx, Timetable{AppID: request.AppID, UserID: request.UserID, ID: input.TimetableID, Name: input.Name, Source: SourceLocal, Active: input.Active})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"timetable": item})
	}
}

func updateTimetableHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input timetableUpdateInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		item, err := store.GetTimetable(ctx, request.AppID, request.UserID, input.TimetableID)
		if err != nil {
			return nil, err
		}
		item.Name = input.Name
		if input.Active != nil {
			item.Active = *input.Active
		}
		item, err = store.UpdateTimetable(ctx, item)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"timetable": item})
	}
}

func deleteTimetableHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input timetableIDInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := store.DeleteTimetable(ctx, request.AppID, request.UserID, input.TimetableID); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"deleted":true}`), nil
	}
}

func activateTimetableHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input timetableIDInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		item, err := store.SetTimetableActive(ctx, request.AppID, request.UserID, input.TimetableID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"timetable": item})
	}
}

func listCoursesHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input timetableIDInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		items, err := store.ListCourses(ctx, request.AppID, request.UserID, input.TimetableID)
		if err != nil {
			return nil, err
		}
		if items == nil {
			items = []Course{}
		}
		return json.Marshal(map[string]any{"courses": items})
	}
}

func getCourseHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input courseGetInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		item, err := store.GetCourse(ctx, request.AppID, request.UserID, input.TimetableID, input.CourseID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"course": item})
	}
}

func courseFromInput(request contracts.RequestContext, input courseInput) Course {
	if input.CourseID == "" {
		input.CourseID = uuid.NewString()
	}
	return Course{AppID: request.AppID, UserID: request.UserID, TimetableID: input.TimetableID, ID: input.CourseID, Title: input.Title, Weekday: input.Weekday, ClassFrom: input.ClassFrom, ClassTo: input.ClassTo, Weeks: input.Weeks, CourseNature: input.CourseNature, Instructor: input.Instructor, Location: input.Location, WeekMeta: input.WeekMeta, StartText: input.StartText, EndText: input.EndText, ExternalID: input.ExternalID}
}

func createCourseHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input courseInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		item, err := store.CreateCourse(ctx, courseFromInput(request, input))
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"course": item})
	}
}

func updateCourseHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input courseInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if input.CourseID == "" {
			return nil, ErrInvalid
		}
		item, err := store.UpdateCourse(ctx, courseFromInput(request, input))
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"course": item})
	}
}

func deleteCourseHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input courseGetInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		if err := store.DeleteCourse(ctx, request.AppID, request.UserID, input.TimetableID, input.CourseID); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"deleted":true}`), nil
	}
}

func importTimetableHandler(store Store) registry.Handler {
	return func(ctx context.Context, request contracts.RequestContext, payload json.RawMessage) (json.RawMessage, error) {
		if err := requireUser(request); err != nil {
			return nil, err
		}
		if err := ensureStore(store); err != nil {
			return nil, err
		}
		var input importInput
		if err := decode(payload, &input); err != nil {
			return nil, err
		}
		data, err := parseCapabilityImport(input)
		if err != nil {
			return nil, err
		}
		if len(data.Courses) == 0 {
			return nil, ErrNoCourses
		}
		name := SanitizeDisplay(input.Name)
		if name == "" {
			name = SanitizeDisplay(data.Name)
		}
		if name == "" {
			name = "导入课表"
		}
		timetableID := strings.TrimSpace(input.TimetableID)
		if timetableID == "" {
			timetableID = uuid.NewString()
		}
		item := Timetable{AppID: request.AppID, UserID: request.UserID, ID: timetableID, Name: name, Source: data.Source, Active: input.Active}
		for index := range data.Courses {
			data.Courses[index].AppID = request.AppID
			data.Courses[index].UserID = request.UserID
			data.Courses[index].TimetableID = timetableID
			data.Courses[index].ID = uuid.NewString()
		}
		created, courses, err := store.ImportTimetable(ctx, item, data.Courses)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"timetable": created, "courses": courses})
	}
}

func parseCapabilityImport(input importInput) (ImportData, error) {
	format := strings.ToLower(strings.TrimSpace(input.Format))
	switch format {
	case "wuda", "academic", "whu":
		courses, err := ParseAcademic([]byte(input.Content))
		return ImportData{Name: fileBase(input.FileName), Source: SourceWUDA, Courses: courses}, err
	case "legacy":
		return ParseWakeUpEnvelope(marshalEnvelope("legacy", input.Content, input.FileName))
	case "wakeup", "csv":
		return ParseWakeUpEnvelope(marshalEnvelope("csv", input.Content, input.FileName))
	default:
		return ImportData{}, fmt.Errorf("%w: %s", ErrUnsupported, format)
	}
}

func marshalEnvelope(format, content, fileName string) []byte {
	encoded, _ := json.Marshal(struct {
		Format   string `json:"format"`
		Content  string `json:"content"`
		FileName string `json:"fileName"`
	}{format, content, fileName})
	return encoded
}
