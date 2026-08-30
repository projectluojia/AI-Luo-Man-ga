package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/campustest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/sdkgen"
)

type sdkTestWebAuthenticator struct{}

func (sdkTestWebAuthenticator) Authenticate(request *http.Request) (web.AuthenticatedWebIdentity, error) {
	if request.Header.Get("Authorization") != "Bearer sdk-test" {
		return web.AuthenticatedWebIdentity{}, errors.New("missing test authorization")
	}
	return web.AuthenticatedWebIdentity{
		PlatformSpaceID: "web", PlatformUserID: "web-test-subject", PlatformSessionID: "web-test-session",
	}, nil
}

// TestGeneratedGoSDKInvokesRealCapability 是端到端真实链路：
// 真实 ailuo.toml 契约 → sdkgen 生成 Go SDK → 临时模块编译运行 →
// httptest 启动 web.Server（装配 registry + Dispatcher + 真实 hosted campus
// wasm 与权威 store）→ SDK 调用 invoke 端点 → 断言返回正确行程。
// 全程不 mock 被调函数：capability 由 campustest 按安装目录路径注册。
func TestGeneratedGoSDKInvokesRealCapability(t *testing.T) {
	// 1. 装配真实端点 + 权威契约（多语言端到端共用）。
	testServer, extensions := newCampusE2E(t)
	defer testServer.Close()

	// 2. 生成 Go SDK。
	files, err := sdkgen.Generate(extensions, sdkgen.Options{Language: sdkgen.LanguageGo, PackageID: campus.ServiceID})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// 5. 临时模块：生成的 client + 调用 journeys.search 的 main。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module sdk-e2e\n\ngo 1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "campus"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "campus", "client.go"), files[0].Code, 0644); err != nil {
		t.Fatal(err)
	}
	main := `package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"sdk-e2e/campus"
)

func main() {
	client := campus.NewClient(os.Args[1], campus.WithHeaderProvider(func(_ context.Context, request *http.Request) error {
		request.Header.Set("Authorization", "Bearer sdk-test")
		return nil
	}))
	// depart_after 传过去时间，确保覆盖 store 中所有行程
	// （guest 对缺省值会归一化为 now，过滤掉更早的行程）。
	departAfter := time.Now().Add(-400 * 24 * time.Hour)
	result, err := client.BusJourneysSearch(context.Background(), campus.BusJourneysSearchInput{
		OriginStopID: "stop-a", DestinationStopID: "stop-b",
		DepartAfter: &departAfter,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "invoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(result))
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(main), 0644); err != nil {
		t.Fatal(err)
	}

	// 6. 编译运行生成的 SDK 调用真实端点（绑定超时上下文，编译挂住时子进程被杀）。
	runCtx, cancelRun := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelRun()
	cmd := exec.CommandContext(runCtx, "go", "run", ".", testServer.URL)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run 生成的 SDK 失败: %v\n%s", err, output)
	}
	var result struct {
		Journeys []struct {
			TripID string `json:"trip_id"`
		} `json:"journeys"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("解析 SDK 返回失败: %v\n%s", err, output)
	}
	if len(result.Journeys) != 2 || result.Journeys[0].TripID != "trip-early" || result.Journeys[1].TripID != "trip-late" {
		t.Fatalf("journeys = %#v, want trip-early then trip-late", result.Journeys)
	}
}

