# PR #2 审查问题清单

## 基本信息

- PR：`#2 feat: 地基框架、平台接入与执行者协议中性化（2026-08 主线）`
- 基准分支：`main`
- 修改分支：`feat/qq-platform`
- 本轮审查头提交：`26436fe4522e261d361192af464278c7e9423665`
- 审查结论：`Request changes`
- GitHub CI：Linux 完整质量门禁、Ubuntu、macOS、Windows 均通过

本文记录审查问题及其关闭状态。已经通过最新提交修复并复核的 WAL 模式校验、迁移 21 非破坏化、孤儿 queued Run 确定性失败、QQ 重放读取失败告警和 Runtime Host 生产接线，不再列为待办。

## 建议处理顺序

1. `PR2-001` 平台 ingress 认证与来源绑定（用户确认忽略，不实施）
2. ~~`PR2-002` 会话隔离与会话键规则~~（已修复并验证）
3. ~~`PR2-003` 跨入口幂等命名空间与完整指纹~~（用户确认忽略，不实施）
4. `PR2-004` 消息、Echo、Run 的原子接纳
5. `PR2-006` QQ 回复副作用幂等
6. `PR2-005` QQ WebSocket 解耦与有界并发
7. ~~`PR2-007` QQ `@` 目标校验~~（已修复并验证）
8. `PR2-008` `/chat/stream` 关闭接入竞态
9. `PR2-009` QQ Compose 安全默认值

---

## PR2-001：平台 ingress 缺少认证与可信来源绑定（忽略）

- 优先级：P1
- 状态：用户确认忽略并接受现状风险（2026-08-15），不实施修复
- 类型：安全边界、身份冒用、资源滥用
- 位置：`internal/access/ingress/ingress.go`、`main.go`

### 问题

`POST /api/v1/ingress/{platform}` 被直接挂载到主 HTTP 服务。处理器没有认证、签名、重放窗口或适配器身份校验，直接信任路径中的 `platform` 以及请求体中的 `platform_space_id`、`platform_user_id` 和平台消息标识。

攻击者只要能访问 HTTP 服务，就可以伪造任意平台事件：

- 使用已经绑定的外部平台身份，以该内部用户身份创建 Echo；
- 将 `platform_user_id` 留空，绕过身份解析并走匿名入口；
- 批量创建持久消息和 Run，消耗 App 队列、模型额度与存储容量；
- 猜测或复用平台消息标识，触发幂等冲突或错误重放。

### 未实施方向（保留风险）

- 为每个平台适配器建立可轮换的机器身份，不允许匿名调用平台 ingress；
- 使用经过认证的适配器配置确定 `platform`，不能把 URL 路径当作可信身份；
- 对远程边界使用认证传输；若仅允许同 Deployment 调用，应限制到 owner-held Unix socket 或明确的 loopback 边界；
- 加入时间戳、请求签名或其他有界重放防护；
- 平台 ingress 不应接受“空平台用户即匿名”的降级，匿名 Web 入口与平台入口必须分离；
- 公共错误不得泄露某个外部身份是否已经绑定。

### 验收标准

- 无凭据、错误凭据、过期凭据、错误平台绑定均被拒绝；
- 已认证适配器不能冒充其他平台；
- 同一签名请求的允许重放行为有明确且受测试的幂等语义；
- 日志不记录凭据、签名原文、消息正文或外部身份原始敏感载荷；
- OpenAPI 明确认证方式、401/403 契约与信任边界。

---

## PR2-002：会话键导致跨渠道和跨用户上下文串线（已修复）

- 优先级：P1
- 状态：已修复并验证（2026-08-15）
- 类型：数据隔离、隐私、上下文正确性
- 位置：`internal/access/hub.go`、`internal/kernel/contextasm/contextasm.go`

### 问题

`Hub.sessionIDFor` 的实际实现与注释不一致：

