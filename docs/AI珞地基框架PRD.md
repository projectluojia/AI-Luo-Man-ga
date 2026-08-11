# AI珞（爱珞）地基框架 PRD

> 状态：待实施产品需求基线
>
> 本文定义 AI珞后续地基模块的产品目标、职责边界、依赖关系与验收标准。本文按高内聚、低耦合的模块组织，不使用阶段划分，也不把逻辑模块等同于独立进程或微服务。

## 一、产品目标

建设一个可长期演进的多入口 AI 助手平台地基，使 AI珞能够：

- 接入 Web、QQ、CLI 及未来平台，而不污染内核业务；
- 统一管理身份、App、会话、消息和附件；
- 由 Go 掌握权限、状态、数据、调度和外部访问；
- 由 Python Agent 负责认知推理，但不持有系统权力；
- 提供有来源、有权限、有引用的知识库能力；
- 支持可靠副作用、扩展运行、数据接入和生产运维；
- 保持 Windows、Linux、macOS 普通开发链路可验证。

现有 Echo/Run、Registry、Dispatcher、Agent ToolCall、校巴 Service/Tool、SQLite、SSE、App 配置、幂等和可观测性作为已实现基线，本文不重复建设。

## 二、全局业务不变量

所有模块共同遵守：

1. Go 是唯一系统事实权威。
2. App 是数据、权限、会话、知识空间和 Agent 配置边界。
3. 外部平台只能连接 Go Access。
4. Python 不直接访问数据库、平台、文件系统或机构系统。
5. Agent 只看到本 Run 投影的 Capability。
6. 权限沿调用链只能收窄。
7. 持久业务状态不能只存在内存。
8. 写入和外部副作用必须具备幂等与确认治理。
9. 真实机构数据必须经过授权、来源校验和原子激活。
10. 日志、指标和 Trace 不记录消息、提示词、Tool 参数或知识正文。
11. 模块通过稳定 Port、Capability 或版本化协议协作，不直接读取彼此的表。
12. 逻辑模块不等于独立进程或微服务。

## 三、模块关系

```text
平台适配器
    │
    ▼
统一接入消息
    │
    ├── 身份与授权
    ├── 会话与消息
    └── 附件与制品
            │
            ▼
      上下文与记忆装配
            │
            ▼
       Echo / Run / Agent
            │
            ▼
   Dispatcher / Capability
       │        │        │
       ▼        ▼        ▼
    知识库   校园服务   确认与副作用
       │
       ▼
 数据接入与来源治理
       │
       ▼
 Go 托管存储与后台任务

扩展 Runtime Host、管理控制面、部署安全、可观测性
横向治理上述全部模块
```

## 四、模块总览

| 模块 | 核心职责 | 明确不负责 |
|---|---|---|
| 平台接入 | 平台协议与标准消息转换 | 身份判定、业务处理 |
| 身份与授权 | 用户、外部身份、App 成员与权限 | 平台协议、消息正文 |
| 会话与消息 | Session、Message、附件引用与历史 | 模型推理、知识检索 |
| 上下文与记忆 | 为 Run 生成受控上下文 | 保存平台原始事件 |
| Go 托管存储 | PostgreSQL、SQLite、Blob、Vector Port | 业务检索策略 |
| 知识库 | 文档、结构化事实、检索、引用 | 任意 SQL、最终回答 |
| 数据接入 | 来源授权、解析、版本、原子激活 | 用户问答 |
| 后台任务 | 持久任务、租约、重试、周期调度 | 通用工作流编排 |
| 确认与副作用 | 确认、幂等、未知结果治理 | 具体业务副作用实现 |
| 扩展运行时 | Host、隔离、资源限制、生命周期 | App 业务权限决策 |
| 管理控制面 | App、权限、知识空间和导入管理 | 绕过内核直接改库 |
| 部署与运维 | Secret、CI、发布、备份、SLO | 业务逻辑 |

## 五、平台接入模块

### 目标

任何新平台只需实现适配器，不修改 Agent、Echo/Run、知识库或业务 Service。

### 模块职责

- 接收平台原始事件；
- 验证签名、时间戳、重放标识和载荷大小；
- 转换为统一 `InboundMessage`；
- 将统一 `OutboundMessage` 转换为平台格式；
- 管理平台连接、限流、重连和发送回执；
- 处理平台消息 ID、回复关系和附件引用。

### 核心契约

```text
InboundMessage
├─ app_id
├─ platform
├─ platform_space_id
├─ platform_user_id
├─ platform_message_id
├─ platform_session_id
├─ message_type
├─ text
├─ attachments[]
├─ reply_to
├─ occurred_at
└─ idempotency_key
```

