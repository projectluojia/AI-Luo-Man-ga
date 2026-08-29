# AI珞（爱珞） V3

当前仓库实现了“校园综合服务 App”的窄范围、可长期保留的校巴纵向切片。Go 是唯一内核；Python AI Agent 只负责真实模型推理和原生 ToolCall 闭环。2026-07-26 审计列出的 P0 已按仓库证据关闭，但完整平台仍有 `AGENTS.md` 所列 P1 阻断项，不能据此描述为整体生产就绪。

## 品牌与升级契约

项目统一使用产品名 **AI珞（爱珞）** 和技术命名空间 `ailuo`。部署环境变量统一使用 `AILUO_*`，Prometheus 指标统一使用 `ailuo_*`，默认数据库文件为 `var/ailuo.db`。Agent gRPC 与 Runtime Host gRPC 均使用 `ailuo.*.v1` 包名并协商协议版本 `2.0`；扩展安装目录使用 `ailuo.install.v2`。这是一次有意的破坏性品牌迁移：部署配置、监控查询、Agent/Runtime Host 构建产物和扩展安装清单必须原子升级，旧版本不会被误判为兼容。

## 已实现链路

```text
Web Access API
  → Echo / Run（Go）
  → Python AI Agent（gRPC 双向流）
  → OpenAI-compatible 模型原生 ToolCall
  → Capability 鉴权与路由（Go）
  → campus / weather / prompt Service
  → 校巴或天气 Tool
  → Go 托管统一数据库（含天气缓存）
  → SSE 流式回复
```

生产 Agent 不包含关键词、正则或固定业务流程。没有模型配置时业务内核与 Agent 不启动，也不会降级为规则机器人；本机配置控制台仍保持可用，等待完成配置。测试使用可控 ModelProvider 验证调用链，但不进入生产运行路径。

## 本地运行

