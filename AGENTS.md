# AI珞（爱珞） V3 Repository Instructions

## Production-Grade Mandate

AI珞（爱珞） V3 is a **production-grade project**. This is a mandatory engineering standard, not a future aspiration and not a label earned merely because a Demo runs.

The delivered feature scope may remain narrow, but every implemented contract, trust boundary, state transition, persistence rule, error path, and test must be designed for long-term production use. Do not justify disposable code with “MVP”, “Demo first”, “temporary”, or “we can rewrite it later”. A narrow production-grade slice is acceptable; a broad prototype foundation is not.

Be precise about status:

- The current repository contains a reliable campus-bus vertical slice and proves the main Go → Python Agent → Capability → Service → Tool → storage → SSE path.
- The repository is **not yet production-ready as a complete platform**. The 2026-07-26 audit P0 list is closed, but passing tests does not implement the P1 platform scope or replace deployment-specific acceptance.
- Never describe an unimplemented design item as completed. Distinguish clearly between design contract, implemented baseline, test evidence, and remaining production blocker.
- Never lower correctness, security, durability, isolation, or observability standards to accelerate a demonstration.

The user is the acceptance owner. Treat the repository as a serious long-lived system that may later handle institutional data and multiple Apps.

## Required Reading Order

Before architecture, protocol, persistence, Agent-runtime, or security changes, read:

1. `AGENTS.md` completely.
2. `docs/v3_overall_design.md` as the primary architecture contract.
3. `docs/校巴场景设计.md` as the first-scenario implementation baseline.
4. `docs/日志与可观测性设计.md` for logging, tracing fields, and disclosure rules.
5. `docs/数据需求与授权清单.md` before any real-data or ingestion work.

Then inspect the current implementation and tests. Do not infer implementation status from design documents alone.

## System Direction

AI珞（爱珞） V3 is a multi-entry AI assistant platform. The Go kernel is the sole system core and source of truth. Python Agent runtimes provide cognition but do not own authorization, system state, external access, scheduling, or business persistence.

The first product space is the `campus-services` App（校园综合服务 App）. Campus bus is its first Capability group, owned by the `campus` Service. The repository owns the Go backend and a minimal Web integration page only; it does not own the product frontend.

The ordinary Go kernel, Web Access, SQLite, and Python Agent development path must remain buildable and testable on Windows, Linux, and macOS. Unix ownership, Unix Socket, and process-group isolation capabilities remain explicit Unix-only security boundaries until an equally strong native implementation exists; unsupported platforms fail closed. Python environments and dependencies are managed only through `uv`, `agent/pyproject.toml`, and the committed `agent/uv.lock`.

## Non-Negotiable Architecture

- Go owns Access, Echo/Run orchestration, authorization, Registry, routing, loading, persistence, scheduling, observability, audit, cancellation, and recovery.
- Frontends and external platforms connect only to Go.
- Python Agents may request actions only through Go-projected Capabilities. They never call Tools, databases, or external institutional systems directly.
- Persistent connections, credentials, migrations, and physical storage choices remain behind the Go-managed storage layer.
- Services and Tools never hold business database credentials or issue arbitrary SQL.
- A Tool is a reusable atomic capability. A Service is a thin business composition that exposes Capabilities.
- Agents and other external callers see Capabilities, not the internal Tool catalog.
- All Capability, Service, and Tool calls pass through the kernel-recognized dispatcher and inherit governed request context.
- Permissions and data scope may only narrow down a call chain. Internal calls never gain trust or privilege.
- `Deployment` is the physical security boundary. `App` is the business, data, permission, Agent-configuration, and session boundary.
- Cross-process communication uses versioned gRPC and Protobuf. Do not introduce a private transport protocol.
- Logical Service and Tool boundaries do not imply one process, port, queue, or database per component.
- Mutable business state must not live only in process memory. In-memory maps and channels may accelerate delivery but are never the source of truth.
- The Go kernel remains authoritative even when an Agent runtime, Tool host, client connection, or process crashes.

## First Scenario: Campus Bus

- Zhihui Luojia（智慧珞珈）is the sole authoritative source of campus-bus business data.
- The ingestion mechanism remains replaceable. Regardless of whether data arrives through pull, push, file exchange, or another authorized adapter, it is validated and written into Go-managed unified storage before business use.
- Campus-bus Service and Tool code depends on storage ports, never on a concrete database or synchronization method.
- Campus-bus business data currently has no user-state requirement. Do not invent anonymous users, login flows, or user persistence for this scenario.
- No user state does not remove governance: calls still carry `app_id`, `echo_id`, `request_id`, `trace_id`, `run_id`, deadline, call depth, and other applicable context.
- Initial scope covers routes, directions, stops, schedules, and time-based journey planning.
- Real-time location, arrival prediction, proactive reminders, and operational claims require explicitly authorized source fields and must never be fabricated.
- Stable identifiers are used for routes, stops, trips, Services, Tools, Capabilities, Echoes, Runs, and calls. Display names are not primary keys.
- Demo data is always non-authoritative, visibly marked, isolated from production, and impossible to enable accidentally in a production environment.
- Stale, expired, incomplete, or non-authoritative data must produce an explicit governed result; the Agent must not silently present it as current fact.