- 已解析用户始终使用 `session-<user_id>`，没有包含平台、群/私聊渠道、平台空间或平台会话；
- 所有匿名 Web 请求始终使用固定的 `web-anonymous`；
- `ensureSession` 始终创建 `direct` 会话，即使来源是 QQ 群；
- 会话已存在时直接忽略新的平台绑定，后续渠道关系不会被写入。

上下文装配器按 `app_id + session_id` 读取历史，因此会出现：

- 同一用户在多个 QQ 群、QQ 私聊和其他平台之间共享历史；
- 不同匿名 Web 用户共享历史；
- 群聊被错误建模为单成员 direct 会话；
- Agent 在当前对话中看到另一个会话的历史内容。

### 修复方向

产品决策（2026-08-15）：系统不再支持匿名用户。这里不再为匿名 Web 设计独立会话，而是删除匿名发送者、共享匿名会话及其降级路径。校巴 Capability“不依赖用户态数据”不等于接入层允许匿名调用。

按以下顺序修复：

1. 收紧身份前置条件：所有用户消息都必须携带可由 Go 解析的可信平台身份，并解析为内部 `user_id` 和有效 AppMembership；空身份、未绑定身份、禁用用户或缺少身份解析器均在创建 Session、Message、Echo 前拒绝。
2. 收紧 Web 边界：Web 请求体中的 `user_id`、`user_name`、`session_id` 都不能作为身份凭据。可信 Web 登录态及其服务端会话尚未实现时，聊天和 Echo 创建入口返回稳定 401，不写消息、不创建 Echo；状态、健康检查等非聊天接口不受影响。
3. 删除匿名模型：移除 `AnonymousSenderID`、`AnonymousSessionID`、`ErrAnonymousOnly` 及 `nil IdentityResolver` 表示匿名渠道的构造语义。Hub 必须持有身份解析器，并以 fail-closed 方式处理配置错误。
4. 定义会话键 v1：以规范化结构的稳定摘要生成 `session-v1-<sha256>`，摘要输入包含版本、`app_id`、平台和 session 类型。direct 还包含可信内部 `user_id`、平台空间与平台会话；group 包含平台空间与平台会话但不包含单个发送者，使同一群成员共享群历史。外部原始标识不直接出现在 `session_id` 中，最大合法输入下仍不超过 128 字节。
5. 正确建模会话：private 创建 `direct`，group 创建 `group`；direct 只允许对应内部用户，group 在每条已解析消息到达时原子确保发送者为成员。既有会话的类型、键材料或平台绑定不一致时显式报冲突，不能把消息写入近似匹配的会话。
6. 扩展存储原子操作：用单事务的“确保会话及成员/绑定”替代 `CreateSession` 后忽略 `ErrSessionExists`。事务必须校验既有 session 类型和平台绑定，并幂等补写合法成员；任一冲突整体回滚。
7. 隔离旧数据：新消息只使用 v1 会话键，不再读取 `session-<user_id>` 或 `web-anonymous` 历史。旧会话保留为不可自动归属的 legacy 数据，按保留策略离线处理；除非能够审计证明来源，否则不得自动迁移或拼入新会话。
8. 更新公共契约：OpenAPI 明确聊天入口要求已认证用户、401/403 稳定错误及请求体身份字段不可信；系统架构说明在代码落地后同步删除匿名渠道现状描述。

### 验收标准

- 同一用户在两个群、群聊与私聊、QQ 与其他平台之间的历史完全隔离；
- 未认证 Web 请求、空平台身份和未绑定身份均在任何持久化前被拒绝，系统中不再创建匿名用户或匿名会话；
- 群会话拥有正确类型和成员语义；
- 会话键在最大合法输入下仍满足存储长度约束；
- 旧格式会话不会被新会话上下文读取，也不会被静默改归属；
- App 隔离、身份撤销、重启恢复和历史上下文装配均有覆盖测试。

### 修复结果