Access 只能提交标准消息，不直接创建私有用户、数据库记录或 Agent Run。

### 验收标准

- 同一标准消息可由 Web、CLI 和测试适配器产生；
- 平台重复投递不会重复创建消息和 Echo；
- 非法签名、过期事件、超限消息和未知字段被拒绝；
- 新增平台不修改 Agent 和知识库代码；
- 出站发送具备稳定回执、超时和错误分类。

## 六、身份与授权模块

### 目标

把外部平台身份、内部用户和 App 权限彻底分离。

### 模块职责

- 管理 Deployment 内部 `user_id`；
- 管理平台外部身份绑定；
- 管理 AppMembership、角色和权限；
- 将接入事件解析为受治理身份上下文；
- 为 Dispatcher 和知识库提供权限快照；
- 处理身份绑定冲突、解绑、禁用和撤权。

### 核心模型

```text
User
ExternalIdentity
AppMembership
Role
PermissionGrant
IdentityBindingRevision
```

唯一键至少包含：

```text
(platform, platform_space_id, platform_user_id)
(app_id, user_id)
```

### 依赖边界

- 依赖 Go 托管存储；
- 由管理控制面执行受保护的绑定和授权操作；
- 平台接入只向其提交外部身份，不读取身份表。

### 验收标准

- 外部平台 ID 不能直接充当内部 `user_id`；
- 同一外部身份不能被并发绑定给两个用户；
- 用户在 App A 的权限不能进入 App B；
- 撤权在下一次 Capability 边界立即生效；
- 身份不存在时返回明确状态，不自动创建匿名权威用户。

### 明确不做

校方统一身份边界确认前，不接入真实校园身份数据。

## 七、会话、消息与附件模块

### 目标

为多入口连续对话提供统一、持久、可审计的数据模型。

### 模块职责

- 管理 Session 生命周期和成员关系；
- 持久化标准 Message；
- 保存消息回复、编辑、撤回和平台映射；
- 管理附件元数据和 Blob 引用；
- 提供 App、Session、用户范围内的受限历史查询；
- 实施消息保留、删除和归档策略。

### 核心模型

```text
Session
├─ app_id
├─ session_id
├─ type: direct | group | system
├─ members[]
└─ platform_bindings[]

Message
├─ app_id
├─ session_id
├─ message_id
├─ sender_user_id
├─ type
├─ content_ref
├─ reply_to
├─ created_at
├─ edited_at
└─ deleted_at
```

平台来源不是 Session 类型。QQ 群、Web 群聊都可以映射成 `group`。

### 验收标准

- 消息写入和 Echo 创建具备原子或 Outbox 关系；
- 同一平台消息重复投递只产生一条标准消息；
- 所有历史查询同时约束 `app_id` 和 `session_id`；
- 删除消息后上下文装配不再读取正文；
- 附件正文不进入普通日志或审计载荷；
- 慢平台发送不阻塞 Agent Run。

## 八、上下文与记忆模块

### 目标

由 Go 决定模型本次能够看到什么，不让 Python 自行读取消息、知识或用户记忆。

### 模块职责

- 按 Run 构造上下文快照；
- 装配受限会话历史；
- 装配知识库证据；
- 装配经治理的用户或 App 记忆；
- 执行 Token、字符数、条目数和时间范围预算；
- 固化上下文来源版本，支持 Run 恢复和审计。

### 上下文来源

```text
系统提示配置修订
+ 当前标准消息
+ 受限会话历史
+ 授权知识证据
+ 授权长期记忆
+ 当前 Capability 投影
```

### 记忆规则

- 对话历史与长期记忆是不同对象；
- 模型可以建议记忆写入，不能直接保存；
- 长期记忆写入必须经过结构校验、权限、敏感性和确认策略；
- 用户可查询和删除自己的可见记忆；
- 不把模型推测自动升级为事实记忆。

### 验收标准

- Python 不持有会话数据库凭据；
- 相同配置修订和数据版本可生成确定性上下文快照；
- 超预算时按明确策略裁剪，并记录数量而非正文；
- 不同 App 的消息、知识和记忆不能混入同一 Run；
- 删除或撤权后，新 Run 不再投影相关内容。

## 九、Go 托管存储模块

### 目标

为核心状态、知识库、消息、附件和向量索引提供统一治理，但不建设万能数据库抽象。

### 模块职责

