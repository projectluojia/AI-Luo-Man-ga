package agenthost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	agentv1 "github.com/projectluojia/AI-Luo-Man-ga/gen/agentv1"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/agenthost"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/agentprotocol"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
)

type healthClient struct {
	response *agentv1.HealthResponse
	err      error
	request  *agentv1.HealthRequest
}

func (c *healthClient) Health(_ context.Context, request *agentv1.HealthRequest, _ ...grpc.CallOption) (*agentv1.HealthResponse, error) {
	c.request = request
	return c.response, c.err
}

func (c *healthClient) Run(context.Context, ...grpc.CallOption) (agentv1.AgentRuntime_RunClient, error) {
	return nil, errors.New("not implemented")
}

func TestHealthCheckerProjectsModelAndProtocol(t *testing.T) {
	t.Parallel()

	client := &healthClient{response: &agentv1.HealthResponse{
		Ready:                     true,
		SupportedProtocolVersions: []string{agentprotocol.Version},
	}}
	checker := agenthost.NewHealthChecker(client, "test-model")
	if err := checker.Ping(context.Background()); err != nil {
		t.Fatalf("health check: %v", err)
	}
	if client.request.GetModel() != "test-model" ||
		len(client.request.GetAcceptedProtocolVersions()) != 1 ||
		client.request.GetAcceptedProtocolVersions()[0] != agentprotocol.Version {
		t.Fatalf("health request=%#v", client.request)
	}
}

func TestHealthCheckerRejectsVersionMismatchAndProviderUnready(t *testing.T) {
	t.Parallel()

	mismatch := agenthost.NewHealthChecker(&healthClient{response: &agentv1.HealthResponse{
		Ready:                     true,
		SupportedProtocolVersions: []string{"3.0"},
	}}, "test-model")
	if err := mismatch.Ping(context.Background()); !errors.Is(err, agentprotocol.ErrVersionMismatch) {
		t.Fatalf("version mismatch error=%v", err)
	}

	unready := agenthost.NewHealthChecker(&healthClient{response: &agentv1.HealthResponse{
		SupportedProtocolVersions: []string{agentprotocol.Version},
		StatusCode:                "provider_unavailable",
	}}, "test-model")
	if err := unready.Ping(context.Background()); err == nil {
		t.Fatal("provider-unready response unexpectedly passed")
	}
}

func TestAppHealthCheckerUsesCurrentModelAndFailsClosed(t *testing.T) {
	client := &healthClient{response: &agentv1.HealthResponse{
		Ready:                     true,
		SupportedProtocolVersions: []string{agentprotocol.Version},
	}}
	current := healthAppConfig()
	var sourceErr error
	source := appConfigSource{
		current: func(context.Context, string) (appconfig.Config, error) {
			if sourceErr != nil {
				return appconfig.Config{}, sourceErr
			}
			return appconfig.Normalize(current)
		},
	}
	checker := agenthost.NewAppHealthChecker(client, source, "app")
	if err := checker.Ping(t.Context()); err != nil || client.request.GetModel() != "model-a" {
		t.Fatalf("first check request=%#v err=%v", client.request, err)
	}
	current.Model = "model-b"
	if err := checker.Ping(t.Context()); err != nil || client.request.GetModel() != "model-b" {
		t.Fatalf("updated check request=%#v err=%v", client.request, err)
	}
	current.Enabled = false
	if err := checker.Ping(t.Context()); err == nil {
		t.Fatal("disabled App unexpectedly passed readiness")
	}
	current.Enabled = true
	sourceErr = errors.New("private config storage failure")
	if err := checker.Ping(t.Context()); !errors.Is(err, sourceErr) {
		t.Fatalf("configuration failure error=%v", err)
	}
}

type appConfigSource struct {
	current func(context.Context, string) (appconfig.Config, error)
}

func (s appConfigSource) Current(ctx context.Context, appID string) (appconfig.Config, error) {
	return s.current(ctx, appID)
}

func (appConfigSource) Revision(context.Context, string, string) (appconfig.Config, error) {
	return appconfig.Config{}, appconfig.ErrNotFound
}

func healthAppConfig() appconfig.Config {
	return appconfig.Config{
		AppID: "app", Generation: 1, Enabled: true, Model: "model-a", SystemPrompt: "系统提示",
		Timezone: "Asia/Shanghai", MaxSteps: 8, MaxToolCalls: 8,
		MaxInputTokens: 32768, MaxOutputTokens: 8192, MaxTotalTokens: 40960,
		MaxOutputBytes: 65536, ProviderTimeout: 30 * time.Second,
	}
}