要求：Go 1.26、[uv](https://docs.astral.sh/uv/)、Python 3.11–3.14、Protobuf 编译器，以及支持原生 ToolCall 的 OpenAI-compatible 模型。Python 版本与依赖由 `agent/pyproject.toml` 和 `agent/uv.lock` 统一管理，不使用系统 `pip` 直接安装。

```bash
make setup-agent
make run
```

Go 主进程会立即在 `http://127.0.0.1:9178` 提供本机配置控制台。首次运行只需在页面填写模型名称、API Base URL、API Key、Provider 超时/重试/限流/并发参数，以及可选的 QQ 接入参数；保存后主进程启动业务内核与 Python Agent。再次保存会有界关闭旧内核并按新修订重新启动，9178 控制台在整个过程中保持可用。普通 Web Access 仍监听 `http://127.0.0.1:8080`。

非秘密配置写入 `var/ailuo-settings.json`；模型 API Key 与 QQ WebSocket Token 分别写入仅属主可访问的 `var/secrets/model-api-key` 和 `var/secrets/qq-ws-token`，GET API、页面、日志和普通配置文件都不返回秘密正文。既有环境变量配置会在首次启动时迁移进控制面，之后以受管配置修订为准，不再要求手工维护 `.env`。

NapCat 保持独立运行和独立 WebUI，负责 QQ 登录及 OneBot 服务；AI珞不接管 NapCat 配置。QQ 白名单属于 `internal/access/qq` 的入口准入，不是内核 Capability 权限。未列入白名单的消息会在进入 Hub、Message、Echo 之前静默丢弃；列入白名单的 QQ 用户由 QQ Access 自动建立内部身份，不需要手工执行 `identity-bind`。9178 还可配置精确快速回复和戳一戳随机文案：白名单群内的快速回复无需 @机器人，命中后不调用 Agent、不写会话且不 @发送人；戳一戳文本同样不 @，清空文案可关闭文本回复。9178 管理入口只绑定并接受本机同源请求。

Provider 默认对单次请求设置 30 秒超时、最多两次仅限首个流数据前的可重试退避、每分钟 60 次请求和 4 路并发上限；这些参数均可在 9178 控制台按部署容量收窄。`/readyz` 会实际探测配置模型，不只检查 Python 进程存在。

Go Run 调度使用 SQLite 持久队列和固定 4 个 worker，默认每个 App 最多容纳 128 个 queued/running Run。超过容量时创建接口返回 HTTP 429 `queue_full` 和 `Retry-After: 1`；相同幂等请求仍可重放。默认最多 3 个 Run attempt，只有稳定错误标记为可重试且当前 attempt 未请求 write/external Capability 时才按持久 `available_at` 延迟重试；活动 Run 的 lease 会周期续期。当前合同面向单 Deployment，不宣称多节点 SQLite 调度。

App 的启停状态、模型、系统提示、时区、运行预算、Capability 集合和权限上界保存在 SQLite 不可变配置修订中，当前 head 以 generation CAS 更新。新 Run 固化创建时的配置修订用于确定性恢复；停用和 Capability/权限撤销在每次调用边界读取当前策略并立即生效。系统提示不会进入普通日志或公共 API。当前没有公开配置管理 API；在多入口管理员身份边界实现前，只允许受信 Go 控制面使用该 CAS 合同，不能直接修改数据库。

`AILUO_LOAD_DEMO_DATA=true` 会把明确标记为非权威的演示快照写入统一数据库。生产环境设置该值会拒绝启动；开发环境查询该快照也会得到 `data_non_authoritative`，不会把演示线路或班次作为当前事实返回。真实智慧珞珈数据必须通过校方授权适配器写入同一个 Storage Port，业务层不读取来源接口或文件。

## API

- `POST /api/v2/echoes`：使用必填 `Idempotency-Key` 原子创建 Echo 和排队 Run；相同请求可安全重放。
- `GET /api/v1/echoes/{echo_id}/events`：SSE 事件流，支持 `Last-Event-ID` 重放。
- `GET /api/v1/echoes/{echo_id}`：查询状态和完整事件。
- `DELETE /api/v1/echoes/{echo_id}`：取消运行。
- `GET /api/v1/capabilities`：查询当前 App 可见 Capability。
- `GET /healthz`：仅报告 Go 进程存活，不探测依赖。
- `GET /readyz`：检查是否仍接收工作、SQLite 与实际模型 Provider 就绪状态。
- `GET /metrics`：Prometheus 文本指标；不包含 App、Echo、Run、调用标识或业务正文。

完整契约见 `docs/openapi.yaml`；执行者跨进程契约见 `proto/executor.proto`，扩展 Runtime Host 契约见 `proto/runtime_host.proto`。生成后的 Go/Python 文件是提交构件，不得手工修改。

## 验证

```bash
make test
make test-race
make vet
make test-integration
```

普通 Go/Python Agent 开发链路支持 Windows、Linux 和 macOS，默认 Python 路径会适配各平台的 uv 虚拟环境。安装目录属主验证、Unix Socket 和进程组隔离属于 Unix Runtime Host 安全边界，当前只在 Linux/macOS 启用；非 Unix 平台会显式拒绝这些能力。CI 在三平台运行核心测试，并在 Linux 运行 race、vet 和全部跨进程集成测试。

集成测试会启动真实 Python Agent 进程，通过 OpenAI SDK 的流式协议发起原生 ToolCall，并完成 Go Capability、Service、Tool、数据库和最终回复的闭环。`go test -tags=integration ./internal/kernel/loader` 另行启动真实 isolated Runtime Host 子进程，验证 Unix gRPC、协议身份、优雅退出和进程组强制清理；该测试需要允许创建本机 Unix Socket。

## 日志与可观测性

Go 内核和 Python Agent 统一输出中文结构化日志。控制台格式优先便于本地阅读，生产环境建议设置 `AILUO_LOG_FORMAT=json`，字段名保持英文稳定，便于检索、告警和跨语言关联。

- `AILUO_ENVIRONMENT`：部署环境名称，默认 `development`。
- `AILUO_LOG_LEVEL`：`debug`、`info`、`warn` 或 `error`，也接受对应中文值。
- `AILUO_LOG_FORMAT`：`console` 或 `json`。
- `AILUO_LOG_SOURCE`：必须保持 `false`；源码绝对路径禁止进入日志，启用时拒绝启动。
- `AILUO_LOG_MAX_VALUE_LENGTH`：单个字符串字段最大字符数，默认 `4096`。
- `AILUO_WEATHER_TIMEOUT_SECONDS`：天气 HTTP 超时，默认 `8`。
- `AILUO_WEATHER_MAX_RETRIES`：天气可重试失败的最大重试次数，默认 `2`。
- `AILUO_WEATHER_REQUESTS_PER_MINUTE`：每个天气数据源的每分钟请求上限，默认 `30`。
- `AILUO_ACCUWEATHER_API_KEY` / `AILUO_ACCUWEATHER_API_KEY_FILE`：AccuWeather 密钥；未配置时该源不可用。

Web 请求会验证并继承 W3C `traceparent`（兼容合法的 32 位十六进制 `X-Trace-ID`），为 HTTP 与 Agent Run 创建 Span，再通过 Protobuf 把追踪上下文传给 Python。日志只记录标识、数量、长度、状态、稳定错误类别和耗时，不记录原始错误正文、用户或模型消息、Tool 参数、请求体、密钥或凭据。完整规范见 `docs/日志与可观测性设计.md`。

## 运维

同 Deployment 的 Go/Python 与扩展 Runtime Host 边界默认使用 loopback 或绝对 Unix 地址的明文 gRPC；代码拒绝任何非本机的不安全地址。当前没有远程 gRPC TLS 配置，因此需要跨主机部署时必须先实现并评审双向认证传输，不能绕过该拒绝策略。

服务收到 `SIGINT`/`SIGTERM` 后立即停止接收新 Echo，先关闭 HTTP 接入，再持久化取消并有界等待活动 Run，最后优雅停止 Python 子进程；超时后返回明确关闭失败。SQLite 备份、校验、恢复命令及保留/演练流程见 `docs/运维与灾难恢复.md`。

## 数据边界

- 智慧珞珈是校巴数据的唯一权威来源。
- 当前不实现用户登录或用户态业务数据。
- Service/Tool 不持有数据库凭据，只依赖 Go 注入的 Storage Port。
- 演示数据带 `authoritative=false` 和独立 `source_revision`，不得被描述为真实班次。
- 成功校巴结果携带权威修订、完整性、导入时间和有效截止时间；缺失、不完整、非权威或过期快照不返回业务行。
- 真实数据接入前必须完成 `docs/数据需求与授权清单.md` 中的校方审批项。
- 天气直连 Open-Meteo / 小米天气 / AccuWeather。未指定地点时默认武汉大学珞珈山。AccuWeather 使用 `AILUO_ACCUWEATHER_API_KEY` 或 `AILUO_ACCUWEATHER_API_KEY_FILE`；未配置时该源不可用，不阻止其他源。查询结果带来源、修订和有效期，过期缓存不会被当作当前事实。