## Production AI Agent Rules

- The production Python Agent uses a real model and provider-native ToolCall.
- Keyword matching, regex intent parsing, hard-coded business routing, or fixed workflows must never masquerade as an AI Agent.
- Tests may inject deterministic `ModelProvider` implementations. Production must fail readiness when required model configuration is absent.
- Go computes the exact Capability projection for each Run. Python rejects unprojected ToolCalls, malformed arguments, duplicate call IDs, and mismatched results.
- ToolCall arguments must be validated again at the Go trust boundary against the registered Capability schema. Provider-side `strict` mode is not a security boundary.
- Agent execution requires explicit budgets: total deadline, step count, ToolCall count, payload size, output size, token usage, and cost where the provider exposes it.
- Provider calls require explicit timeouts, cancellation propagation, retry classification, bounded exponential backoff with jitter, rate limiting, and no retries for unsafe side effects.
- Health and readiness must reflect actual runtime/provider ability, not merely successful object construction.
- Provider errors are mapped to stable internal error codes and safe public messages. Raw provider bodies, headers, prompts, messages, and credentials never cross into logs or public APIs.
- Multiple ToolCalls must have deterministic ordering and documented concurrency semantics.
- Python process loss must not corrupt authoritative Go state. Durable recovery and retry decisions belong to Go.
- Subagents, when implemented, are Go-created child Runs with `parent_run_id`, narrowed permissions, separate budgets, governed cancellation, and explicit result routing. Python must not create untracked private sub-runs.

## Security And Isolation

- Every storage read, write, update, delete, event subscription, cancellation, and audit query is scoped by `app_id` at the API and SQL boundary where applicable.
- Do not rely on globally unique UUIDs as authorization or App isolation.
- Required permissions declared by Capability and Tool specs must be enforced, not merely stored as metadata.
- Side-effect metadata must drive confirmation, idempotency, retry, and cycle policy.
- External input is untrusted. Enforce body, field, string, collection, frame, event, and gRPC message limits before allocation or persistence.
- Validate structured input with strict decoders and schemas. Unknown fields are rejected unless the versioned contract explicitly permits them.
- Public responses never expose raw internal errors, SQL details, filesystem paths, provider responses, stack traces, or secrets.
- Never log or persist credentials, authorization headers, cookies, user/model message bodies, prompts, Tool arguments, Tool results, or raw sensitive payloads outside their explicitly governed business/audit store.
- Audit storage is not ordinary logging. It requires centralized sanitization, App scoping, retention rules, access control, and deletion policy.
- Insecure gRPC is acceptable only for an explicitly same-Deployment, loopback-only boundary. Any non-loopback or remote boundary requires authenticated transport and a documented trust model.
- Secrets come from an approved secret source, are never committed, are not echoed at startup, and have a rotation/revocation path.
- Real institutional data requires written authorization. Do not scrape, reverse engineer, or import unofficial copies.

## Durable State And Reliability

- Model Echo and Run as explicit durable state machines with validated transitions. State transitions must be atomic and reject invalid or duplicate terminal writes.
- A Run is not merely an in-memory goroutine. Persist Run identity, parent relationship, status, attempt, deadlines, model/config version, sequence state, and recoverable execution metadata.
- Event sequence semantics must support retries and multiple Runs for one Echo without primary-key collisions. Sequence allocation is authoritative and monotonic within its documented scope.
- Creating an Echo and scheduling its Run must survive a process crash between the two operations. Use a durable work/lease/outbox design rather than relying only on `go func`.
- On startup, reconcile abandoned `running` records deterministically: resume, retry, cancel, or fail them according to policy.
- Writes and external side effects require idempotency keys and replay-safe handling. Duplicate client requests and duplicate Agent frames must not duplicate effects.
- Cancellation and deadlines propagate through HTTP, Echo, Run, gRPC, Capability, Service, Tool, and storage operations.
- Cleanup and terminal persistence may detach from a cancelled request only through a new bounded context. Never use unbounded background cleanup.
- Do not ignore errors from state transitions, audit writes, stream sends, shutdown, or cleanup. If a secondary error cannot be returned, record it safely and preserve the primary cause.
- Slow or disconnected SSE clients must not block Runs. Persisted events remain replayable, and subscriber eviction/reconnect behavior must be tested.
- Graceful shutdown stops accepting work, cancels or drains active Runs according to policy, persists terminal/recoverable state, stops the Agent child process, and closes storage within bounded deadlines.
- Liveness and readiness are different: liveness reports process health; readiness reports ability to accept work and may include dependencies.
- Error classification must distinguish validation, permission, conflict, unavailable, timeout, cancelled, rate-limited, retryable dependency, protocol violation, and internal failure.