- Hub 构造强制身份解析器，空身份、未绑定身份、禁用用户、无 AppMembership 与非法身份上下文均在持久化前 fail closed；匿名常量与匿名降级路径已删除。
- Web Access 新增可信 `WebAuthenticator` 边界；主程序未配置认证器时 `/api/v2/echoes` 与 `/chat/stream` 返回 401，测试与 e2e 通过显式可信认证器验证完整链路，请求体身份字段不被信任。
- 会话键升级为固定 75 字节的 `session-v1-<sha256>`；direct、group 使用不同键材料，跨群、群/私聊和跨平台历史完全隔离，旧格式会话不会被新上下文读取。
- SQLite `EnsureSession` 在单事务内创建或校验会话与平台绑定，并幂等补写群成员；绑定或成员角色冲突整体回滚。
- 已通过 focused Go 测试、`go test ./...`、`go test -race ./...`、`go vet ./...`、Python 编译与 28 项单测、Loader integration、Go→Python e2e、OpenAPI YAML 解析和 `git diff --check`。

---

## PR2-003：平台消息与 Echo 幂等键缺少入口命名空间（忽略）

- 优先级：已降级，不处理（原审查 P1）
- 状态：用户确认忽略并接受低概率请求冲突风险（2026-08-15），不实施修复
- 类型：幂等正确性、跨平台冲突、错误结果路由
- 位置：`internal/storage/sqlite/session_store.go`、`internal/storage/sqlite/echo_store.go`、`internal/kernel/echo/orchestrator.go`

### 问题

当前消息唯一索引是 `(app_id, platform_message_id)`，Echo 创建幂等映射是 `(app_id, idempotency_key)`，都没有平台或入口作用域。Echo 请求指纹又只包含消息正文，没有覆盖用户、会话、标准消息标识、渠道或平台。

复核后的实际影响：

- 两个平台只有在产生完全相同的原始编号时才会冲突；当前 Web 默认使用随机 UUID，QQ 使用平台消息编号，自然碰撞概率极低；
- 消息存储会继续比较会话、发送者、正文和平台消息编号，不一致时在创建 Echo 前返回 409，因此正常链路不会把其他用户的旧 Echo 当作成功结果返回；
- 当前现实影响主要是第二条合法消息被拒绝，需要重新投递，而不是回复发错人或读取他人结果；
- 通用 ingress 可以人为提交碰撞编号，但这属于用户已确认忽略的 PR2-001 入口信任风险，单独扩展幂等存储不能解决该入口问题。

OpenAPI 声称平台消息“按 App 和平台去重”，与数据库实际约束不一致。

### 原建议修复方向（不实施）

- 增加前向迁移，将平台消息唯一约束改为至少 `(app_id, platform, platform_message_id)`；
- 在消息记录中持久化平台字段，而不是只在会话绑定中间接表达；
- Echo 幂等操作引入闭式 `scope`，例如入口类型和平台；
- 使用规范化 JSON 计算完整请求指纹，覆盖所有影响结果与路由的语义字段；
- 明确客户端幂等键与平台投递标识是两个不同概念，不应直接混用；
- 冲突响应不得暴露其他身份或会话的 Echo 标识。

### 验收标准

- 不同平台、入口、用户和会话使用相同原始键时互不影响；
- 相同作用域、相同完整请求安全重放；
- 相同作用域、任一语义字段变化均稳定返回冲突；
- 从上一迁移版本升级后，既有数据得到确定性处理且无唯一键丢失；
- OpenAPI、数据库约束和实现注释保持一致。

---

## PR2-004：消息持久化与 Echo/Run 创建不是一个原子接纳事务

- 优先级：P1
- 类型：持久状态一致性、崩溃恢复
- 位置：`internal/access/hub.go`、`internal/access/ingress/ingress.go`、`internal/access/web/chat.go`、`internal/access/web/server.go`、`internal/access/qq/qq.go`

### 问题

所有入口都先调用 `Hub.Intake` 提交会话和消息，再调用 `Orchestrator.CreateIdempotent` 创建 Echo 和 queued Run。两步之间的进程崩溃、超时、取消、队列满、配置不可用或幂等冲突会留下没有对应 Echo 的孤儿消息。

