package web_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
)

type fakeOrchestrator struct {
	store         *sqlite.Store
	block         bool
	observed      chan observedContext
	createErr     error
	recovery      []kernelecho.RunWork
	createEntered chan struct{}
	releaseCreate chan struct{}
	runGate       chan struct{}
	activeRuns    atomic.Int32
	maxActiveRuns atomic.Int32
}

func outputEvent(text string) []byte {
	encoded, _ := json.Marshal(kernelecho.Output{ContentType: "text/plain", Data: []byte(text)})
	return encoded
}

type testWebResolver struct{}

func (testWebResolver) ResolveIdentity(_ context.Context, appID, _, _, _ string) (identity.IdentityContext, error) {
	return identity.IdentityContext{
		AppID: appID, UserID: "web-test-user",
		Membership: &identity.AppMembership{AppID: appID, UserID: "web-test-user"},
	}, nil
}

type testWebAuthenticator struct{}

func (testWebAuthenticator) Authenticate(*http.Request) (web.AuthenticatedWebIdentity, error) {
	return web.AuthenticatedWebIdentity{
		PlatformSpaceID: "web", PlatformUserID: "web-test-subject", PlatformSessionID: "web-test-session",
	}, nil
}

type testController struct{}

func (testController) Enqueue(context.Context, string) {}

func (testController) Cancel(context.Context, string) (bool, error) { return false, nil }

func newEchoAdmission(creator kernelecho.Creator, enqueuer kernelecho.Enqueuer) kernelecho.Admission {
	return kernelecho.NewAdmission(creator, enqueuer)
}

type testOrchestrator interface {
	kernelecho.SchedulerRunner
	kernelecho.Creator
}

type testWebServer struct {
	*web.Server
	scheduler *kernelecho.Scheduler
}

// newTestHub 为 Web 测试构造需要受治理身份的接入入口。
func newTestHub(store *sqlite.Store, appID string) *access.Hub {
	hub, err := access.NewHub(appID, store, testWebResolver{})
	if err != nil {
		panic(err)
	}
	return hub
}

func newAuthenticatedServer(
	t *testing.T,
	ctx context.Context,
	orchestrator testOrchestrator,
	reader web.EchoReader,
	health web.HealthChecker,
	reg *registry.Registry,
	policy runtime.AppPolicy,
	appID string,
	platformHub *access.Hub,
) *testWebServer {
	events := access.NewEventHub()
	scheduler := kernelecho.NewScheduler(ctx, orchestrator, reader, events, appID)
	if _, err := scheduler.Recover(ctx); err != nil {
		t.Fatalf("recover scheduler: %v", err)
	}
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := scheduler.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown scheduler: %v", err)
		}
	})
	return &testWebServer{
		Server: web.NewServer(newEchoAdmission(orchestrator, scheduler), reader, health, reg, policy, appID, platformHub, scheduler, events,
			web.WithWebAuthenticator(testWebAuthenticator{})),
		scheduler: scheduler,
	}
}

type observedContext struct {
	requestID string
	traceID   string
}