## Storage And Data Rules

- Domain code depends on narrow ports. Concrete SQLite or future production adapters implement those ports.
- All business tables and queries enforce App isolation. Composite keys and indexes include `app_id` where the data is App-owned.
- Use parameterized SQL. Dynamic identifiers are allowed only from closed, code-defined allowlists.
- Schema changes use forward, reviewable migrations. Never rewrite an already-applied migration; add a new migration and test upgrades from the prior schema.
- Migrations are bounded, observable, crash-safe, and validated against realistic data. Destructive migrations require an explicit backup/rollback plan.
- Enforce relational integrity with appropriate foreign keys, uniqueness, checks, and state constraints; do not rely only on Go validation.
- Snapshot ingestion validates stable IDs, references, time ordering, source revision, authority, freshness, duplicates, and completeness before atomic replacement.
- Failed ingestion preserves the last complete valid revision. Readers never observe a partially imported catalog.
- Time is stored in a documented UTC representation and converted using an explicit IANA time zone at boundaries.
- Production storage requires documented backup, restore, retention, capacity, corruption recovery, and migration procedures.
- SQLite may remain a supported adapter, but do not assume a single-process SQLite configuration satisfies every production deployment. Preserve ports for another Go-managed database adapter.
- Do not add user persistence to the campus-bus scenario until the user confirms the institutional identity boundary.

## Contracts And Versioning

- Public HTTP APIs, SSE event types, Protobuf packages, Capability schemas, Tool specs, and persisted event shapes are versioned contracts.
- Additive compatibility is preferred. Breaking changes require a new version and an explicit migration/compatibility plan.
- Go and Python negotiate or validate protocol compatibility before a Run; a hard-coded unused version field is insufficient.
- Validate frame ordering, identity, sequence, payload size, unknown event types, duplicate calls, late results, and terminal behavior on both sides of gRPC.
- OpenAPI documents actual request and response schemas, error codes, headers, SSE event envelopes, status behavior, and compatibility expectations.
- Generated Protobuf files are committed build artifacts in this repository. Never edit them manually. When `proto/agent.proto` changes, regenerate both Go and Python outputs and include them with tests.
- Registry registration validates IDs, semantic versions, schemas, side-effect values, dependency declarations, and permission declarations atomically.

## Observability And Comments

- All handwritten code comments use Chinese. Generated files, `//go:build`, and `//go:embed` directives are exempt.
- User-facing log messages use clear Chinese and prioritize readability. Stable field keys remain English for machine querying and cross-language consistency.
- Go logs through `internal/observe`; Python Agent logs through `agent.observe`. Do not create component-private logging systems.
- Important boundaries record lifecycle state, applicable IDs, counts, sizes, result status, error class, and duration.
- Propagate `request_id`, `trace_id`, `app_id`, `echo_id`, `run_id`, `parent_run_id`, `call_id`, `capability_id`, `service_id`, and `tool_id` whenever applicable.
- Background work copies only the necessary correlation context before detaching from an HTTP request.
- Suppress or adapt third-party logs that violate Chinese readability, disclose provider endpoints unnecessarily, or bypass redaction policy.
- Logs are necessary but not sufficient. Production observability also requires metrics and distributed tracing for request rate, success rate, model latency, first-token latency, Tool latency, storage latency, retries, cancellations, queue depth, active Runs, token use, and cost.
- Console logs optimize local readability; JSON logs provide stable production ingestion fields.
- Any new sensitive field must include redaction and negative tests proving the value does not appear in console or JSON output.

## Current Implemented Baseline

As of 2026-07-26, the repository implements and tests:

