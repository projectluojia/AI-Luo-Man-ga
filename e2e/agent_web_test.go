//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/access/web"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/adapters/packagesource"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/executor"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/health"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/packstore"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/session"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/blob"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/memory"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campus"
	"github.com/projectluojia/AI-Luo-Man-ga/testsupport/campus/campustest"
)

// syncBuffer 保护 os/exec 输出协程与测试断言之间的并发读写。
type syncBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

type integrationWebAuthenticator struct{}

func (integrationWebAuthenticator) Authenticate(*http.Request) (web.AuthenticatedWebIdentity, error) {
	return web.AuthenticatedWebIdentity{
		PlatformSpaceID: "web", PlatformUserID: "integration-user", PlatformSessionID: "integration-session",
	}, nil
}

func TestGoPythonModelToolDatabaseLoop(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("Executor 包的受限模型密钥文件校验仅支持 Unix")
	}
	var modelTurns atomic.Int32
	modelHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"data":[{"id":"test-model","object":"model","created":1,"owned_by":"test"}]}`)
			return
		}
		if request.URL.Path == "/v1/models/test-model" {
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"id":"test-model","object":"model","created":1,"owned_by":"test"}`)
			return
		}
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		body, _ := io.ReadAll(request.Body)
		bodyText := string(body)
		writer.Header().Set("Content-Type", "text/event-stream")
		// writeTurn 输出一轮 chat.completion 的 delta/结束/用量三段 SSE。
		writeTurn := func(id, delta, finish, usage string) {
			fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":`+delta+`,"finish_reason":null}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`+"\n\n")
			fmt.Fprint(writer, `data: {"id":"`+id+`","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":`+usage+`}`+"\n\n")
		}
		// Agent Executor 只请求一次普通 Capability；后续模型轮次消费结果。
		modelTurns.Add(1)
		hasToolResult := strings.Contains(bodyText, `"role":"tool"`)
		switch {
		case !hasToolResult:
			if !strings.Contains(bodyText, "cap_campus_bus_routes_list") || strings.Contains(bodyText, "create_child") {
				t.Errorf("model request missing projected capability: %s", body)
			}
			writeTurn("one", `{"role":"assistant","tool_calls":[{"index":0,"id":"route-call","type":"function","function":{"name":"cap_campus_bus_routes_list","arguments":"{\"limit\":10}"}}]}`, "tool_calls", `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`)
		case hasToolResult:
			if !strings.Contains(bodyText, "cap_campus_bus_routes_list") || !strings.Contains(bodyText, `"role":"tool"`) {
				t.Errorf("model follow-up request is invalid: %s", body)
			}
			writeTurn("two", `{"role":"assistant","content":"当前有一条测试线路。"}`, "stop", `{"prompt_tokens":20,"completion_tokens":3,"total_tokens":23}`)
		default:
			t.Errorf("unexpected model request: %s", body)
			http.Error(writer, "unexpected model request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(writer, "data: [DONE]\n\n")
	})
	modelListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	modelServer := httptest.NewUnstartedServer(modelHandler)
	modelServer.Listener = modelListener
	modelServer.Start()
	defer modelServer.Close()

	executorPackageRoot := installedExecutorPackage(t)
	executorInstallRoot := filepath.Dir(executorPackageRoot)
	installed, err := packageio.ReadInstalled(t.Context(), executorPackageRoot)
	if err != nil {
		t.Fatalf("读取已安装 Executor 包失败: %v", err)
	}
	projectRoot := writeExecutorProject(t, installed)
	catalog, err := packagesource.NewCatalog(executorInstallRoot)
	if err != nil {
		t.Fatalf("创建安装包目录: %v", err)
	}
	projectLock, err := packagesource.ReadProjectLock(t.Context(), projectRoot)
	if err != nil {
		t.Fatalf("读取项目锁: %v", err)
	}
	records, err := catalog.DiscoverLocked(t.Context(), projectLock)
	if err != nil {
		t.Fatalf("发现已安装 Executor 包: %v", err)
	}
	var executorRecord loader.InstalledRecord
	for _, record := range records {
		if record.Runtime.Role != loader.RoleExecutor {
			continue
		}
		if executorRecord.Runtime.ID != "" {
			t.Fatal("项目锁包含多个 Executor 组件")
		}
		executorRecord = record
	}
	if executorRecord.Runtime.ID == "" {
		t.Fatal("项目锁未包含 Executor 组件")
	}
	executorSpec, err := catalog.ResolveProcess(t.Context(), executorRecord.Runtime)
	if err != nil {
		t.Fatalf("解析已安装 Executor 进程规格: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	deploymentSpec := executorSpec
	// 这些变量属于 Agent 自己的 Deployment；Core 的 ProcessHost 只连接
	// 安装 lock 中的空环境，不把模型配置注入包进程。
	secretPath := filepath.Join(t.TempDir(), "model-key")
	if err := os.WriteFile(secretPath, []byte("test-key\n"), 0o600); err != nil {
		t.Fatalf("写入测试模型密钥: %v", err)
	}
	deploymentSpec.Env = append(os.Environ(),
		"PYTHONDONTWRITEBYTECODE=1",
		"AILUO_MODEL_NAME=test-model",
		"AILUO_MODEL_API_KEY_FILE="+secretPath,
		"AILUO_MODEL_BASE_URL="+modelServer.URL+"/v1",
	)

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 关闭顺序（defer 逆序）：取消调度 → 租约归还 → 关闭执行者 → 取消上下文 → 关闭存储。
	// 调度必须先于关库，避免活动 Run 或轮询打到已关闭的存储。
	defer store.Close()
	defer cancel()
	logs := &syncBuffer{}
	// Executor 由自己的 Deployment 按安装 lock 启动；Core 只负责连接和治理。
	executorProcess, err := loader.StartProcess(ctx, deploymentSpec, logs, logs)
	if err != nil {
		t.Fatalf("启动已安装 Executor 包: %v", err)
	}
	defer func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cleanupCancel()
		if err := executorProcess.Reap(cleanupContext, 3*time.Second, 2*time.Second); err != nil {
			t.Errorf("回收 Executor 包进程: %v", err)
		}
	}()

	executorHost, err := loader.NewProcessHost(loader.ProcessHostConfig{
		Resolve: catalog.ResolveProcess, Verify: catalog.VerifyProcess,
		Spawn:          false,
		DialTimeout:    10 * time.Second,
		StopGrace:      3 * time.Second,
		TerminateGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewProcessHost: %v", err)
	}
	executorManager, err := loader.New(executorHost)
	if err != nil {
		t.Fatalf("new executor manager: %v", err)
	}
	reg := registry.New()
	if err := loader.RegisterInstalled(ctx, executorManager, reg, records); err != nil {
		t.Fatalf("register installed runtimes: %v", err)
	}
	if err := executorManager.Warmup(ctx, executorManager.Pinned(), 1); err != nil {
		t.Fatalf("warm executor: %v\n%s", err, logs.String())
	}
	executorLease, err := executorManager.Executor(ctx, executorRecord.Runtime.ID)
	if err != nil {
		t.Fatalf("resolve executor: %v", err)
	}
	executorRuntime := executorLease.Runtime()
	clientProvider, ok := executorRuntime.(executor.ClientProvider)
	if !ok {
		t.Fatal("executor runtime does not expose an executor client")
	}
	executorClient := clientProvider.Client()
	// 关闭顺序（defer 逆序）：Shutdown 需等待租约排空，故租约归还最后注册、最先执行。
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := executorManager.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown executor manager: %v", err)
		}
	}()
	defer executorLease.Release()

	docs := memory.NewDocuments()
	scope := packstore.Scope{AppID: campus.AppID, PackageID: campus.PackageID, Namespace: campus.StorageNamespace}
	routes := []packstore.Document{{ID: "r", Payload: []byte(`{"id":"r","name":"测试线路","direction":"去程","source_revision":"e2e-revision"}`)}}
	if err := docs.ReplaceSnapshot(context.Background(), scope, packstore.SnapshotMeta{
		Revision: "e2e-revision", Source: "zhihui-luojia", Authoritative: true, Complete: true,
		ImportedAt: time.Now().UTC().Add(-time.Hour), ValidUntil: time.Now().UTC().Add(time.Hour),
	}, map[string][]packstore.Document{"routes": routes}); err != nil {
		t.Fatal(err)
	}
	policy := runtimetest.NewStaticAppPolicy()
	policy.Enable(campus.AppID, campus.BusRouteListCapabilityID)
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{IdempotencyStore: store})
	campustest.RegisterHosted(t, reg, docs)
	// 上下文装配使用真实会话来源（SQLite 消息存储 + 安全 Blob 存储）。
	blobStore, err := blob.Open(filepath.Join(t.TempDir(), "blobs"), session.MaxMessageContentBytes)
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	defer blobStore.Close()
	sessionService, err := session.NewService(store, blobStore)
	if err != nil {
		t.Fatalf("new session service: %v", err)
	}
	if _, _, err := store.Ensure(ctx, appconfig.Config{
		AppID: campus.AppID, Enabled: true, ExecutorID: executorRecord.Runtime.ID,
		ExecutorConfig: []byte(`{"system_prompt":"test"}`), MaxSteps: 4, MaxCapabilityCalls: 8,
		MaxExecutionUnits: 1500, MaxOutputBytes: 4096,
		ExecutionTimeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("seed app config: %v", err)
	}
	orchestrator := kernelecho.NewOrchestrator(executorClient, reg, dispatcher, policy, kernelecho.StorePorts{
		Idempotency: store, Creation: store, Execution: store, Recovery: store,
		Cancellation: store, Events: store, Audit: store,
	}, kernelecho.Config{
		AppID:           campus.AppID,
		AppConfigSource: store,
		Context:         sessionService,
	})
	echoEvents := access.NewEventHub()
	runScheduler := kernelecho.NewScheduler(ctx, orchestrator, store, echoEvents, campus.AppID)
	if _, err := runScheduler.Recover(ctx); err != nil {
		t.Fatalf("recover durable runs: %v", err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := runScheduler.Shutdown(shutdownContext); err != nil {
			t.Errorf("shutdown run scheduler: %v", err)
		}
	}()
	identities := identity.NewService(store)
	if _, err := identities.CreateUser(ctx, "integration-user"); err != nil {
		t.Fatal(err)
	}
	if err := identities.BindExternalIdentity(ctx, identity.ExternalIdentity{
		AppID: campus.AppID, Platform: "web", PlatformSpaceID: "web",
		PlatformUserID: "integration-user", UserID: "integration-user",
	}); err != nil {
		t.Fatal(err)
	}
	if err := identities.SetMembership(ctx, identity.AppMembership{AppID: campus.AppID, UserID: "integration-user"}); err != nil {
		t.Fatal(err)
	}
	platformHub, err := access.NewHub(campus.AppID, store, identities)
	if err != nil {
		t.Fatal(err)
	}
	echoAdmission := kernelecho.NewAdmission(orchestrator, runScheduler)
	handler := web.NewServer(
		echoAdmission, store,
		health.Combined{store, health.ExecutorChecker{Client: executorClient}},
		reg, policy, campus.AppID, platformHub, runScheduler, echoEvents,
		web.WithWebAuthenticator(integrationWebAuthenticator{}),
	).Handler()
	readinessRecorder := httptest.NewRecorder()
	handler.ServeHTTP(readinessRecorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if readinessRecorder.Code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s logs=%s", readinessRecorder.Code, readinessRecorder.Body.String(), logs.String())
	}
	createRecorder := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v2/echoes", bytes.NewBufferString(`{"message":"有哪些校巴线路？"}`))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Idempotency-Key", "integration-create-echo")
	handler.ServeHTTP(createRecorder, createRequest)
	if createRecorder.Code != http.StatusAccepted {
		t.Fatalf("create echo status=%d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	traceID := createRecorder.Header().Get("X-Trace-ID")
	var accepted map[string]string
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	echoID := accepted["echo_id"]
	eventsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(eventsRecorder, httptest.NewRequest(http.MethodGet, accepted["events_url"], nil))
	if eventsRecorder.Code != http.StatusOK || !strings.Contains(eventsRecorder.Body.String(), "event: run.completed") {
		t.Fatalf("events status=%d body=%s logs=%s", eventsRecorder.Code, eventsRecorder.Body.String(), logs.String())
	}
	record, events, err := store.GetEcho(ctx, campus.AppID, echoID)
	if err != nil {
		t.Fatal(err)
	}
	if string(record.Result.Data) != "当前有一条测试线路。" || modelTurns.Load() != 2 {
		encoded, _ := json.Marshal(events)
		t.Fatalf("record=%#v model_turns=%d events=%s logs=%s", record, modelTurns.Load(), encoded, logs.String())
	}
	runs, err := store.ListRuns(ctx, campus.AppID, echoID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v logs=%s", runs, err, logs.String())
	}
	rootRun := runs[0]
	messages, err := store.ListMessages(ctx, campus.AppID, rootRun.SessionID, session.MessageQuery{Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].SenderUserID != "integration-user" || messages[0].Type != "text" {
		t.Fatalf("会话消息未持久化或形状错误: messages=%#v err=%v", messages, err)
	}
	if rootRun.ID == "" || rootRun.LastExecutorSequence != 6 || rootRun.UsedExecutionUnits != 35 ||
		rootRun.UsedRetries != 0 {
		t.Fatalf("run usage/state=%#v logs=%s", rootRun, logs.String())
	}
	audits, err := store.ListCapabilityCalls(ctx, campus.AppID, echoID)
	if err != nil || len(audits) != 1 {
		t.Fatalf("audits=%#v err=%v logs=%s", audits, err, logs.String())
	}
	if traceID == "" || !strings.Contains(logs.String(), `trace_id="`+traceID+`"`) || !strings.Contains(logs.String(), "parent_span_id=") {
		t.Fatalf("Go→Python trace context missing: trace_id=%q logs=%s", traceID, logs.String())
	}
}

// installedExecutorPackage 返回已经经过 Package Manager 安装的 Executor 包。
// CI 可通过环境变量提供已安装目录；本地未提供时走同一 CLI 先打包再安装，
// 不允许测试直接把源码目录当成运行时。
func installedExecutorPackage(t *testing.T) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("AILUO_EXECUTOR_PACKAGE_DIR")); configured != "" {
		path, err := filepath.Abs(configured)
		if err != nil {
			t.Fatalf("解析 Executor 包目录: %v", err)
		}
		return path
	}
	repositoryRoot := findRepositoryRoot(t)
	source := filepath.Join(repositoryRoot, "packages", "agent")
	distribution := t.TempDir()
	installRoot := t.TempDir()
	runPackageManagerCommand(t, repositoryRoot, "pack", source, distribution)
	entries, err := os.ReadDir(distribution)
	if err != nil {
		t.Fatalf("读取 Executor 发布物: %v", err)
	}
	var archive string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tgz") {
			if archive != "" {
				t.Fatal("Executor 发布目录包含多个 tarball")
			}
			archive = filepath.Join(distribution, entry.Name())
		}
	}
	if archive == "" {
		t.Fatal("Executor 发布目录缺少 tarball")
	}
	runPackageManagerCommand(t, repositoryRoot, "install", "--root", installRoot, archive)
	return filepath.Join(installRoot, "agent")
}

