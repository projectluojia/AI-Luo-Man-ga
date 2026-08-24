package web_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus/campustest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/bus"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/sdkgen"
)

// TestGeneratedGoSDKInvokesRealCapability 是端到端真实链路：
// 真实 ailuo.toml 契约 → sdkgen 生成 Go SDK → 临时模块编译运行 →
// httptest 启动 web.Server（装配 registry + Dispatcher + 真实 hosted campus
// wasm 与权威 store）→ SDK 调用 invoke 端点 → 断言返回正确行程。
// 全程不 mock 被调函数：capability 由 campustest 按安装目录路径注册。
func TestGeneratedGoSDKInvokesRealCapability(t *testing.T) {
	// 1. 权威数据 store（必须先设 metadata 再 Replace：Replace 内部以
	// metadata.Revision 覆写行程 SourceRevision，顺序颠倒会不匹配）。
	store := memory.NewBusStore()
	now := time.Now().UTC()
	store.SetSnapshotMetadata(campus.AppID, bus.SnapshotMetadata{
		Revision: "e2e-revision", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	})
	baseTime := time.Date(2026, time.July, 24, 8, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	store.Replace(campus.AppID, []bus.Journey{
		journeyFixture("trip-early", "stop-a", "stop-b", baseTime.Add(30*time.Minute)),
		journeyFixture("trip-late", "stop-a", "stop-b", baseTime.Add(90*time.Minute)),
	})

	// 2. 注册真实 hosted campus（安装目录路径）。
	reg := registry.New()
	campustest.RegisterHosted(t, reg, store)
	policy := runtimetest.NewStaticAppPolicy()
	for _, capabilityID := range []string{
		campus.BusStopSearchCapabilityID, campus.BusRouteListCapabilityID, campus.BusJourneySearchCapabilityID,
	} {
		policy.Enable(campus.AppID, capabilityID)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{})

	// 3. httptest 启动真实 HTTP 端点。
	server := web.NewServer(
		context.Background(), &fakeOrchestrator{}, nil, nil,
		reg, policy, campus.AppID, nil,
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithDispatcher(dispatcher),
	)
	testServer := httptest.NewServer(server.Handler())
	defer testServer.Close()

	// 4. 从真实 ailuo.toml 契约生成 Go SDK。
	manifest, _, _, err := packagefmt.Parse("../../../extensions/campus.bus/ailuo.toml")
	if err != nil {
		t.Fatalf("Parse ailuo.toml: %v", err)
	}
	files, err := sdkgen.Generate(manifest.Extensions, sdkgen.Options{Language: sdkgen.LanguageGo, PackageID: manifest.ID})
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
	"os"
	"time"

	"sdk-e2e/campus"
)

func main() {
	client := campus.NewClient(os.Args[1])
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

	// 6. 编译运行生成的 SDK 调用真实端点。
	cmd := exec.Command("go", "run", ".", testServer.URL)
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
		context.Background(), &fakeOrchestrator{}, nil, nil,
		reg, policy, campus.AppID, nil,
		web.WithWebAuthenticator(testWebAuthenticator{}),
		web.WithDispatcher(dispatcher),
	)
	response := invokeRequest(t, server.Handler(), "campus.bus.missing", `{"input":{}}`)
	if response.Code != 404 || !strings.Contains(response.Body.String(), `"code":"capability_not_found"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// journeyFixture 构造测试行程（与 hosted_test 同形）。
func journeyFixture(id, origin, destination string, departure time.Time) bus.Journey {
	return bus.Journey{
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