- Go Web Access with Echo creation, status, cancellation, SSE replay/live delivery, health endpoints, and a minimal test page.
- Go Echo/Run orchestration over bidirectional gRPC.
- A real Python model-driven Agent loop using OpenAI-compatible native ToolCall.
- Go Registry, persistent App Capability/permission policy, dispatcher routing, call-depth enforcement, and non-progressing cycle detection.
- `campus` Service with stop, route, and journey Capabilities backed by reusable bus Tools.
- Go-managed SQLite storage for campus-bus snapshots, Echoes, events, and Capability audit.
- App-scoped Echo reads, terminal updates, event persistence/SSE, cancellation tracking, and audit access, with composite SQLite keys and a tested forward migration.
- Stable safe public HTTP and Agent Capability errors; internal causes are excluded from public responses, persisted Echo errors, and SSE failure events.
- Atomic Echo + queued Run creation, durable Run attempts, lease-token claims, validated transactional Run/Echo terminal transitions, queued cancellation, and startup reconciliation.
- Single-Deployment durable Run scheduling with a fixed worker limit, App-scoped transactional capacity backpressure, persisted delayed attempts, periodic lease renewal, and bounded automatic retry only before any write/external Capability.
- A Host-neutral Loader state machine plus Runtime Host 2.0 gRPC client/server validation, shared hosted connection and capacity limits, fatal-runtime fail-fast/recovery, and a real isolated-process supervisor with bounded process-group cleanup.
- Owner-held persistent installed-manifest/lock discovery with strict JSON and digest validation, atomic Loader/Registry startup registration, pinned-runtime warmup, and bounded shutdown wired into the kernel. A real extension Host Backend and hosted OS resource isolation remain P1 blockers.
- Immutable App configuration revisions with generation-based compare-and-swap, current-policy fail-closed enforcement, historical Run configuration recovery, immediate Capability/permission revocation, dynamic model readiness, and App disable behavior.
- One governed Subagent level through the `agent.run` external-side-effect Capability: Go-created durable child Runs with parent/call identity, acceptance-time scope ceilings, current-policy narrowing, independent half budgets, one-child limit, no nested projection or retry, child-before-root cancellation, private result routing, root-only Echo/SSE events, and centralized task/result audit redaction.
- Storage-authoritative Echo event sequencing that remains monotonic across multiple Run attempts and rejects late terminal events.
- Registry-time compilation of strict Capability/Tool JSON Schemas, semantic metadata validation, and atomic Service registration.
- Dispatcher-time Go Schema revalidation, required-permission enforcement and narrowing, and fail-closed SideEffect/idempotency/confirmation policy.
- App-scoped durable idempotency for client Echo creation, Agent calls, audits, and future write/external Capability and Tool calls, including replay, conflict, concurrent wait, and unknown-outcome handling.
- Negotiated Agent protocol version, explicit bidirectional frame sequences, bounded gRPC/frame/field payloads, strict unknown/duplicate/late-frame rejection, and persisted Agent sequence progress.
- Provider timeouts, cancellation, bounded pre-stream retry/backoff with jitter, rate/concurrency limits, token/output/ToolCall budgets, durable usage capture, and model-backed readiness.
- Stable Provider failure codes with accurate retryability, plus suppression of third-party plaintext logging.
- Atomic campus-bus snapshot activation with strict ingestion validation, explicit completeness/authority/validity metadata, fail-closed stale/non-authoritative query governance, and production demo-data rejection.
- Separate process liveness and dependency/model readiness, admission-safe bounded Run shutdown, and graceful managed Python child termination.
- Fail-closed loopback-only insecure gRPC, owner-held restricted secret-file loading in production, and raw-secret environment rejection.
- W3C HTTP trace propagation through Go Run and Python Agent spans, plus fixed low-cardinality Prometheus metrics for HTTP, Runs/queue, first Token, Provider retries, usage/cost, Capability, Tool, and storage.
- Structured Chinese logging, correlation context, centralized raw-error/path suppression, audit sanitization, third-party log suppression, and HTTP request logs.
- SQLite consistent backup, integrity/foreign-key/schema validation, no-overwrite offline restore, real migration-9-to-14 and previous-version-13-to-14 upgrade tests, parent-child Run restore coverage, and documented retention/DR drills.
- Unit tests, Go race tests, `go vet`, Python tests, real isolated Runtime Host process tests, and a real Go → Python root/child Subagent → Capability → storage → SSE integration test.

During the 2026-07-26 production audit, all listed Go unit tests, Python tests, Go race tests, `go vet`, and the cross-process integration test passed. This is baseline evidence for the current behavior, not a permanent waiver; rerun the gates after every relevant change.

This baseline is valuable and must be preserved. It is not evidence that the P1 items below are implemented.

The App-scoped storage/public-error, durable Echo/Run state, Go trust-boundary/idempotency, Agent protocol/Provider reliability, campus-bus source-governance, production-operations, single-Deployment durable scheduler, Runtime Host, persistent App configuration/policy, and one-level governed Subagent baselines were implemented and reverified on 2026-07-26. This closes the audit P0 list and the corresponding initial P1 foundation slices. It does not close governed confirmation storage, multi-entry scope, authorized ingestion, the remaining Loader deployment boundaries, or the other P1 work below.

## 2026-08-12 地基框架基线（PRD 模块一/六/七/十二/十三）

依据 `docs/AI珞地基框架PRD.md` 实现并测试了以下地基模块（全部为独立实现、尚未涉及真实校方身份/数据授权）：