// runPackageManagerCommand 通过独立的 Package Manager CLI 执行测试准备命令。
func runPackageManagerCommand(t *testing.T, repositoryRoot string, arguments ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", append([]string{"run", "./cmd/ailuo-pm"}, arguments...)...)
	command.Dir = filepath.Join(repositoryRoot, "package-manager")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("ailuo-pm %v 失败: %v\n%s", arguments, err, output)
	}
	return output
}

// writeExecutorProject 写入只包含已安装 Executor 的项目清单和锁，供 Core
// 以真实 project lock 发现并校验安装目录。
func writeExecutorProject(t *testing.T, installed packageio.InstalledRecord) string {
	t.Helper()
	projectRoot := t.TempDir()
	source := "github:e2e/" + installed.Manifest.ID
	manifest := projectcontract.Manifest{
		SchemaVersion: projectcontract.SchemaVersion, ID: "executor-e2e",
		Dependencies: []projectcontract.Dependency{{
			ID: installed.Manifest.ID, Constraint: installed.Manifest.Version, Source: source,
		}},
	}
	manifestText := "[project]\nid = \"executor-e2e\"\n\n[dependencies.\"" + installed.Manifest.ID + "\"]\nversion = \"" + installed.Manifest.Version + "\"\nregistry = \"" + source + "\"\n"
	manifestPath := filepath.Join(projectRoot, "ailuo.toml")
	if err := os.WriteFile(manifestPath, []byte(manifestText), 0o640); err != nil {
		t.Fatal(err)
	}
	manifestDigest, err := packageio.HashFile(t.Context(), manifestPath, packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lockDigest, err := packageio.CanonicalLockDigest(t.Context(), installed.Directory, installed.Lock)
	if err != nil {
		t.Fatal(err)
	}
	packageManifestDigest, err := packageio.HashFile(t.Context(), filepath.Join(installed.Directory, "manifest.json"), packagecontract.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lock := projectcontract.Lock{
		SchemaVersion: projectcontract.SchemaVersion, ProjectID: manifest.ID,
		ProjectManifestSHA256: manifestDigest,
		Packages: []projectcontract.LockedPackage{{
			ID: installed.Manifest.ID, Version: installed.Manifest.Version, Source: source,
			ManifestSHA256: packageManifestDigest, LockSHA256: lockDigest,
		}},
	}
	if err := projectcontract.ValidateLock(lock, manifest); err != nil {
		t.Fatalf("构造项目锁: %v", err)
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "ailuo.lock"), lockBytes, 0o640); err != nil {
		t.Fatal(err)
	}
	return projectRoot
}

// findRepositoryRoot 从测试工作目录向上寻找仓库 workspace 文件。
func findRepositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.work")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("repository root not found")
		}
		directory = parent
	}
}
