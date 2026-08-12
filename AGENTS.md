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

## Known Production Blockers

Unless the user explicitly reprioritizes, address these before expanding product breadth.

### P0

No unresolved item remains from the 2026-07-26 P0 audit. This is dated evidence, not a permanent waiver; a new failure, threat model, deployment boundary, or audit finding becomes P0 when its impact warrants it.

### P1 — Designed But Not Yet Implemented

- Product wiring of a real extension Host Backend and hosted shared-process CPU/memory/filesystem isolation. Persistent installed-manifest/lock discovery and kernel startup registration, the Runtime Host protocol, client/server validation, hosted connection/capacity boundary, and isolated process lifecycle are implemented baselines.
- Multi-entry identity/session/message normalization.
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