- SQLite 迁移改为版本注册表机制（`registerMigration(version, sql)`），多个模块可独立注册前向迁移，当前 Schema 版本 1–18 连续。
- 身份与授权（迁移 15）：Deployment 级 User、App 级 ExternalIdentity/AppMembership/Role/PermissionGrant/IdentityBindingRevision；外部平台 ID 永不充当内部 user_id；同一外部身份并发绑定互斥（库唯一约束+race 测试）；撤权/禁用即时生效（查询时实时计算）；身份不存在返回 ErrNotFound 不自动创建；跨 App 权限 fail-closed。
- 会话、消息与附件（迁移 16）：Session（direct/group/system）、Message（content_ref、回复、编辑、软删除）、平台消息按 app_id+platform_message_id 去重（并发重复投递只产生一条）、历史查询约束 app_id+session_id、BlobStore 窄端口 + 安全本地文件系统实现（os.Root 沙箱、路径穿越/符号链接/超限拒绝）、消息正文不进日志与审计（redaction 负向测试）。
- 确认与副作用治理（迁移 17）：持久 Confirmation 五态状态机（waiting/approved/rejected/expired/revoked），argument_digest 与范围绑定，CAS 决策、并发重复批准幂等、重启后待确认状态仍在、未知执行结果不伪报；Service 实现 `runtime.ConfirmationVerifier` 并已注入 Dispatcher（未批准 fail-closed）。
- 后台任务与调度（迁移 18）：持久 Task 状态机（租约领取/续期/死亡恢复/有界重试/App 容量隔离/封闭类型注册表+参数 Schema 双重校验/Outbox 事件）；`main.go` 已装配调度器，首个真实消费者为确认过期清扫（`governance.confirmation.expiry`，5 分钟周期，任务自续链，启动播种一轮）。
- 测试基建：`internal/kernel/strictschema` 共享严格 JSON Schema 编译/校验（Registry 与任务类型注册表共用，消除重复实现）；7 个 fuzz 目标（种子随 `go test` 常驻）；task.Event 与 SSE 信封两组 golden 契约。
- 平台接入统一入口：`internal/access` 定义标准 `InboundMessage`/`OutboundMessage` 与 `Hub.Intake`（校验 → 身份解析 → 会话找到或创建 → 消息入库）。Web 适配器已接入该入口：HTTP 请求经标准消息 → 会话/消息持久化（SQLite，匿名发送者）→ Echo 创建，平台与 Agent 历史解耦；重复投递由同一幂等键在消息与 Echo 两侧去重，重放不产生重复消息或重复 Echo。身份服务已装配（匿名 Web 不触发，携带平台身份的消息到达时才解析）。

该批模块的存储/服务/调度代码与测试全部合并并通过 Go 全量测试、`go vet`、`-race`、Python 测试与 e2e 集成门禁（e2e 断言 Web 消息已持久化到会话台账）。

## 2026-08-12 三模式包接入基线（embedded / hosted / isolated）

按"包 = 逻辑单元（Tool/Service）+ 运行模式"的统一架构，落地三种运行模式的 Loader 接入；**campus 完全重构为 hosted 内置包，无旧兼容**（`campus.Register` 直连注册已删除）：

- hosted 生产 Backend（wazero）：`internal/kernel/loader/wasm_host.go` 用 wazero 沙箱执行 wasm32-wasi 工件——线性内存上限（默认 128 MiB）、WASI 裁剪（无文件系统/网络/环境变量）、每次调用独立实例（调用之间零共享状态）、stdin/stdout JSON 信封 ABI（`{tool_id,payload}` → `{ok,result,code,message}`）、宿主函数（host function）线性内存 ABI 投影（guest 以 `//go:wasmimport` 调用），治理上下文按调用绑定（guest 无法伪造 app_id/权限/调用链）。
- campus hosted 化：业务逻辑在 `internal/campus/bus`（纯 Go，`bus.Store` 端口）；guest（`extensions/campus`，wasm32-wasi，`//go:build wasip1`）复用 bus handler，存储经宿主函数 `ailuo.bus.query` 投影（App 隔离/权限在宿主侧强制）；工件 `go:embed` 进内核（`internal/campus/builtin`），`campus.Manifest/ReadArtifact` 以 digest 锁定工件防漂移；链路为 Dispatcher → Loader.Acquire → WasmHost → campus guest → 宿主函数 → bus.Store，Dispatcher 治理（Schema/权限/深度/取消/幂等）与 App 策略不变。
- `CapabilitySpec` 新增 `ToolID`：Dispatcher 把 Capability 直接执行的工具注入治理上下文 `ToolID`，运行时级 Handler（含 hosted guest）据此分发。
- 信封错误码闭环：guest 闭式错误码（`invalid_argument`/`data_unavailable`/`data_incomplete`/`data_untrusted`/`data_expired`/`internal`）由宿主映射为稳定内部错误（数据治理错误保留类别），未知错误码按协议违例拒绝。
- embedded 机制：`internal/kernel/loader/embedded_host.go`（进程内 Runtime，Verify 校验内置包表，生命周期/治理与 hosted/isolated 一致）；当前无生产 embedded 业务包（为内核自有组件以包形式纳管预留），有完整单元测试。
- isolated 资源限额：`ProcessSpec.Limits`（RLIMIT_AS/RLIMIT_CPU/RLIMIT_NOFILE/RLIMIT_FSIZE）由锁固定；Linux 用 `prlimit` 在子进程启动后立即应用，非 Linux Unix 与 Windows 对非零限额 fail-closed；合理上限校验 + Linux 真实进程集成测试（FSIZE 生效）。
- 参考 hosted 包 `extensions/strings.tool`（纯计算字符串工具）演示沙箱契约与分发形态；`extensions/*/build.ps1|build.sh` 交叉编译 wasm32-wasi 工件。
- 测试：campus 完整 hosted 链路行为测试（journey 排序/空结果/App 隔离/快照治理/Schema/取消/深度）、WasmHost 单元与并发隔离测试、host function 投影测试；orchestrator 与 e2e 均改走真实 hosted 链路（装配时 Warmup 提前编译，避免占用 Run deadline）。