- 保留 SQLite adapter；
- 增加 Go 管理的 PostgreSQL adapter；
- 管理连接池、凭据、迁移、事务和错误分类；
- 提供 Blob Store Port；
- 提供 Knowledge Store 和核心 Repository；
- 管理 pgvector 和 PostgreSQL 全文索引；
- 提供备份、恢复、保留和容量指标。

### 数据边界

- Echo/Run 可以继续使用 SQLite；
- 知识库优先使用 PostgreSQL、`tsvector + GIN`、pgvector；
- Service 依赖领域 Port，不依赖 SQL client；
- Python 不连接 PostgreSQL；
- `space_id` 是知识空间，不替代 `app_id`。

### 迁移要求

- forward-only；
- 每个 migration 具有唯一版本；
- 不修改已应用 migration；
- 升级前支持备份；
- 覆盖前一版本升级和失败恢复；
- 必需扩展不可用时 readiness 失败。

### 验收标准

- 所有 App 数据 SQL 均包含 `app_id`；
- 参数化查询，不接受模型生成 SQL；
- 连接等待和查询接受 context 取消；
- 数据库错误映射为稳定内部分类；
- PostgreSQL 不可用时知识模块明确不可就绪；
- SQLite 与 PostgreSQL adapter 不改变上层业务契约。

## 十、知识库模块

### 目标

向 Agent 提供有权限、有来源、有版本、有引用的知识证据。

### 模块职责

- 管理 Knowledge Space；
- 管理文档、Chunk、实体、关系和结构化事实；
- 提供混合检索、过滤和重排；
- 返回结构化证据和引用；
- 评估检索质量、引用完整性和无答案行为；
- 管理知识修订的激活、失效和回滚。

### 首批 Capability

#### `knowledge.search`

用于非结构化文档检索。

输入：

```text
query
space_ids[]
filters
limit
```

输出：

```text
evidence[]
├─ document_id
├─ chunk_id
├─ excerpt
├─ source_revision
├─ authority
├─ valid_at
└─ citation
```

#### `knowledge.query_facts`

用于封闭 Schema 的结构化事实查询。输入只能是受支持的语义计划：

```text
fact_type
subject
filters
effective_at
limit
```

禁止模型提交 SQL。

### 回答权归属

- Knowledge Service 返回证据，不生成最终用户回答；
- 主 Agent 基于证据组织最终回复；
- 没有足够证据时返回 `insufficient_evidence`；
- 引用失效、过期、无权限时不返回正文。

### 检索基线

```text
PostgreSQL FTS
+ pgvector
+ 权限前置过滤
+ 结果融合
+ 可选 reranking
```

ParadeDB 只有在质量基准证明必要时再加入。

### 验收标准

- 所有表和索引按 `app_id + space_id` 隔离；
- 权限过滤发生在检索前；
- 每条证据可追踪到文档、Chunk 和来源修订；
- 删除、撤权或切换修订后旧内容不再被检索；
- 同一活动修订原子可见；
- 引用覆盖率、召回率、无答案准确率具有固定测试集；
- Agent 不得把低权威或过期证据描述为当前事实。

## 十一、数据接入与来源治理模块

### 目标

让知识文档、结构化事实和校巴数据通过统一治理进入存储。

### 模块职责

- 管理授权 Source；
- 保存原始制品或制品摘要；
- 解析和标准化数据；
- 验证来源、引用、大小、完整性和有效期；
- 创建不可变 Source Revision；
- 原子激活新修订；
- 失败时保留上一完整修订；
- 记录导入审计、指标和失败分类。

### 标准流程

```text
授权来源
→ 获取制品
→ 校验哈希和大小
→ 保存 Artifact
→ 解析至 staging
→ 领域校验
→ 建索引
→ 原子激活
→ 发布 revision event
```

### 接入器边界

接入器只负责：

- 从授权来源获取数据；
- 转换为标准导入包。

接入器不负责：

- 直接修改活动表；
- 自己创建数据库 Schema；
- 静默关闭知识库；
- 绕过 Go Storage Port。

### 验收标准

- 同一来源修订重复导入不会重复创建活动版本；
- 中途崩溃不会暴露半份数据；
- 无授权来源无法进入生产激活流程；
- 不完整、过期、非权威数据显式 fail-closed；
- 导入任务可取消、可恢复、可观测；
- 真实智慧珞珈接入必须具备书面授权。

## 十二、后台任务与调度模块

### 目标

支撑数据同步、索引构建、保留清理和未来提醒，不再为每个模块创建私有 goroutine 调度器。

### 模块职责