“消息与 Echo 使用同一幂等键”只能避免部分重复，不能提供原子性，也不能在崩溃后自动补偿缺失的 Echo。

### 修复方向

- 首选在 Go 管理的存储层提供窄的原子接纳端口，在同一事务中提交会话、消息、Echo、首个 Run 和幂等映射；
- 如果未来消息与 Run 不在同一物理数据库，使用 durable outbox/inbox 状态机，不能依赖请求 goroutine 补偿；
- 接纳记录必须包含明确状态，重启后能够确定性完成、重试或失败；
- 请求取消只能取消等待，不能把已提交状态留在不确定结果中；
- 队列容量检查与接纳事务必须保持 App 作用域和并发原子性。

### 验收标准

- 在每个事务边界注入失败后，数据库中不存在“有消息无接纳状态”或“有 Echo 无消息”的不可恢复组合；
- 并发重复请求只产生一条标准消息、一个 Echo 和一个首 attempt；
- 崩溃重启能够恢复未完成接纳；
- Web、chat、QQ、ingress 复用同一受治理接纳原语。

---

## PR2-005：QQ WebSocket 读取循环被单条 Run 同步阻塞

- 优先级：P1
- 状态：已修复并验证（2026-08-15）
- 类型：可用性、背压、连接可靠性
- 位置：`internal/access/qq/qq.go`

### 修复结果

- WebSocket 读取循环只负责解码、过滤动作响应并把消息投入有界 Echo 队列，不再同步等待 Run 终态；
- 使用 4 个固定 worker 实现 Echo 级并发，不按群、私聊或会话串行，不创建无界 goroutine；
- 队列容量固定为 32，满载时明确拒绝新消息并记录安全元数据；
- OneBot 连接指针的设置、清理与并发回复统一经过发送锁；
- 上下文取消会关闭正在读取的连接，worker 随后有界退出；
- 新增真实 WebSocket 并发测试，证明 4 个 Echo 可同时处理、第 5 个等待 worker，且适配器可正常关闭。

### 验证证据

- `go test ./internal/access/qq`：通过；
- `go test -race ./internal/access/qq`：通过；
- `go test ./...`：通过；
- `go test -race ./...`：通过；
- `go vet ./...`：通过；
- Python `py_compile` 与 28 项单元测试：通过；
- Loader integration：通过；
- Go → Python e2e integration：通过。

### 问题

`serve` 在读取一条 OneBot 消息后同步执行 `handleEvent`。`handleEvent` 创建 Echo 后调用 `waitReply`，默认最多等待三分钟。在此期间，唯一读取循环无法继续处理：

- 后续 QQ 消息；
- OneBot 动作响应；
- notice/poke 事件；
- WebSocket ping/pong 和断线检测。

单个慢模型请求即可造成整个 QQ 接入连接头阻塞，消息积压还可能触发服务端断连。

### 修复方向

- 读取循环只负责严格解码、分类和投入有界队列，不能等待 Run 终态；
- 使用固定 worker 数和明确队列上限，满载时执行可观测的拒绝或降级策略；
- 定义同一会话内是否需要顺序执行，不允许用全连接串行替代会话级顺序；
- 连接关闭、进程关闭和 Run 取消必须传播到 worker；
- WebSocket 控制帧与动作响应必须持续被读取；
- 不创建无界 goroutine。

### 验收标准

- 第一条消息阻塞时，第二条消息和控制帧仍能被读取；
- 并发数和队列深度严格受限；
- 队列满、连接断开、关闭和超时都有确定性行为；
- race 测试覆盖连接、发送锁、worker 关闭和事件订阅。

---

## PR2-006：QQ 幂等重放仍会重复发送回复

- 优先级：P1
- 类型：外部副作用幂等、重复投递
- 位置：`internal/access/qq/qq.go`

### 问题

