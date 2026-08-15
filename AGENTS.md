# AI珞（爱珞）V3 Repository Instructions

## Production Standard

AI珞 V3 是长期维护的生产级项目。功能范围可以窄，但已实现的契约、信任边界、状态转换、持久化、错误路径和测试必须可长期保留。不得以 “MVP”“临时方案” 或 “以后重写” 降低正确性、安全性、耐久性、隔离性与可观测性。

必须准确描述状态：区分设计、已实现基线、测试证据和剩余生产阻塞项。当前状态与路线图见 `docs/仓库状态与路线图.md`，不得把未实现设计描述为已完成。

用户是验收负责人。涉及产品范围、机构数据授权、部署信任边界或外部协调的重大决定由用户确认；普通实现细节自主完成。

## Required Reading

架构、协议、持久化、Agent Runtime 或安全改动前依次阅读：

1. `AGENTS.md`
2. `docs/v3_overall_design.md`
3. `docs/仓库状态与路线图.md`
4. `docs/校巴场景设计.md`
5. `docs/日志与可观测性设计.md`
6. `docs/数据需求与授权清单.md`（真实数据或采集工作）

设计文档不是实现证据；必须检查当前代码与测试。

## Architecture

- Go kernel 是唯一系统核心和事实来源，负责 Access、Echo/Run 编排、授权、Registry、路由、Loader、持久化、调度、审计、取消和恢复。
- 前端与外部平台只连接 Go。Python Agent 只负责认知，通过 Go 投影的 Capability 请求动作，不拥有授权、系统状态、外部访问、调度或业务持久化。
- Tool 是可复用原子能力；Service 是薄业务组合并暴露 Capability。Agent 和外部调用方只看 Capability，不看内部 Tool 目录。
- 所有 Capability、Service、Tool 调用经过内核 Dispatcher，并携带受治理上下文。权限和数据范围只能收窄，内部调用不得提权。
- `Deployment` 是物理安全边界；`App` 是业务、数据、权限、Agent 配置和会话边界。
- 跨进程通信使用版本化 gRPC/Protobuf，不引入私有传输协议。
- 逻辑边界不等于一组件一进程、一端口、一队列或一数据库。
- 可变业务状态不得只存在内存。Go 在 Agent、Tool Host、客户端或进程崩溃后仍保持权威。
- Subagent 是 Go 创建的持久 child Run，具有 `parent_run_id`、收窄权限、独立预算、受治理取消和显式结果路由；Python 不得私建未跟踪子任务。

## Repository Layout

- `internal/kernel`：治理、编排、协议、Registry、Loader 和领域端口。
- `internal/access`、`internal/storage`、`internal/observe`：平台基础设施。
- `internal/tools`：跨 Service 可复用的原子 Tool，绝不反向依赖 Service。
- `internal/services`：业务 Service 与运行时装配；通过 `ToolDependencies` 使用 Tool。
- 具体存储实现位于领域/内核窄端口之后；测试适配器可放 `internal/storage/memory`。
- Python 环境和依赖仅由 `uv`、`agent/pyproject.toml` 与提交的 `agent/uv.lock` 管理。
- 普通 Go/Web/SQLite/Python 开发路径须兼容 Windows、Linux、macOS；平台专属强安全边界在不支持的平台 fail closed。

## Campus Bus Boundary

- 智慧珞珈是校巴业务数据唯一权威来源。真实数据必须有书面授权，不抓取、逆向或导入非官方副本。
- 数据先校验并写入 Go 管理的统一存储，再供业务使用；Service/Tool 依赖存储端口，不依赖具体同步方式或数据库。
- 当前校巴业务没有用户状态需求，不发明匿名用户、登录流程或用户持久化；调用仍携带完整治理上下文。
- 实时位置、到站预测、提醒和运营结论必须有明确授权字段，不得编造。
- Demo 数据必须非权威、显式标记、与生产隔离且不能在生产环境误启用。
- 过期、不完整、非权威数据必须返回明确受治理结果，不得静默当作当前事实。

## Agent And Protocol Rules

- 生产 Agent 使用真实模型和 Provider 原生 ToolCall；关键词、正则或固定流程不得伪装为 AI Agent。
- Go 为每个 Run 计算精确 Capability 投影；Python 拒绝未投影调用、畸形参数、重复 call ID 和错配结果。
- ToolCall 参数在 Go 信任边界再次按注册 Schema 验证；Provider strict mode 不是安全边界。
- Run 必须具备 deadline、步骤、ToolCall、载荷、输出、Token 和可用成本预算。
- Provider 调用具备超时、取消传播、稳定错误分类、限流、并发限制和有界退避；不安全副作用不自动重试。
- readiness 反映真实模型/Provider 能力，不得只检查对象构造成功。
- 原始 Provider 响应、Headers、提示词、消息和凭据不得进入日志或公共 API。
- 多 ToolCall 的顺序与并发语义必须确定并有测试。
- Protobuf 是版本化契约；修改 `proto/executor.proto` 后重新生成并提交 Go/Python 生成物，禁止手改生成文件。

## Security And Data