- 持久 Task 状态机；
- 定时、延迟和周期任务；
- Lease、续租、重试和死亡任务恢复；
- 有界并发和 App 容量限制；
- Outbox 事件投递；
- 任务取消和部署关闭；
- 封闭任务类型与参数 Schema。

### 与 Run 的关系

- Agent Run 继续使用现有 Run 调度；
- 系统 Task 使用独立状态模型；
- Task 可以通过内核调用 Capability；
- Task 不能伪装成用户 Echo；
- 需要 Agent 推理时，由 Go 创建正式 Run。

### 验收标准

- 进程崩溃后任务可确定性恢复；
- 重复领取不会重复产生副作用；
- 不安全副作用默认不自动重试；
- 每个任务有 deadline、attempt、幂等键和错误分类；
- 某 App 的任务拥塞不能占满整个 Deployment；
- 调度器不执行模型提供的任意代码或任意任务名。

## 十三、确认与副作用治理模块

### 目标

统一管理发送消息、写入记忆、修改数据和调用外部系统等风险动作。

### 模块职责

- 持久化 Confirmation；
- 绑定 App、Run、Call、Capability 和参数摘要；
- 管理等待、批准、拒绝、过期和撤销状态；
- 与幂等记录和副作用执行衔接；
- 管理未知执行结果；
- 提供用户可理解的确认内容。

### 核心模型

```text
Confirmation
├─ app_id
├─ confirmation_id
├─ echo_id
├─ run_id
├─ call_id
├─ capability_id
├─ argument_digest
├─ status
├─ expires_at
├─ confirmed_by
└─ decided_at
```

### 验收标准

- 参数改变后旧确认不可复用；
- 确认不能跨 App、用户或 Capability 使用；
- 重启后待确认状态仍存在；
- 重复批准不会重复执行副作用；
- 超时、撤权和 Run 取消会使确认失效；
- 未知执行结果不会自动重试或伪报失败。

## 十四、扩展 Runtime Host 模块

### 目标

把现有 Loader 和 Runtime Host 协议接入真实扩展执行环境。

### 模块职责

- 实现真实 Host Backend；
- 加载已安装且锁定的 Runtime；
- 执行 Capability、Service 或 Tool 调用；
- 管理生命周期、容量和故障恢复；
- 限制 CPU、内存、进程、文件系统和网络访问；
- 阻断环境变量、凭据和宿主目录泄漏；
- 支持 hosted 和 isolated 模式。

### 平台边界

- Unix 使用属主、权限、Unix Socket 和进程组隔离；
- Windows 未具备同等安全实现前显式拒绝对应扩展模式；
- 普通 Go/Python Agent 开发链路继续跨平台；
- 不通过弱化权限验证换取表面兼容。

### 验收标准

- 扩展只能加载已安装、已锁定、Digest 匹配的代码；
- 扩展无法读取内核凭据和业务数据库；
- 崩溃、卡死、超限和协议违例具有稳定错误；
- shared host 内不同 App 调用上下文不混淆；
- 关闭后无子进程、Socket 和临时文件泄漏；
- Host Backend 失败不能破坏 Go 权威状态。

## 十五、管理控制面模块

### 目标

提供唯一受控入口管理 App、权限、知识空间、数据导入和运行配置。

### 模块职责

- App 启停和配置修订；
- Capability 与权限管理；
- 身份绑定和 AppMembership 管理；
- Knowledge Space 管理；
- 数据来源、导入任务和修订激活管理；
- Confirmation 查询和决策；
- Runtime 安装状态和健康查询；
- 审计查询。

### 产品形态

先提供：

```text
受保护的管理 API
+ 运维 CLI
```

不要求立即建设复杂管理前端。

### 验收标准

- 所有管理请求具有操作者身份和权限；
- 所有写入使用 CAS 或明确版本；
- 所有管理操作产生脱敏审计；
- 禁止直接修改数据库作为正常管理方式；
- 配置回滚生成新修订，不删除历史；
- 管理员不能越过 Deployment/App 边界。

## 十六、部署安全与 Secret 模块

### 目标

让生产部署有明确的凭据、网络、进程和配置边界。

### 模块职责

- Secret Provider Port；
- 文件 Secret、Windows 原生 Secret 或外部 Secret Manager 适配；
- Secret 轮换和吊销；
- 非 loopback gRPC 的双向认证传输；
- 生产启动配置严格校验；
- 最小权限数据库账户；
- 开发、测试、生产配置隔离；
- 依赖和构建产物完整性校验。

### 验收标准

