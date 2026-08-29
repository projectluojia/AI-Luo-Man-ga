package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	runtimev1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/runtimev1"
	"github.com/projectluojia/weather/src/wx"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const testVersion = "1.0.0"

// stubService 只回显入参，用于协议层验证而不依赖真实供应商。
type stubService struct {
	fail    *wx.Error
	payload string
}

func (s *stubService) Current(_ context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	return json.RawMessage(s.payload), nil
}

func (s *stubService) Hourly(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	return s.Current(ctx, appID, payload)
}

func (s *stubService) AQI(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	return s.Current(ctx, appID, payload)
}

func (s *stubService) Alerts(ctx context.Context, appID string, payload json.RawMessage) (json.RawMessage, error) {
	return s.Current(ctx, appID, payload)
}

func startServer(t *testing.T, backend capabilityService) *runtimev1.RuntimeHostClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.MaxRecvMsgSize(512<<10), grpc.MaxSendMsgSize(512<<10))
	runtimev1.RegisterRuntimeHostServer(server, newHostServerWith(backend, func() {}))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(512<<10), grpc.MaxCallSendMsgSize(512<<10)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := runtimev1.NewRuntimeHostClient(conn)
	return &client
}

func identity() *runtimev1.RuntimeIdentity {
	return &runtimev1.RuntimeIdentity{RuntimeId: "weather.provider", Version: testVersion, ProtocolVersion: protocolVersion}
}

func governedContextProto(capability string) *runtimev1.GovernedRequestContext {
	return &runtimev1.GovernedRequestContext{
		AppId: "campus-services", EchoId: "echo-1", RequestId: "request-1",
		DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(),
		CallId:         "call-1", CapabilityId: capability,
	}
}

func TestHostLifecycleSequence(t *testing.T) {
	client := startServer(t, &stubService{payload: `{}`})
	describe, err := (*client).Describe(t.Context(), &runtimev1.DescribeRequest{Identity: identity()})
	if err != nil {
		t.Fatal(err)
	}
	if describe.RuntimeId != "weather.provider" || describe.Version != testVersion ||
		describe.Mode != packagecontract.ModeIsolated ||
		len(describe.SupportedProtocolVersions) != 1 || describe.SupportedProtocolVersions[0] != protocolVersion {
		t.Fatalf("describe=%+v", describe)
	}
	start, err := (*client).Start(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()})
	if err != nil {
		t.Fatal(err)
	}
	if !start.Ready || start.StatusCode != "ready" || start.Identity.ProtocolVersion != protocolVersion {
		t.Fatalf("start=%+v", start)
	}
	health, err := (*client).Health(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()})
	if err != nil || !health.Ready || health.StatusCode != "ready" {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	stop, err := (*client).Stop(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()})
	if err != nil || stop.Ready || stop.StatusCode != "stopped" {
		t.Fatalf("stop=%+v err=%v", stop, err)
	}
}

func TestHostInvokeRoundTripAndFailureMapping(t *testing.T) {
	client := startServer(t, &stubService{payload: `{"ok":true}`})
	if _, err := (*client).Start(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()}); err != nil {
		t.Fatal(err)
	}
	response, err := (*client).Invoke(t.Context(), &runtimev1.InvokeRequest{
		Identity: identity(), Context: governedContextProto("weather.current"), PayloadJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Success || !json.Valid(response.PayloadJson) || response.ErrorCode != "" || response.Retryable {
		t.Fatalf("response=%+v", response)
	}

	failing := startServer(t, &stubService{payload: `{}`, fail: &wx.Error{Code: wx.CodeDataUnavailable, Message: "down", Retryable: true}})
	if _, err := (*failing).Start(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()}); err != nil {
		t.Fatal(err)
	}
	response, err = (*failing).Invoke(t.Context(), &runtimev1.InvokeRequest{
		Identity: identity(), Context: governedContextProto("weather.aqi"), PayloadJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Success || response.ErrorCode != wx.CodeDataUnavailable || !response.Retryable || len(response.PayloadJson) != 0 {
		t.Fatalf("failure response=%+v", response)
	}
}

func TestHostInvokeRejectsBadPayloadAndUnknownCapability(t *testing.T) {
	client := startServer(t, &stubService{payload: `{}`})
	if _, err := (*client).Start(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()}); err != nil {
		t.Fatal(err)
	}
	// 未知能力：领域错误分类走 InvokeResponse 而非 RPC 错误。
	response, err := (*client).Invoke(t.Context(), &runtimev1.InvokeRequest{
		Identity: identity(), Context: governedContextProto("weather.other"), PayloadJson: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Success || response.ErrorCode != wx.CodeInvalidArguments {
		t.Fatalf("unknown capability response=%+v", response)
	}
	// 未知字段载荷由领域层严格解码拒绝（wx 包 service_test 覆盖）；协议层只验证
	// 传输层越界（非法 JSON）应答 InvalidArgument RPC 错误。
	_, err = (*client).Invoke(t.Context(), &runtimev1.InvokeRequest{
		Identity: identity(), Context: governedContextProto("weather.current"), PayloadJson: []byte(`not-json`),
	})
	if err == nil || !strings.Contains(err.Error(), "InvalidArgument") {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestHostInvokeRejectsExpiredDeadlineAndWrongIdentity(t *testing.T) {
	client := startServer(t, &stubService{payload: `{}`})
	if _, err := (*client).Start(t.Context(), &runtimev1.LifecycleRequest{Identity: identity()}); err != nil {
		t.Fatal(err)
	}
	expired := governedContextProto("weather.current")
	expired.DeadlineUnixMs = time.Now().Add(-time.Minute).UnixMilli()
	if _, err := (*client).Invoke(t.Context(), &runtimev1.InvokeRequest{Identity: identity(), Context: expired, PayloadJson: []byte(`{}`)}); err == nil {
		t.Fatal("expired deadline was accepted")
	}
	wrong := identity()
	wrong.ProtocolVersion = "2.0"
	if _, err := (*client).Invoke(t.Context(), &runtimev1.InvokeRequest{Identity: wrong, Context: governedContextProto("weather.current"), PayloadJson: []byte(`{}`)}); err == nil {
		t.Fatal("wrong protocol version was accepted")
	}
}