- 所有适用的存储读写、事件订阅、取消和审计查询在 API 与 SQL 边界按 `app_id` 隔离；UUID 唯一性不是授权。
- 外部输入不可信。分配或持久化前限制 body、字段、字符串、集合、帧、事件和 gRPC 消息大小。
- 使用严格解码和 Schema；除非版本化契约允许，否则拒绝未知字段。
- 公共响应不得泄露内部错误、SQL、文件路径、Provider 响应、堆栈或秘密。
- 不在普通日志或非授权存储记录凭据、认证头、Cookie、用户/模型正文、提示词、Tool 参数或 Tool 结果。
- 审计不是普通日志，必须集中净化、App 隔离，并具备保留、访问与删除策略。
- 非 loopback/远程边界需要认证传输和文档化信任模型；明文 gRPC 仅限显式同 Deployment loopback。
- Secret 来自批准的秘密源，不提交、不启动回显，并具备轮换和撤销路径。

## Durable State

- Echo、Run、Task、Confirmation 等使用显式持久状态机；转换原子化，拒绝非法转换和重复终态写。
- Run 持久化身份、父子关系、状态、attempt、deadline、配置版本、序号、预算和恢复元数据，不只是 goroutine。
- Echo 创建与 Run 入队必须跨崩溃安全；调度使用持久 work/lease，不只依赖 `go func`。
- 启动时确定性处理遗留 `running` 记录；重试、恢复、取消或失败策略必须明确。
- 写入和外部副作用使用幂等键并支持重放安全；未知执行结果不得伪报成功或失败。
- 取消和 deadline 贯穿 HTTP、Echo、Run、gRPC、Capability、Service、Tool 与存储。
- 取消后的终态/清理只能用新的有界 context，不使用无界后台清理。
- 慢或断开的 SSE 客户端不得阻塞 Run；事件持久可重放，驱逐与重连有测试。
- 优雅关闭停止接入、取消或排空活动工作、保存状态、停止 Agent 子进程并在期限内关闭存储。

## Storage And Contracts

- 使用参数化 SQL；动态标识仅来自闭式代码 allowlist。
- Schema 只做新的前向迁移，不改写已应用迁移；测试从上一版本升级。
- 表结构使用外键、唯一约束、检查约束和包含 `app_id` 的复合键/索引，不只依赖 Go 校验。
- 快照导入在原子替换前验证稳定 ID、引用、时间、来源修订、权威性、完整性和重复项；失败保留上一完整版本。
- 时间使用文档化 UTC 表示，边界转换使用显式 IANA 时区。
- 保持数据库适配端口；SQLite 支持不代表适合所有生产部署。
- 公共 HTTP、SSE、Protobuf、Capability Schema、Tool spec 和持久事件都是版本化契约；优先加法兼容。
- OpenAPI 必须描述真实请求、响应、错误、Headers、SSE 信封和状态行为。

## Code And Observability

- 所有手写代码注释使用中文；生成文件、`//go:build`、`//go:embed` 例外。
- Go 日志统一使用 `internal/observe`，Python 使用 `agent.observe`，不创建组件私有日志系统。
- 用户可见日志使用清晰中文；稳定字段键保持英文。
- 适用时传播 `request_id`、`trace_id`、`app_id`、`echo_id`、`run_id`、`parent_run_id`、`call_id`、`capability_id`、`service_id`、`tool_id`。
- 新敏感字段必须加入净化并有 console/JSON 负向测试。
- 优先标准库；仅在维护良好的依赖显著降低风险时引入，并有意锁定版本。
- 所有阻塞或外部 Go 操作传递 `context.Context`；Python async 保留取消和 deadline。
- 生产请求路径不 panic；启动不变量失败返回明确可行动错误。
- 保留 dirty worktree 中的用户改动，不改无关代码；不提交凭据、`.env`、本地数据库、真实校方数据、缓存、虚拟环境或临时输出。

## Git Conventions

- 提交信息遵循 Conventional Commits：`feat`、`fix`、`docs`、`style`、`refactor`、`perf`、`test`、`build`、`ci`、`chore`、`revert`。
- `main` 使用 squash 合并和 required checks；一个提交应是可独立构建、回滚的自洽逻辑单元。
- 分支使用 `feat/`、`fix/`、`chore/`、`docs/`、`refactor/` 前缀。
- 依赖升级遵循 `renovate.json5`；Protobuf/grpc 生成工具链升级必须人工重新生成并评审。

## Validation

新增功能按适用范围覆盖：严格边界、App 隔离、权限收窄、幂等、状态转换、重启恢复、取消/超时、事件顺序、SSE 重连、背压、协议违例、并发/race、迁移/备份恢复和敏感信息不泄露。

先跑最相关测试，再跑完整门禁：

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

门禁无法运行时准确说明原因，不声称通过，不通过削弱测试换绿。

## Definition Of Done

完成意味着：解决根因和可持久契约；考虑信任边界、App 隔离、取消、并发和失败；状态原子可恢复；公共错误稳定安全；可观测但不泄密；正向、负向与失败测试到位；设计/OpenAPI/迁移/运维文档同步；相关门禁通过；最终报告区分已完成、证据与剩余阻塞。
