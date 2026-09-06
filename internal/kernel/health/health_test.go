package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"

	"google.golang.org/grpc"
)

// healthClient 是 executor.Client 的最小测试实现（只实现 Health）。
type healthClient struct {
	response *executor.HealthResponse
	err      error
}

func (h *healthClient) Health(_ context.Context, _ *executor.HealthRequest, _ ...grpc.CallOption) (*executor.HealthResponse, error) {
	return h.response, h.err
}

func (h *healthClient) Run(_ context.Context, _ ...grpc.CallOption) (executor.RunStream, error) {
	return nil, errors.New("run not supported in health test")
}

func TestExecutorCheckerReportsProviderReadiness(t *testing.T) {
	checker := ExecutorChecker{Client: &healthClient{response: &executor.HealthResponse{
		Ready: true, Provider: "test", SupportedProtocolVersions: []string{executor.Version},
	}}, Model: "test-model"}
	if err := checker.Ping(context.Background()); err != nil {
		t.Fatalf("ready executor rejected: %v", err)
	}
}

func TestExecutorCheckerRejectsVersionMismatch(t *testing.T) {
	checker := ExecutorChecker{Client: &healthClient{response: &executor.HealthResponse{
		Ready: true, SupportedProtocolVersions: []string{"9.9"},
	}}, Model: "test-model"}
	if err := checker.Ping(context.Background()); !errors.Is(err, executor.ErrVersionMismatch) {
		t.Fatalf("mismatch error = %v, want ErrVersionMismatch", err)
	}
}

func TestExecutorCheckerRejectsUnreadyExecutor(t *testing.T) {
	checker := ExecutorChecker{Client: &healthClient{response: &executor.HealthResponse{
		Ready: false, SupportedProtocolVersions: []string{executor.Version},
	}}, Model: "test-model"}
	if err := checker.Ping(context.Background()); err == nil {
		t.Fatal("unready executor must fail")
	}
}

func TestExecutorCheckerPropagatesRPCError(t *testing.T) {
	checker := ExecutorChecker{Client: &healthClient{err: errors.New("boom")}, Model: "test-model"}
	if err := checker.Ping(context.Background()); err == nil {
		t.Fatal("rpc error must propagate")
	}
}

// staticSource 是 appconfig.Source 的最小测试实现。
type staticSource struct {
	config appconfig.Config
	err    error
}

func (s staticSource) Current(context.Context, string) (appconfig.Config, error) {
	return s.config, s.err
}

func (s staticSource) Revision(context.Context, string, string) (appconfig.Config, error) {
	return s.config, s.err
}

func TestExecutorAppCheckerUsesCurrentAppModel(t *testing.T) {
	checkedModel := ""
	client := &capturingHealthClient{respond: func(request *executor.HealthRequest) *executor.HealthResponse {
		checkedModel = request.Model
		return &executor.HealthResponse{
			Ready: true, Provider: "test", SupportedProtocolVersions: []string{executor.Version},
		}
	}}
	config, err := appconfig.Normalize(appconfig.Config{
		AppID: "campus-services", Enabled: true, Model: "current-model",
		SystemPrompt: "系统提示", Timezone: "Asia/Shanghai",
		MaxSteps: 8, MaxCapabilityCalls: 8, MaxInputTokens: 32768, MaxOutputTokens: 8192,
		MaxTotalTokens: 40960, MaxOutputBytes: 65536, ProviderTimeout: 30 * time.Second,
		EnabledCapabilities: []string{"campus.bus.routes.list"},
	})
	if err != nil {
		t.Fatal(err)
	}
	config.Generation = 1
	checker := ExecutorAppChecker{Client: client, Source: staticSource{config: config}, AppID: "campus-services"}
	if err := checker.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if checkedModel != "current-model" {
		t.Fatalf("checked model = %q, want current-model", checkedModel)
	}
}

func TestExecutorAppCheckerRejectsDisabledOrInvalidApp(t *testing.T) {
	disabled := staticSource{config: appconfig.Config{AppID: "campus-services", Enabled: false, Model: "m", Generation: 1}}
	if err := (ExecutorAppChecker{Client: &healthClient{}, Source: disabled, AppID: "campus-services"}).Ping(context.Background()); err == nil {
		t.Fatal("disabled app must fail readiness")
	}
	invalid := staticSource{config: appconfig.Config{AppID: "campus-services", Enabled: true, Model: "m"}}
	if err := (ExecutorAppChecker{Client: &healthClient{}, Source: invalid, AppID: "campus-services"}).Ping(context.Background()); err == nil {
		t.Fatal("invalid app config must fail readiness")
	}
	missing := staticSource{err: errors.New("unavailable")}
	if err := (ExecutorAppChecker{Client: &healthClient{}, Source: missing, AppID: "campus-services"}).Ping(context.Background()); err == nil {
		t.Fatal("source error must fail readiness")
	}
}

// capturingHealthClient 记录 Health 请求并返回注入的响应。
type capturingHealthClient struct {
	respond func(*executor.HealthRequest) *executor.HealthResponse
}

func (c *capturingHealthClient) Health(_ context.Context, request *executor.HealthRequest, _ ...grpc.CallOption) (*executor.HealthResponse, error) {
	return c.respond(request), nil
}

func (c *capturingHealthClient) Run(_ context.Context, _ ...grpc.CallOption) (executor.RunStream, error) {
	return nil, errors.New("run not supported in health test")
}