## 2026-08-13 多入口身份归一化基线

- 平台事件统一入口：`internal/access/ingress` 提供 `POST /api/v1/ingress/{platform}`——平台适配器把原始事件规范化为标准 `Event` 后推送，内核经 Hub 走"校验 → 身份解析 → 会话找到或创建 → 标准消息入库 → 幂等 Echo 创建"的受治理链路；平台永远不直接创建 Echo、不写消息库、不解析身份（与 Web 适配器同约束）。
- 严格入站契约：64KiB 请求体上限、`DisallowUnknownFields` 严格解码、单 JSON 对象 EOF 校验、平台路径标识正则、字段级校验（platform_message_id/message_type/text≤4000/幂等键复用 `idempotency.ValidateKey`）；非法平台身份标识（含控制字符）映射 400 `invalid_platform_identity`（此前会误落 500）。
- 跨入口共享错误映射：`access.WriteIntakeError`/`WriteEchoError`/`SecurityHeaders` 从 Web 适配器抽取为共享函数，web 与 ingress 共用同一公共错误契约（身份未绑定 401 `identity_not_found`、用户禁用 403 `user_disabled`、平台消息去重键冲突 409 `idempotency_conflict`、队列满 429+Retry-After、App 禁用/配置不可用 503）。
- 幂等闭环：同一平台消息重复投递（相同 platform_message_id + 幂等键）在消息与 Echo 两侧同时去重，重放返回既有 echo 且 created=false；同一 platform_message_id 配不同幂等键被拒绝（409），不产生新消息/新 Echo。
- 会话归一：同一身份用户跨平台消息归一到同一 `session-<user_id>` 会话（消息在会话内按序累积），匿名渠道使用保留匿名会话。
- `identity-bind` 维护命令：`main.go` 新增幂等身份开通（CreateUser 已存在则继续、同一外部身份重复绑定同一用户视为成功重放、绑定其他用户明确报错、成员关系 upsert），支持 --user/--app/--platform/--space/--platform-user/--roles，用于控制面开通内部用户与平台绑定。
- HTTP 接线：外层 ServeMux 组合——`/api/v1/ingress/` 前缀交给 ingress，其余路径交给 Web Access（健康检查、Echo/SSE、演示页面）。
- 测试：ingress 9 个用例（身份解析/去重/会话连续性/幂等冲突/未绑定与禁用/匿名/畸形事件/错误映射）+ identity-bind 幂等与冲突测试 + access 全量回归。

## 2026-08-13 agent 纳管基线（isolated Runtime 同构）

- **agent 不再是特殊进程**：`internal/agenthost` 已整体删除，无任何回退旧代码。内置 AI 执行者（Python Agent）改由 `internal/kernel/loader/agent_host.go` 的 `AgentHost` 以 isolated Runtime 形态纳管，与扩展包完全同构——经 `Manager.Register/Warmup/Acquire/Shutdown` 走统一生命周期，进程启动/资源限额/优雅停止/强制清理复用 loader 进程原语（`startCommandProcess`/`applyProcessLimits`/`terminateCommandProcess`/`killCommandProcess`）。
- 装配契约：`AgentHost` 实现 `Host`（`Manifest`/`Verify`/`Load`），`Load` 生成 `agentRuntime`（`Runtime`）；`Lease.Runtime()` 暴露租约持有的运行时，装配经窄接口 `AgentClientProvider` 取 agent 协议客户端、`AgentProcessLifecycle`（`Done`/`Err`）做受监督进程崩溃监控（异常退出 → 内核 fail-closed 停止）。
- 健康检查内聚到 `internal/kernel/health`：`AgentChecker`（协议协商 + 指定模型 Provider 就绪）、`AgentAppChecker`（读取当前 App 配置的模型与启停状态再检查），与 `Combined` 一起构成 readiness。
- 停止语义与 isolated 扩展一致：Spawn 模式先优雅终止等待（stopGrace），超时强制清理（terminateGrace）；连接模式只关闭 gRPC 连接、不拥有进程。e2e 全链路（Go → Python Agent → 子任务 → Capability → 存储 → SSE）真实走 AgentHost Spawn 路径。