// TestInvokeEndpointRejectsUnknownCapability 验证生成 SDK 面对不存在 capability
// 时收到稳定 404 错误（HTTP 信封在 SDK 传输层被正确识别）。
func TestInvokeEndpointRejectsUnknownCapability(t *testing.T) {
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	server := web.NewServer(
		newEchoAdmission(&fakeOrchestrator{}, testController{}), nil, nil,
		reg, policy, campus.AppID, nil, testController{}, access.NewEventHub(),
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithIdentityResolver(testWebResolver{}),
		web.WithDispatcher(dispatcher),
	)
	response := invokeRequest(t, server.Handler(), "campus.bus.missing", `{"input":{}}`)
	if response.Code != 404 || !strings.Contains(response.Body.String(), `"code":"capability_not_found"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// journeyDoc 是行程文档（campus/bus journeys 集合的领域契约形状）。
type journeyDoc struct {
	TripID            string    `json:"trip_id"`
	RouteID           string    `json:"route_id"`
	RouteName         string    `json:"route_name"`
	Direction         string    `json:"direction"`
	OriginStopID      string    `json:"origin_stop_id"`
	OriginStopName    string    `json:"origin_stop_name"`
	DestinationStopID string    `json:"destination_stop_id"`
	DestinationName   string    `json:"destination_stop_name"`
	DepartureAt       time.Time `json:"departure_at"`
	ArrivalAt         time.Time `json:"arrival_at"`
	SourceRevision    string    `json:"source_revision"`
}

// journeyFixture 构造测试行程。
func journeyFixture(id, origin, destination string, departure time.Time) journeyDoc {
	return journeyDoc{
		TripID:            id,
		RouteID:           "route-1",
		RouteName:         "文理学部-信息学部",
		Direction:         "outbound",
		OriginStopID:      origin,
		OriginStopName:    origin,
		DestinationStopID: destination,
		DestinationName:   destination,
		DepartureAt:       departure,
		ArrivalAt:         departure.Add(20 * time.Minute),
		SourceRevision:    "e2e-revision",
	}
}

// newCampusE2E 装配多语言端到端共享环境：权威数据播种 + 真实 hosted campus
// 注册 + httptest 端点 + campus 权威契约。返回 HTTP 端点与生成 SDK 的契约输入。
// 全程不 mock 被调函数（campustest 现场编译真实 wasm 并经 ailuo.store 读取）。
func newCampusE2E(t *testing.T) (*httptest.Server, json.RawMessage) {
	t.Helper()
	// 权威数据播种：快照元数据与行程文档经 packstore 一次性原子写入。
	docs := memory.NewDocuments()
	now := time.Now().UTC()
	scope := packstore.Scope{AppID: campus.AppID, Namespace: campus.StorageNamespace}
	baseTime := time.Date(2026, time.July, 24, 8, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	revision := "e2e-revision"
	journeys := []journeyDoc{
		journeyFixture("trip-early", "stop-a", "stop-b", baseTime.Add(30*time.Minute)),
		journeyFixture("trip-late", "stop-a", "stop-b", baseTime.Add(90*time.Minute)),
	}
	encoded := make([]packstore.Document, 0, len(journeys))
	for _, journey := range journeys {
		payload, err := json.Marshal(journey)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, packstore.Document{ID: journey.TripID, Payload: payload})
	}
	if err := docs.ReplaceSnapshot(t.Context(), scope, packstore.SnapshotMeta{
		Revision: revision, Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}, map[string][]packstore.Document{"journeys": encoded}); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	campustest.RegisterHosted(t, reg, docs)
	policy := runtimetest.NewStaticAppPolicy()
	for _, capabilityID := range []string{
		campus.BusStopSearchCapabilityID, campus.BusRouteListCapabilityID, campus.BusJourneySearchCapabilityID,
	} {
		policy.Enable(campus.AppID, capabilityID)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})
	server := web.NewServer(
		newEchoAdmission(&fakeOrchestrator{}, testController{}), nil, nil,
		reg, policy, campus.AppID, nil, testController{}, access.NewEventHub(),
		web.WithWebAuthenticator(sdkTestWebAuthenticator{}),
		web.WithIdentityResolver(testWebResolver{}),
		web.WithDispatcher(dispatcher),
	)
	testServer := httptest.NewServer(server.Handler())
	extensions, err := campus.Extensions()
	if err != nil {
		testServer.Close()
		t.Fatalf("构造 extensions: %v", err)
	}
	return testServer, extensions
}