QQ 处理器忽略 `CreateIdempotent` 返回的 `created`。平台重复投递同一消息时，代码仍会进入 `waitReply`，从持久事件中立即读到旧的 `reply.final`，然后再次调用 `send_group_msg` 或 `send_private_msg`。

因此当前实现只保证“不重复创建 Echo”，没有保证文档声明的“不重复回复”。回复是外部副作用，简单依赖 Echo 幂等不足以控制它。

### 修复方向

- 短期至少不能在 `created=false` 时无条件重发已有终态；
- 生产方案应建立 App 作用域的 durable outbound/outbox 记录，保存来源消息、目标渠道、回复摘要、投递状态和幂等标识；
- 明确“Run 成功但发送前崩溃”“发送成功但确认前崩溃”“OneBot 返回未知结果”的恢复策略；
- OneBot 动作响应应通过唯一 `echo` 相关联，不能把写入 socket 当作发送成功；
- 未知结果不得自动无限重试，也不能伪报成功。

### 验收标准

- 相同平台事件重复投递不会产生第二次回复；
- 在回复发送各阶段注入崩溃后，恢复行为符合明确策略；
- 回复失败和未知结果有稳定错误分类、指标与安全日志；
- 不在普通日志或幂等记录中保存回复正文。

---

## PR2-007：QQ 群聊 `@` 过滤没有比较机器人 QQ 号（已修复）

- 优先级：P1
- 状态：已修复并验证（2026-08-15）
- 类型：事件路由正确性、群聊刷屏
- 位置：`internal/access/qq/qq.go`、`main.go`

### 修复结果

- QQ 适配器构造时要求非空、规范且为正整数的机器人 QQ 号，非法配置 fail-closed；
- 结构化 OneBot 消息只在 `at.data.qq` 精确匹配当前机器人时认定提及；
- raw CQ fallback 解析 `CQ:at` 参数并精确匹配目标机器人；
- `@其他用户`、`@全体成员` 不再触发机器人；
- 机器人自身消息和 `self_id` 不匹配的事件被忽略，防止回环和错连接事件进入 Echo；
- 新增规范化、非法配置和真实 WebSocket 负向测试。

### 验证证据

- `go test ./internal/access/qq`：通过；
- `go test -race ./internal/access/qq`：通过；
- `go test ./...`：通过；
- `go test -race ./...`：通过；
- `go vet ./...`：通过；
- Python `py_compile` 与 28 项单元测试：通过；
- Loader integration：通过；
- Go → Python e2e integration：通过。

### 问题

`extractText` 遇到任意 `at` 段就把 `mentioned` 设为 true，没有读取 `data.qq` 并与 `Config.BotQQID` 比较。`normalizeEvent` 也没有接收机器人 QQ 号。结果是群成员提及任何人都会触发机器人。

此外，配置了 QQ WebSocket 地址时并不要求 `AILUO_QQ_BOT_ID` 非空，错误配置会静默进入不可靠状态。

### 修复方向

- 规范化阶段显式传入 Bot QQ ID，只在 `at.data.qq` 精确匹配时认定提及；
- raw CQ fallback 也必须只匹配目标机器人，不能只判断存在任意 CQ at；
- 配置 QQ 接入时要求合法且非空的 Bot QQ ID；
- 机器人自己的消息必须防止回环处理；
- 群聊与私聊规则分别测试。

### 验收标准

- `@机器人` 被处理，`@其他用户` 和没有 `@` 的群消息被忽略；
- 缺失或非法 Bot QQ ID 时启动失败；
- array 消息和 raw CQ 两种格式行为一致；
- 机器人自身消息不会触发新的 Echo。

---

## PR2-008：`/chat/stream` 的 admission 生命周期没有覆盖 Echo 接纳

- 优先级：P1
- 类型：优雅关闭、持久队列状态
- 位置：`internal/access/web/chat.go`、`internal/access/web/server.go`
- 当前状态：已解决。持久 Run 调度已移入 `internal/kernel/echo.Scheduler`；Web、Ingress 和 QQ 的 admission 必须覆盖校验、`CreateIdempotent`、`Enqueue` 的完整接纳区间，主关闭流程先停止全部入口并等待接纳完成，再调用 `Scheduler.Shutdown`。