- Secret 不进入环境输出、日志或公共错误；
- 生产环境不接受不受治理的原始密钥；
- Secret 更新不要求修改业务代码；
- 非本机明文 gRPC 始终拒绝；
- demo 数据和开发配置不能在生产误启用；
- Windows 只有在完成原生 Secret 校验后才开放生产密钥路径。

## 十七、可观测性与生产运维模块

### 目标

让每个模块能够被监控、诊断、恢复和容量规划。

### 模块职责

- 统一日志、指标、Trace 和审计；
- 定义模块级 SLI/SLO；
- 队列、数据库、Provider、知识检索和 Host 告警；
- 备份、恢复、容量和保留演练；
- Runbook 和故障处置流程；
- Token、模型调用和知识检索成本统计。

### 重点指标

```text
Access 接收/发送成功率
消息去重冲突
Run 排队时间与成功率
Provider 首 Token 与总耗时
Capability/Tool 延迟
数据库连接池与查询延迟
知识检索召回、无答案和引用覆盖率
导入积压、失败和修订新鲜度
Confirmation 等待与过期数
Runtime 崩溃和资源超限数
```

### 验收标准

- 指标标签来自低基数闭集；
- 日志不含正文、凭据或 Tool 载荷；
- 一次请求可跨 Access、Run、知识库和存储重建 Trace；
- 每种依赖不可用都有 readiness 和告警表现；
- 备份恢复进行真实演练，不只检查文件存在。

## 十八、CI、发布与兼容性模块

### 目标

保证每次变更都经过跨平台和生产契约验证。

### 模块职责

- Windows、Linux、macOS 核心测试；
- Linux race、vet 和跨进程集成测试；
- Python `uv.lock` 一致性检查；
- Protobuf 生成构件一致性检查；
- OpenAPI 和 Capability Schema 兼容性检查；
- migration 升级测试；
- 依赖漏洞和许可证检查；
- 可复现构建、版本和发布说明。

### 验收标准

- 必需 CI job 失败时禁止合并；
- 锁文件和依赖声明不一致时失败；
- 生成文件与 Proto 不一致时失败；
- 破坏性协议变更没有升版时失败；
- migration 不能只测试空数据库；
- 发布产物能追踪到提交、依赖锁和构建环境；
- CI 不使用真实机构数据或生产 Secret。

## 十九、模块依赖规则

允许的依赖方向：

```text
Access
→ Identity / Session
→ Echo / Run
→ Dispatcher
→ Service
→ Tool
→ Storage Port
→ Storage Adapter
```

知识链路：

```text
Agent
→ knowledge.search
→ Knowledge Service
→ KnowledgeStore Port
→ PostgreSQL / pgvector
```

导入链路：

```text
Task Scheduler
→ Authorized Source Adapter
→ Ingestion Service
→ KnowledgeStore / Domain Store
→ Atomic Revision Activation
```

禁止的依赖：

- Access 直接访问 Knowledge 表；
- Agent 直接读取 Message 或 Knowledge 数据库；
- Knowledge Service 直接调用平台 API；
- Source Adapter 直接切换活动修订；
- Tool 绕过 Dispatcher 调用另一个 Tool；
- 管理 API 直接拼 SQL；
- Runtime Host 自行判断 App 权限；
- 模块共享可变内存作为权威状态。

## 二十、整体验收场景

地基完成后，应通过以下端到端场景：

1. 同一用户从两个平台进入同一 App，身份被正确归一。
2. 平台重复消息只创建一条标准消息和一个 Echo。
3. Session 历史按预算投影给 Agent。
4. Agent 调用 `knowledge.search`，获得当前授权修订的证据。
5. Agent 返回带引用回答，引用可追踪到文档和来源。
6. 无权限、过期或证据不足时明确拒绝回答事实。
7. 数据导入中途失败，用户仍读取上一完整知识修订。
8. 外部副作用等待持久确认，重启后可继续。
9. 扩展进程崩溃不破坏 Echo、Run 或数据库状态。
10. App 撤权后已有 Agent 不能继续调用被撤销 Capability。
11. Windows、Linux、macOS 核心测试通过，Unix 隔离测试在 Linux 通过。
12. 日志、审计、指标和 Trace 中不存在消息正文、知识正文和密钥。

## 二十一、明确暂不建设

在没有真实产品或容量证据前，不建设：

- 通用微服务平台；
- 通用工作流引擎；
- 独立向量数据库；
- 插件市场；
- 多级 Subagent；
- 分布式调度；
- ParadeDB；
- 每个模块独立进程、端口或数据库。

出现明确部署、规模、授权或质量数据证明现有方案不足时，再对相应能力单独立项。