## 2026-08-13 hosted 生产边界基线

- **hosted CPU 时间预算（强制终止，非协作式）**：wazero 无指令级计数（已核实 v1.12 及其全部历史版本），进程内无法预占 guest 忙循环。因此用 wazero 公共特性 `WithCloseOnContextDone(true)`——编译器与解释器**编译期插入周期检查**，调用 context 取消/超时即关闭模块强制终止执行——叠加**每次调用执行时间预算**（`WasmHostConfig.CallTimeout`，默认 30 秒，0 不表示不限时而是取默认）。预算耗尽按 `context.DeadlineExceeded` 归类（内核既有稳定超时分类），不视为协议违例；测试用死循环 guest 工件（`testdata/busy.wasm`，Go 交叉编译）验证 300ms 预算内被强制终止，且预算经外部 Runtime Host 协议同样生效。
- **外部 Runtime Host 进程产品接线**：`RuntimeHostBackend` 生产实现 `hostedRuntimeBackend`（宿主进程内 wazero 执行，含内存上限与执行时间预算）；`RuntimeHostProtocolServer` 服务端首次拥有真实 Backend（此前只有测试 fake）；全链路测试（真实 wazero 执行经完整协议被内核 GRPCHost 调用，含预算强制跨协议生效）；`main.go` 新增 `runtime-host` 子命令（`--install-root` + `--address`，独立信号上下文，loopback/Unix socket 强制）。**host function 是内核特权**：需要宿主函数投影的工件（内置 campus）只能内核进程内执行，外部宿主承载无宿主函数的 hosted 包——这是架构契约，不是降级路径。安装目录配置了 hosted 包而宿主地址缺失时内核拒绝就绪（fail-closed，无进程内回退）。
- **Windows Job Object 资源限额**（替换原 fail-closed）：`process_limits_windows.go` 用 Job Object 强制 `MaxAddressBytes`（JOB_OBJECT_LIMIT_PROCESS_MEMORY，等效 RLIMIT_AS）与 `MaxCPUSeconds`（JOB_OBJECT_LIMIT_JOB_TIME + 耗尽终止动作，等效 RLIMIT_CPU）；无论限额是否为零都创建 Job 并附加 KILL_ON_JOB_CLOSE（等效 Unix 进程组清理，且内核崩溃后由 OS 兜底防孤儿）；Job 句柄由 `commandProcess.release` 持有、子进程回收后在 `finish()` 释放（提前释放会立即误杀，这是 `applyProcessLimits` 签名改为返回释放器的原因）。`max_open_files`/`max_file_bytes`：Windows 无对应进程级原语，非零值 fail-closed（正确语义而非降级）。集成测试验证 1 秒 CPU 限额下死循环子进程被系统强制终止。其余平台（macOS/BSD、Plan 9 等）维持非零限额 fail-closed。

## Known Production Blockers

Unless the user explicitly reprioritizes, address these before expanding product breadth.

### P0

No unresolved item remains from the 2026-07-26 P0 audit. This is dated evidence, not a permanent waiver; a new failure, threat model, deployment boundary, or audit finding becomes P0 when its impact warrants it.

### P1 — Designed But Not Yet Implemented

- Hosted 包余下的生产边界：外部 hosted 包安装目录分发在非 Unix 平台受属主校验 fail-closed 限制（内置 hosted 包如 campus 不受影响）。CPU 执行时间预算（2026-08-13 基线）、外部 Runtime Host 进程接线（`runtime-host` 子命令）与 Windows Job Object 资源限额（2026-08-13 基线）已落地；wazero 指令级计数为上游缺失能力，进程内以时间预算强制替代，更细粒度预算依赖上游提供。
- A production database adapter if deployment requirements exceed SQLite.
- Authorized Zhihui Luojia ingestion adapter.

Do not implement the alternative Tool/package-management comparison owned by another person unless the user explicitly reassigns it.

## Repository Conventions