### 问题

`decodeChatRequest` 内部调用 `beginAdmission`，并在函数返回时立即执行 `admissionWG.Done`。随后 `chatStream` 才调用 `startChatRun` 完成消息入库和 Echo 创建。

关闭竞态如下：

1. chat 请求完成解码，提前退出 admission；
2. `Shutdown` 看到 admission 已清空，停止调度器；
3. chat 请求随后创建新的 queued Run 并调用 `Scheduler.Enqueue`；
4. 关闭流程此前收集的 pending/queued 集合可能不包含该 Run；
5. Run 留在 queued 状态，直到下次启动恢复。

### 修复方向

- admission 覆盖“校验、持久接纳、注册调度”完整临界区，token 在 `chatStream` 完成入队通知后释放；
- 持久 Run 调度器已移出 `web.Server`，由 `internal/kernel/echo.Scheduler` 统一管理；
- 关闭时 main 先停止接纳并等待 Web admission，再关闭 HTTP 与 Scheduler；Scheduler 以 SQLite 重新读取 queued Run 并执行有界取消。

### 验收标准

- 用 barrier 精确卡在 chat 解码完成和 Echo 创建之间，`Shutdown` 必须等待该接纳完成；
- 已接纳 Run 在关闭后只能处于约定终态或明确的可恢复状态；
- 关闭完成后不能再创建新的 queued Run；
- `/api/v2/echoes`、`/chat/stream`、QQ 和 ingress 的关闭语义一致。

---

## PR2-009：QQ Compose 使用不安全的部署默认值

- 优先级：P2
- 状态：用户确认忽略（2026-08-15）
- 类型：部署安全、供应链、凭据管理
- 位置：`docker/qq-onebot.compose.yaml`

### 决策

当前 PR 不修改 QQ Compose。已知默认端口绑定、固定 WebUI Token、镜像版本和容器权限风险由用户接受，不作为 PR #2 的合并阻塞项。

### 问题

当前 Compose 文件同时存在以下不安全默认值：

- 使用未固定 digest 的 `mlikiowa/napcat-docker:latest`；
- 容器以 UID/GID 0 运行；
- 提交固定 `WEBUI_TOKEN=ailuo-qq`；
- `3001` 和 `6099` 使用默认端口映射，绑定所有主机网卡；
- 登录态卷持久化，但没有给出权限、备份、清理和泄露后的撤销说明。

### 修复方向

- 固定审核过的镜像版本和 digest，并记录升级流程；
- 使用镜像支持的非 root UID/GID，显式设置卷权限；
- WebUI/OneBot Token 从批准的 secret 来源注入，不提供固定默认值；
- 默认只绑定 `127.0.0.1`，远程部署必须另行定义认证传输和网络策略；
- 增加登录态卷的保留、备份、删除和凭据撤销说明；
- 将该 Compose 明确标记为开发环境或补齐生产部署加固。

### 验收标准

- 仓库中不存在可直接使用的固定 Token；
- 默认部署不会在非 loopback 网卡暴露 WebUI 或 OneBot；
- 镜像不可被 `latest` 漂移；
- 容器与卷权限满足最小权限原则；
- 配置检查或部署测试覆盖关键安全默认值。

---

## 每项修复的共同完成条件

每个问题关闭时至少应满足：

- 修复根因，不只增加表面条件判断；
- 明确 App、身份、会话、平台和幂等作用域；
- 增加正向、拒绝、重复、并发、取消、超时和重启恢复测试；
- 更新相关 OpenAPI、架构文档、迁移和运维说明；
- 普通日志、JSON 日志和审计中不出现消息正文、回复正文、凭据、签名或原始平台载荷；
- 运行 `gofmt`、Go 单测、Python 测试、race、vet 和适用的集成测试；
- 最终报告区分实现结果、验证证据与仍未解决的生产阻塞项。
