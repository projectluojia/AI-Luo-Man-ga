# AI珞 V3 总体设计

> 状态：当前统一模型。详细目标与实施状态见 `docs/目标架构.md`，当前代码证据见
> `docs/系统架构说明.md` 和 `docs/仓库状态与路线图.md`。

## 一、唯一系统模型

```text
Deployment
└─ App
   └─ Package
      └─ Component Runtime
         └─ Capability
```

- `Package` 是分发、版本、依赖、安装和升级单位。
- `Component` 是 Package 内的运行单元，包含 mode、role、工件、进程规格和导出声明。
- `Capability` 是唯一的公开操作契约，也是授权、输入 Schema、幂等、确认和审计边界。
- `Provider` Component 响应 Capability 调用；`Executor` Component 驱动一次受治理的 Run。
- `Run`、权限、App 数据、事件和审计由 Go Core 持久化，扩展进程不能成为事实来源。

公共 Package 模型中不再出现额外的能力分类。内部普通函数、语言库和 DDD application
service 可以存在，但不进入 Registry、Dispatcher、manifest/lock 或跨进程协议。

## 二、Core 边界

Go Core 是唯一核心，负责：

- 外部入口、身份、App 策略和会话；
- Echo/Run 持久状态机、队列、租约、取消、恢复和 child Run；
- PackageSource、Loader、Capability Registry 和统一 Dispatcher；
- 存储、幂等、确认、审计和可观测性。

Executor 只接收 Go 投影的 Capability 集合，发送 CapabilityCall、回复片段和终态。
它不能访问 Core 数据库、权限实现或安装目录，也不能在进程内私建未跟踪的子执行。

## 三、运行协议

统一的是 Component 身份、清单、Loader 生命周期、治理上下文和错误边界；协议按交互
语义保持最小化：

- Provider Runtime：`Describe / Start / Health / Invoke / Stop`，一次调用请求对应一次结果。
- Executor Runtime：`Health / Run(stream)`，承载 StartRun、CapabilityCall、结果、回复、用量、取消和终态。

强行合并会制造带大量可选字段的万能协议，并削弱两种角色的状态机约束。两种协议均
必须版本化、严格解码、限制消息大小、传播 deadline/cancel，并拒绝未知字段和身份错配。

## 四、依赖图

依赖关系保持两种明确语义，但都使用统一 Package/Capability 契约：

1. Package 依赖：用于构建、安装、来源、版本求解和 lock，必须是 DAG。
2. Runtime 组合：由 Core 在任务图或 Executor 会话中调用 Capability，经过 Dispatcher、
   权限收窄和审计；Component 不保存其他组件地址，也不做私有直连。

不能用运行时调用替代安装依赖，也不能把编译期库强行注册成运行时节点。纯代码库由对应
语言的包管理器作为构建依赖；进入 Ailuo 安装根的 Package 必须包含可装载 Component。
需要独立权限、数据或生命周期的实现才声明 Provider Component。Executor 的多 Agent 能力统一由 Core 的 `run.create_child` Capability 提供，
所有 Executor 使用同一套规则。

## 五、安全与耐久性

- 安装不等于授权；App 对 Capability 默认拒绝，启用必须来自受信配置。
- Capability 的权限只能收窄，内部调用不能提权。
- write/external 调用需要幂等键；未知外部结果不得自动重做。
- child Run 是 Go 创建的持久记录，拥有父子关系、独立预算、租约、取消和结果路由。
- Storage/事件/审计按 `app_id` 隔离；秘密、正文、提示词和原始上游响应不得进入日志。
- Schema、manifest、lock、HTTP/SSE 和 Protobuf 都是版本化公共契约；破坏性变更直接升版，
  不保留旧解析 fallback。

## 六、实现证据

当前重构完成前不得宣称整体可合并。至少需要：

- Core、`contracts`、`package-manager` 和 Agent 测试通过；
- Protobuf 生成物无漂移；
- 安装、发现、注册、升级和失败回滚有负向测试；
- 文档、OpenAPI、清单示例只描述 Package/Component/Provider/Executor/Capability；
- 完整门禁结果区分已运行项目和未运行项目。