- Keep kernel contracts under `internal/kernel`.
- Keep campus domain and Service composition under `internal/campus`.
- Keep storage implementations behind domain/kernel ports; test adapters may live under `internal/storage/memory`.
- Prefer the Go standard library unless a maintained dependency materially reduces security or correctness risk.
- Pin dependencies intentionally, review release notes, and do not perform unrelated bulk upgrades.
- Use structured serialization APIs. Do not build protocol or persisted data with ad hoc strings.
- Validate inputs at every trust boundary and preserve wrapped internal error causes while returning safe external errors.
- Thread `context.Context` through every blocking or external Go operation. Python async operations must preserve cancellation and deadlines.
- Avoid hidden mutable global business state. Centralized immutable/configured infrastructure such as the logging facade is the exception, not a pattern for domain state.
- No panic in production request paths. Startup invariant failures must produce explicit actionable errors.
- Preserve user changes in a dirty worktree and do not rewrite unrelated code.
- Never commit credentials, `.env`, local databases, real campus data, caches, virtual environments, or temporary output.
- Do not manually edit generated files.

## Testing And Quality Gates

A happy-path test is not production evidence. Add focused negative and failure-path tests alongside implementation.

Required coverage includes, where applicable:

- registration atomicity and invalid specs;
- strict schema and boundary validation;
- App isolation on every read and write path;
- permission narrowing and denial;
- idempotency and duplicate delivery;
- valid and invalid state transitions;
- retries, restart recovery, and abandoned Runs;
- cancellation, timeout, late results, and cleanup failure;
- event ordering, SSE reconnect, slow subscribers, and backpressure;
- data freshness, source revision, invalid references, and atomic ingestion;
- model rate limit, timeout, malformed stream, partial ToolCall, and provider failure;
- protocol mismatch, duplicate frames, oversized payloads, and unknown frame types;
- concurrency-sensitive code under the race detector;
- migrations from the previous schema, backup, and restore behavior;
- logging and public-error non-disclosure.

Before considering a change complete, run the relevant focused tests and then the full gates:

```bash
gofmt -w .
GOCACHE=/tmp/github.com/projectluojia/AI-Luo-Man-ga-gocache go test ./...
uv sync --project agent --locked
uv run --project agent --locked python -m compileall -q agent
uv run --project agent --locked python -m unittest discover -s agent -p 'test_*.py' -v
GOCACHE=/tmp/github.com/projectluojia/AI-Luo-Man-ga-gocache go test -race ./...
GOCACHE=/tmp/github.com/projectluojia/AI-Luo-Man-ga-gocache go vet ./...
GOCACHE=/tmp/github.com/projectluojia/AI-Luo-Man-ga-gocache go test -tags=integration ./internal/kernel/loader -v -timeout=30s
GOCACHE=/tmp/github.com/projectluojia/AI-Luo-Man-ga-gocache go test -tags=integration ./e2e -v -timeout=30s
```

If a gate cannot run, report exactly why. Do not claim it passed. Do not weaken or delete tests merely to obtain green output.

## Definition Of Done

A task is complete only when:

- the root cause or durable contract is addressed rather than patched around;
- trust boundaries, App isolation, cancellation, concurrency, and failure behavior have been considered;
- state changes are atomic and recoverable where required;
- external errors are stable and safe;
- observability is detailed without leaking protected content;
- focused positive, negative, and failure-path tests exist;
- affected design, OpenAPI, Protobuf, migrations, and operational documentation are updated;
- formatting, unit tests, race tests, vet, Python tests, and applicable integration tests pass;
- the final report distinguishes completed work, remaining production blockers, and validation evidence.

Production-grade does not mean implementing every future feature in every task. It means that every feature actually implemented is safe to keep.

## Communication And Decisions

- Make ordinary technical decisions autonomously: package layout, types, algorithms, validation, tests, and implementation details do not require user confirmation.
- Ask only about macro decisions that materially change product scope, ownership, institutional coordination, data authorization, delivery targets, deployment trust boundaries, or architecture ownership.
- The user validates outcomes. Report important tradeoffs, migration impact, security impact, validation evidence, and genuine blockers directly.
- When a macro decision is confirmed, update this file or the relevant design document so future sessions inherit it.
- Do not overstate maturity. Say “implemented and verified” only with evidence, and say “designed” or “planned” when that is the truth.

## Current Work Order

When the user says only “continue”, begin with the highest unresolved P0 blocker. If no P0 exists, continue the P1 platform foundation before adding product breadth. The recommended order is:

```text
Product Host Backend wiring and hosted OS resource isolation
  -> multi-entry identity/session/message normalization
  -> authorized real-data ingestion
  -> additional product capabilities
```

Preserve the current durable vertical slice:

```text
request context
  -> App Capability policy
  -> Registry Capability route
  -> campus Service
  -> campus bus Tool
  -> Go-managed storage port
  -> structured result
```

Web Access, Python Agent, future persistent storage adapters, and the authorized Zhihui Luojia ingestion adapter attach to this slice without moving its ownership boundaries.