func (f *fakeOrchestrator) CreateIdempotent(ctx context.Context, request kernelecho.RunRequest) (string, bool, error) {
	if f.createEntered != nil {
		close(f.createEntered)
		select {
		case <-f.releaseCreate:
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}
	if f.createErr != nil {
		return "", false, f.createErr
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	return f.store.CreateEchoRunIdempotentLimited(ctx, request.IdempotencyKey, idempotency.Fingerprint([]byte(request.Message)), kernelecho.Record{
		ID: id, AppID: "campus-services", InputMessage: request.Message,
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, kernelecho.RunRecord{
		ID: "run-" + id, RunGroupID: "run-" + id, AppID: "campus-services", EchoID: id, Attempt: 1,
		SessionID: request.SessionID, UserID: request.UserID, MessageID: request.MessageID, Channel: request.Channel,
		Status: kernelecho.RunStatusQueued, ExecutorID: "executor.test", ExecutorConfig: []byte(`{"strategy":"test"}`), ConfigRevision: "test-config",
		InputPayload: []byte(request.Message), InputContentType: "text/plain; charset=utf-8",
		ProtocolVersion: "1.0", MaxSteps: 4, MaxCapabilityCalls: 4,
		MaxExecutionUnits: 2000,
		MaxOutputBytes:    4096, ExecutionTimeoutMS: 5000, Deadline: now.Add(time.Minute), AvailableAt: now,
		RecoverableState: []byte(`{}`), CreatedAt: now,
	}, 0)
}

func (f *fakeOrchestrator) Recoverable(context.Context) ([]kernelecho.RunWork, error) {
	return append([]kernelecho.RunWork(nil), f.recovery...), nil
}

func (f *fakeOrchestrator) Runnable(ctx context.Context, limit int) ([]kernelecho.RunWork, error) {
	if f.store == nil {
		return nil, nil
	}
	return f.store.ListRunnableRuns(ctx, "campus-services", time.Now().UTC(), limit)
}

func (f *fakeOrchestrator) Cancel(ctx context.Context, echoID string) (bool, error) {
	if f.store == nil {
		return false, nil
	}
	return f.store.CancelQueuedRun(ctx, "campus-services", echoID, time.Now().UTC())
}

func (f *fakeOrchestrator) CancelQueuedRuns(ctx context.Context) error {
	if f.store == nil {
		return nil
	}
	return f.store.CancelQueuedRuns(ctx, "campus-services", time.Now().UTC())
}

type failingReader struct {
	err error
}

func (f failingReader) GetEcho(context.Context, string, string) (kernelecho.Record, []kernelecho.Event, error) {
	return kernelecho.Record{}, nil, f.err
}

func (f failingReader) ListRuns(context.Context, string, string) ([]kernelecho.RunRecord, error) {
	return nil, f.err
}

func (f failingReader) GetRun(context.Context, string, string) (kernelecho.RunRecord, error) {
	return kernelecho.RunRecord{}, f.err
}

type staticHealth struct {
	err error
}

func (h staticHealth) Ping(context.Context) error {
	return h.err
}

type webPolicyFunc func(context.Context, string) (appconfig.PolicySnapshot, error)

func (f webPolicyFunc) Snapshot(ctx context.Context, appID string) (appconfig.PolicySnapshot, error) {
	return f(ctx, appID)
}

func (f *fakeOrchestrator) run(ctx context.Context, echoID string, emit kernelecho.EventEmitter) error {
	if f.observed != nil {
		f.observed <- observedContext{
			requestID: observe.String(ctx, "request_id"),
			traceID:   observe.String(ctx, "trace_id"),
		}
	}
	now := time.Now().UTC()
	runs, err := f.store.ListRuns(ctx, "campus-services", echoID)
	if err != nil || len(runs) != 1 {
		return errors.New("expected one persisted Run")
	}
	run, err := f.store.ClaimRun(ctx, "campus-services", echoID, runs[0].ID, "lease-"+echoID, now, now.Add(time.Minute))
	if err != nil {
		return err
	}
	active := f.activeRuns.Add(1)
	defer f.activeRuns.Add(-1)
	for {
		maximum := f.maxActiveRuns.Load()
		if active <= maximum || f.maxActiveRuns.CompareAndSwap(maximum, active) {
			break
		}
	}
	if f.runGate != nil {
		select {
		case <-f.runGate:
		case <-ctx.Done():
			return f.store.CompleteRun(context.Background(), run, kernelecho.RunStatusCancelled, kernelecho.StatusCancelled, kernelecho.Output{}, publicerror.Echo("cancelled"), time.Now().UTC())
		}
	}
	if f.block {
		<-ctx.Done()
		return f.store.CompleteRun(context.Background(), run, kernelecho.RunStatusCancelled, kernelecho.StatusCancelled, kernelecho.Output{}, publicerror.Echo("cancelled"), time.Now().UTC())
	}
	events := []kernelecho.Event{
		{AppID: "campus-services", EchoID: echoID, RunID: run.ID, Type: "output.delta", Payload: outputEvent("你好"), CreatedAt: time.Now().UTC()},
		{AppID: "campus-services", EchoID: echoID, RunID: run.ID, Type: "run.completed", Payload: outputEvent("你好"), CreatedAt: time.Now().UTC()},
	}
	for _, event := range events {
		stored, err := f.store.AppendEchoEvent(ctx, event)
		if err != nil {
			return err
		}
		if err := emit(stored); err != nil {
			return err
		}
	}
	return f.store.CompleteRun(ctx, run, kernelecho.RunStatusSucceeded, kernelecho.StatusSucceeded, kernelecho.Output{ContentType: "text/plain", Data: []byte("你好")}, publicerror.Error{}, time.Now().UTC())
}

func (f *fakeOrchestrator) RunQueued(ctx context.Context, work kernelecho.RunWork, emit kernelecho.EventEmitter) error {
	return f.run(ctx, work.Run.EchoID, emit)
}

func TestWebAccessEchoSSEAndStatus(t *testing.T) {
	handler, store := newTestServer(t, false)
	echoID, eventsURL := createEcho(t, handler, "你好")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, eventsURL, nil))
	body := response.Body.Bytes()
	if response.Code != http.StatusOK || !bytes.Contains(body, []byte("event: output.delta")) || !bytes.Contains(body, []byte("event: run.completed")) {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
	record, events, err := store.GetEcho(context.Background(), "campus-services", echoID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != kernelecho.StatusSucceeded || len(events) != 2 {
		t.Fatalf("record=%#v events=%#v", record, events)
	}

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/v1/echoes/"+echoID, nil))
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"runs"`) {
		t.Fatalf("status endpoint returned %d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
}

func TestWebAccessCancelsRunningEcho(t *testing.T) {
	handler, store := newTestServer(t, true)
	echoID, _ := createEcho(t, handler, "cancel me")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/echoes/"+echoID, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel status=%d", response.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, _, err := store.GetEcho(context.Background(), "campus-services", echoID)
		if err == nil && record.Status == kernelecho.StatusCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("echo did not become cancelled: record=%#v err=%v", record, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWebAccessRejectsUnknownFields(t *testing.T) {
	handler, _ := newTestServer(t, false)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", strings.NewReader(`{"message":"ok","user_id":"not-allowed"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "unknown-fields")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestWebAccessRejectsUnauthenticatedEchoBeforePersistence(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "unauthenticated.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	server := web.NewServer(
		newEchoAdmission(&fakeOrchestrator{store: store}, testController{}), store, store,
		registry.New(), runtimetest.NewStaticAppPolicy(), "campus-services", newTestHub(store, "campus-services"), testController{}, access.NewEventHub(),
	)
	response := createEchoRequest(t, server.Handler(), "不应写入", "unauthenticated")
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"authentication_required"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWebAccessRequiresAndReplaysEchoCreationIdempotency(t *testing.T) {
	handler, store := newTestServer(t, false)

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/v2/echoes", strings.NewReader(`{"message":"test"}`)))
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), `"code":"invalid_idempotency_key"`) {
		t.Fatalf("missing key status=%d body=%s", missing.Code, missing.Body.String())
	}

	first := createEchoRequest(t, handler, "same request", "same-key")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	second := createEchoRequest(t, handler, "same request", "same-key")
	if second.Code != http.StatusOK || second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d header=%q body=%s", second.Code, second.Header().Get("Idempotency-Replayed"), second.Body.String())
	}
	var firstBody map[string]string
	var secondBody map[string]string
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if firstBody["echo_id"] == "" || secondBody["echo_id"] != firstBody["echo_id"] {
		t.Fatalf("idempotent responses differ: first=%#v second=%#v", firstBody, secondBody)
	}
	runs, err := store.ListRuns(context.Background(), "campus-services", firstBody["echo_id"])
	if err != nil || len(runs) != 1 {
		t.Fatalf("duplicate creation produced runs=%#v err=%v", runs, err)
	}

	conflict := createEchoRequest(t, handler, "different request", "same-key")
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"code":"idempotency_conflict"`) {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestWebAccessCopiesRequestContextIntoBackgroundRun(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	observed := make(chan observedContext, 1)
	backend := &fakeOrchestrator{store: store, observed: observed}
	handler := newAuthenticatedServer(t, context.Background(), backend, store, store, reg, policy, "campus-services", newTestHub(store, "campus-services")).Handler()
	payload := strings.NewReader(`{"message":"查询校巴"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", payload)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "background-run")
	request.Header.Set("X-Request-ID", "request-background")
	request.Header.Set("X-Trace-ID", "1234567890abcdef1234567890abcdef")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("创建 Echo 状态码=%d 响应=%s", response.Code, response.Body.String())
	}
	select {
	case fields := <-observed:
		if fields.requestID != "request-background" || fields.traceID != "1234567890abcdef1234567890abcdef" {
			t.Fatalf("后台 Run 关联标识错误：%+v", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("等待后台 Run 上下文超时")
	}
}

func TestWebAccessRecoversPersistedQueuedRun(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "recover.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	backend := &fakeOrchestrator{store: store}
	echoID, _, err := backend.CreateIdempotent(context.Background(), kernelecho.RunRequest{Message: "recover", IdempotencyKey: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(context.Background(), "campus-services", echoID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	backend.recovery = []kernelecho.RunWork{{Run: runs[0]}}
	_ = newAuthenticatedServer(t, context.Background(), backend, store, store, reg, policy, "campus-services", newTestHub(store, "campus-services"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		record, _, err := store.GetEcho(context.Background(), "campus-services", echoID)
		if err == nil && record.Status == kernelecho.StatusSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued Run was not recovered: record=%#v err=%v", record, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWebAccessDoesNotExposeCrossAppEcho(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	now := time.Now().UTC()
	if _, created, err := store.CreateEchoRunIdempotentLimited(context.Background(), "other-app-echo", idempotency.Fingerprint([]byte("secret")), kernelecho.Record{
		ID: "other-app-echo", AppID: "app-b", InputMessage: "secret",
		Status: kernelecho.StatusRunning, CreatedAt: now,
	}, kernelecho.RunRecord{
		ID: "other-app-run", RunGroupID: "other-app-run", AppID: "app-b", EchoID: "other-app-echo", Attempt: 1,
		Status: kernelecho.RunStatusQueued, ExecutorID: "executor.test", ExecutorConfig: []byte(`{"strategy":"test"}`), ConfigRevision: "test-config",
		InputPayload: []byte("secret"), InputContentType: "text/plain; charset=utf-8",
		ProtocolVersion: "1.0", MaxSteps: 4, MaxCapabilityCalls: 4,
		MaxExecutionUnits: 2000,
		MaxOutputBytes:    4096, ExecutionTimeoutMS: 5000, Deadline: now.Add(time.Minute), AvailableAt: now,
		RecoverableState: []byte(`{}`), CreatedAt: now,
	}, 0); err != nil || !created {
		t.Fatal(err)
	}
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	backend := &fakeOrchestrator{store: store}
	handler := newAuthenticatedServer(t, context.Background(), backend, store, store, reg, policy, "app-a", newTestHub(store, "app-a")).Handler()
	for _, methodAndPath := range [][2]string{
		{http.MethodGet, "/api/v1/echoes/other-app-echo"},
		{http.MethodDelete, "/api/v1/echoes/other-app-echo"},
		{http.MethodGet, "/api/v1/echoes/other-app-echo/events"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(methodAndPath[0], methodAndPath[1], nil))
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"echo_not_found"`) {
			t.Fatalf("%s %s status=%d body=%s", methodAndPath[0], methodAndPath[1], response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "app-b") {
			t.Fatalf("cross-app response disclosed data: %s", response.Body.String())
		}
	}
}

func TestWebAccessPublicErrorsDoNotDiscloseInternalDetails(t *testing.T) {
	secret := "SQL /srv/private.db api-key-secret"
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()

	createHandler := newAuthenticatedServer(
		t,
		context.Background(),
		&fakeOrchestrator{store: store, createErr: errors.New(secret)},
		store,
		staticHealth{},
		reg,
		policy,
		"campus-services",
		newTestHub(store, "campus-services"),
	).Handler()
	createResponse := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", strings.NewReader(`{"message":"test"}`))
	createRequest.Header.Set("Idempotency-Key", "public-error")
	createHandler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusInternalServerError || strings.Contains(createResponse.Body.String(), secret) {
		t.Fatalf("create response status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}

	readHandler := newAuthenticatedServer(
		t,
		context.Background(),
		&fakeOrchestrator{store: store},
		failingReader{err: errors.New(secret)},
		staticHealth{},
		reg,
		policy,
		"campus-services",
		newTestHub(store, "campus-services"),
	).Handler()
	readResponse := httptest.NewRecorder()
	readHandler.ServeHTTP(readResponse, httptest.NewRequest(http.MethodGet, "/api/v1/echoes/echo", nil))
	if readResponse.Code != http.StatusInternalServerError || strings.Contains(readResponse.Body.String(), secret) {
		t.Fatalf("read response status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}

	healthHandler := newAuthenticatedServer(
		t,
		context.Background(),
		&fakeOrchestrator{store: store},
		store,
		staticHealth{err: errors.New(secret)},
		reg,
		policy,
		"campus-services",
		newTestHub(store, "campus-services"),
	).Handler()
	healthResponse := httptest.NewRecorder()
	healthHandler.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if healthResponse.Code != http.StatusServiceUnavailable || strings.Contains(healthResponse.Body.String(), secret) {
		t.Fatalf("health response status=%d body=%s", healthResponse.Code, healthResponse.Body.String())
	}
}

func TestCapabilitiesFailClosedForUnavailableOrDisabledAppPolicy(t *testing.T) {
	secret := "SQL /srv/private.db api-key-secret"
	tests := []struct {
		name, code string
		policy     runtime.AppPolicy
	}{
		{
			name: "unavailable", code: "app_policy_unavailable",
			policy: webPolicyFunc(func(context.Context, string) (appconfig.PolicySnapshot, error) {
				return appconfig.PolicySnapshot{}, errors.New(secret)
			}),
		},
		{
			name: "identity_mismatch", code: "app_policy_unavailable",
			policy: webPolicyFunc(func(context.Context, string) (appconfig.PolicySnapshot, error) {
				return appconfig.PolicySnapshot{AppID: "other-app", Revision: "test", Generation: 1, Enabled: true}, nil
			}),
		},
		{
			name: "disabled", code: "app_disabled",
			policy: webPolicyFunc(func(_ context.Context, appID string) (appconfig.PolicySnapshot, error) {
				return appconfig.PolicySnapshot{AppID: appID, Revision: "test", Generation: 1}, nil
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := test.policy
			expectedCode := test.code
			tempDir := t.TempDir()
			store, err := sqlite.Open(filepath.Join(tempDir, "capabilities.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
			handler := newAuthenticatedServer(
				t,
				t.Context(),
				&fakeOrchestrator{},
				failingReader{err: errors.New("unused")},
				staticHealth{},
				registry.New(),
				policy,
				"campus-services",
				newTestHub(store, "campus-services"),
			).Handler()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), secret) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), `"code":"`+expectedCode+`"`) {
				t.Fatalf("body=%s", response.Body.String())
			}
		})
	}
}

func TestEchoCreationMapsAppConfigurationFailuresToSafeErrors(t *testing.T) {
	secret := "SQL /srv/private.db api-key-secret"
	for name, createErr := range map[string]error{
		"app_disabled":           kernelecho.ErrAppDisabled,
		"app_config_unavailable": errors.Join(kernelecho.ErrAppConfigUnavailable, errors.New(secret)),
	} {
		t.Run(name, func(t *testing.T) {
			tempDir := t.TempDir()
			store, err := sqlite.Open(filepath.Join(tempDir, "app-config-error.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
			handler := newAuthenticatedServer(
				t,
				t.Context(),
				&fakeOrchestrator{store: store, createErr: createErr},
				store,
				staticHealth{},
				registry.New(),
				runtimetest.NewStaticAppPolicy(),
				"campus-services",
				newTestHub(store, "campus-services"),
			).Handler()
			response := createEchoRequest(t, handler, "test", "app-config-error")
			if response.Code != http.StatusServiceUnavailable ||
				!strings.Contains(response.Body.String(), `"code":"`+name+`"`) ||
				strings.Contains(response.Body.String(), secret) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

// TestEchoCreationPersistsStandardMessageToSessionStore 验证平台消息经统一入口
// 持久化到会话台账（SQLite），且同一幂等键的重复投递既不重复消息也不重复 Echo。
func TestEchoCreationPersistsStandardMessageToSessionStore(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "session-persist.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	backend := &fakeOrchestrator{store: store}
	handler := newAuthenticatedServer(t, context.Background(), backend, store, store, reg, policy, "campus-services", newTestHub(store, "campus-services")).Handler()
	first := createEchoRequest(t, handler, "有哪些校巴线路？", "persist-message")
	if first.Code != http.StatusAccepted {
		t.Fatalf("首次创建 status=%d body=%s", first.Code, first.Body.String())
	}
	var firstBody, secondBody map[string]string
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListRuns(context.Background(), "campus-services", firstBody["echo_id"])
	if err != nil || len(runs) != 1 || !strings.HasPrefix(runs[0].SessionID, "session-v1-") {
		t.Fatalf("run session=%#v err=%v", runs, err)
	}
	messages, err := store.ListMessages(context.Background(), "campus-services", runs[0].SessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "web-test-user" || messages[0].PlatformMessageID != "persist-message" {
		t.Fatalf("标准消息未持久化到会话台账: messages=%#v err=%v", messages, err)
	}
	replay := createEchoRequest(t, handler, "有哪些校巴线路？", "persist-message")
	if replay.Code != http.StatusOK {
		t.Fatalf("重放 status=%d body=%s", replay.Code, replay.Body.String())
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &secondBody); err != nil {
		t.Fatal(err)
	}
	if secondBody["echo_id"] != firstBody["echo_id"] {
		t.Fatalf("重放返回不同 Echo: first=%s second=%s", firstBody["echo_id"], secondBody["echo_id"])
	}
	messages, err = store.ListMessages(context.Background(), "campus-services", runs[0].SessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 {
		t.Fatalf("重复投递产生多条标准消息: messages=%#v err=%v", messages, err)
	}
}

func TestHealthzIsProcessLivenessAndReadyzChecksDependencies(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "healthz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	handler := newAuthenticatedServer(
		t,
		context.Background(),
		&fakeOrchestrator{},
		failingReader{err: errors.New("unused")},
		staticHealth{err: errors.New("dependency unavailable")},
		registry.New(),
		runtimetest.NewStaticAppPolicy(),
		"app",
		newTestHub(store, "app"),
	).Handler()

	liveness := httptest.NewRecorder()
	handler.ServeHTTP(liveness, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if liveness.Code != http.StatusOK || !strings.Contains(liveness.Body.String(), `"status":"live"`) {
		t.Fatalf("liveness status=%d body=%s", liveness.Code, liveness.Body.String())
	}
	readiness := httptest.NewRecorder()
	handler.ServeHTTP(readiness, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readiness.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status=%d body=%s", readiness.Code, readiness.Body.String())
	}
}

func TestWebAccessReturnsStableBackpressureResponse(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "queue-full.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	server := newAuthenticatedServer(
		t,
		context.Background(),
		&fakeOrchestrator{store: store, createErr: kernelecho.ErrQueueFull},
		store,
		store,
		registry.New(),
		runtimetest.NewStaticAppPolicy(),
		"campus-services",
		newTestHub(store, "campus-services"),
	)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewBufferString(`{"message":"full"}`))
	request.Header.Set("Idempotency-Key", "queue-full")
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" ||
		!strings.Contains(response.Body.String(), `"code":"queue_full"`) {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestMetricsEndpointUsesPrometheusFormatWithoutBusinessIdentifiers(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	handler := newAuthenticatedServer(
		t,
		context.Background(),
		&fakeOrchestrator{},
		failingReader{err: errors.New("unused")},
		staticHealth{},
		registry.New(),
		runtimetest.NewStaticAppPolicy(),
		"secret-app-id",
		newTestHub(store, "secret-app-id"),
	).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics status=%d headers=%v", response.Code, response.Header())
	}
	body := response.Body.String()
	if !strings.Contains(body, "ailuo_http_responses_total") || strings.Contains(body, "secret-app-id") {
		t.Fatalf("metrics body=%s", body)
	}
}

func TestWebAccessShutdownStopsAdmissionAndDrainsActiveRuns(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	backend := &fakeOrchestrator{store: store, block: true}
	server := newAuthenticatedServer(
		t,
		context.Background(),
		backend,
		store,
		store,
		registry.New(),
		runtimetest.NewStaticAppPolicy(),
		"campus-services",
		newTestHub(store, "campus-services"),
	)
	handler := server.Handler()
	echoID, _ := createEcho(t, handler, "shutdown")
	// 排空含持久化与运行取消，CI 并行负载下 1 秒墙钟预算不足，放宽到 10 秒（断言语义不变）。
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.StopAccepting()
	if err := server.scheduler.Shutdown(shutdownContext); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	record, _, err := store.GetEcho(context.Background(), "campus-services", echoID)
	if err != nil || record.Status != kernelecho.StatusCancelled {
		t.Fatalf("record=%#v err=%v", record, err)
	}
	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewBufferString(`{"message":"late"}`))
	request.Header.Set("Idempotency-Key", "late-after-shutdown")
	handler.ServeHTTP(create, request)
	if create.Code != http.StatusServiceUnavailable || !strings.Contains(create.Body.String(), `"code":"shutting_down"`) {
		t.Fatalf("late create status=%d body=%s", create.Code, create.Body.String())
	}
	readiness := httptest.NewRecorder()
	handler.ServeHTTP(readiness, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readiness.Code != http.StatusServiceUnavailable || !strings.Contains(readiness.Body.String(), `"code":"shutting_down"`) {
		t.Fatalf("shutdown readiness status=%d body=%s", readiness.Code, readiness.Body.String())
	}
}

func TestPersistentSchedulerBoundsConcurrentRuns(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "worker-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	gate := make(chan struct{})
	backend := &fakeOrchestrator{store: store, runGate: gate}
	server := newAuthenticatedServer(
		t,
		context.Background(), backend, store, store, registry.New(),
		runtimetest.NewStaticAppPolicy(), "campus-services", newTestHub(store, "campus-services"),
	)
	handler := server.Handler()
	echoIDs := make([]string, 0, 10)
	for index := 0; index < 10; index++ {
		echoID, _ := createEcho(t, handler, fmt.Sprintf("bounded-%d", index))
		echoIDs = append(echoIDs, echoID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for backend.activeRuns.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active, maximum := backend.activeRuns.Load(), backend.maxActiveRuns.Load(); active != 4 || maximum != 4 {
		t.Fatalf("active=%d max=%d, want fixed worker limit 4", active, maximum)
	}
	time.Sleep(30 * time.Millisecond)
	if maximum := backend.maxActiveRuns.Load(); maximum != 4 {
		t.Fatalf("scheduler exceeded worker limit: %d", maximum)
	}
	close(gate)
	deadline = time.Now().Add(10 * time.Second)
	completed := 0
	for time.Now().Before(deadline) {
		completed = 0
		for _, echoID := range echoIDs {
			record, _, readErr := store.GetEcho(context.Background(), "campus-services", echoID)
			if readErr == nil && record.Status == kernelecho.StatusSucceeded {
				completed++
			}
		}
		if completed == len(echoIDs) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed != len(echoIDs) {
		t.Fatalf("completed=%d, want %d", completed, len(echoIDs))
	}
	if maximum := backend.maxActiveRuns.Load(); maximum != 4 {
		t.Fatalf("scheduler exceeded worker limit after drain: %d", maximum)
	}
	// 排空含运行取消与持久化，CI 并行负载下 1 秒墙钟预算不足，放宽到 10 秒（断言语义不变）。
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.scheduler.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownWaitsForAdmittedCreationBeforeCancellingRun(t *testing.T) {
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "admission.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	backend := &fakeOrchestrator{
		store:         store,
		block:         true,
		createEntered: make(chan struct{}),
		releaseCreate: make(chan struct{}),
	}
	server := newAuthenticatedServer(
		t,
		context.Background(),
		backend,
		store,
		store,
		registry.New(),
		runtimetest.NewStaticAppPolicy(),
		"campus-services",
		newTestHub(store, "campus-services"),
	)
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewBufferString(`{"message":"admitted"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "admitted-before-shutdown")
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		responseDone <- response
	}()
	<-backend.createEntered
	server.StopAccepting()
	shutdownDone := make(chan error, 1)
	// 等待已接入创建事务排空含持久化，CI 并行负载下 1 秒墙钟预算不足，放宽到 10 秒（断言语义不变）。
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		shutdownDone <- server.WaitAdmissions(shutdownContext)
	}()
	select {
	case err := <-shutdownDone:
		t.Fatalf("关闭没有等待已接入创建事务：%v", err)
	default:
	}
	close(backend.releaseCreate)
	response := <-responseDone
	if response.Code != http.StatusAccepted {
		t.Fatalf("已接入请求状态=%d body=%s", response.Code, response.Body.String())
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := server.scheduler.Shutdown(shutdownContext); err != nil {
		t.Fatalf("scheduler shutdown: %v", err)
	}
	var accepted map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.GetEcho(context.Background(), "campus-services", accepted["echo_id"])
	if err != nil || record.Status != kernelecho.StatusCancelled {
		t.Fatalf("record=%#v err=%v", record, err)
	}
}

func newTestServer(t *testing.T, block bool) (http.Handler, *sqlite.Store) {
	t.Helper()
	tempDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tempDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, tempDir) })
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	backend := &fakeOrchestrator{store: store, block: block}
	return newAuthenticatedServer(t, context.Background(), backend, store, store, reg, policy, "campus-services", newTestHub(store, "campus-services")).Handler(), store
}

func createEcho(t *testing.T, handler http.Handler, message string) (string, string) {
	t.Helper()
	response := createEchoRequest(t, handler, message, uuid.NewString())
	if response.Code != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create status=%d body=%s", response.Code, body)
	}
	var result map[string]string
	if err := json.NewDecoder(bufio.NewReader(response.Body)).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result["echo_id"], result["events_url"]
}

func createEchoRequest(t *testing.T, handler http.Handler, message, key string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	handler.ServeHTTP(response, request)
	return response
}
