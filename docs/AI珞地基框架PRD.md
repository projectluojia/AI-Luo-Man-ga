# AI珞 V3 地基框架 PRD

> 状态：与当前统一 Package/Component/Capability 架构一致的产品基线。

## 一、产品目标

建设一个多入口、可长期维护的 AI 助手平台：Go Core 掌握身份、App、权限、持久状态、
调度、外部访问和审计；Executor 只负责一次认知 Run；Provider Package 通过受治理
Capability 提供业务功能。

## 二、全局不变量

1. Go 是唯一系统事实权威。
2. App 是数据、权限、会话和执行配置边界。
3. 外部平台只能连接 Go Access。
4. Executor/Provider 不直接访问 Core 数据库、凭据或用户通道。
5. Executor 只能看到本 Run 投影的 Capability。
6. Capability 调用的权限沿调用链只能收窄。
7. 持久状态不能只存在内存；崩溃、取消、超时和重启都必须有确定语义。
8. write/external Capability 需要幂等与确认治理；未知结果不能自动重做。
9. 真实机构数据必须经过授权、来源校验、完整性校验和原子激活。
10. 日志、指标和审计不记录消息、提示词、CapabilityCall 参数、结果或凭据。

## 三、公共模型

```text
Deployment → App → Package → Component Runtime → Capability
```

- Package：分发、版本、来源、依赖、安装和升级单位。
- Component：运行单元，声明 mode、role、工件、进程规格和对外 exports。
- Capability：唯一公开操作契约、授权边界、Schema、幂等键、确认和审计边界。
- Provider：响应 Capability 的运行协议。
- Executor：驱动持久 Run 的运行协议。

内部普通函数、语言库和 application service 可以存在，但不进入公共 Package 模型、
Registry 或模型可见调用面。

## 四、核心模块

| 模块 | 职责 |
|---|---|
| Access | 平台认证、标准消息转换、SSE/出站 |
| Identity/App | 用户、成员、权限和 App 配置 |
| Echo/Run | 持久执行、队列、租约、取消、恢复、child Run |
| Registry/Dispatcher | Capability 注册、Schema、权限、确认、幂等和审计 |
| Loader | Package Component 的发现、宿主选择、加载、健康、升级和停止 |
| Storage | Go 托管 SQLite/Blob/未来数据库，按 App 隔离 |
| Provider Package | 业务规则和领域 Capability |
| Executor Package | 模型/规划/工作流等认知执行，实现统一 Executor 协议 |

## 五、多 Agent

多 Agent 不是 Executor 的私有功能。root Run 调用 Core 的 `run.create_child` Capability，
Core 持久化 child，计算权限交集和独立预算，并负责调度、取消、结果路由和审计。child
不能继续派生。任何 Executor 都通过同一协议获得相同能力，不需要编写场景专用接线。

## 六、验收

- Package 安装、lock、依赖闭包和工件完整性可验证；
- Capability 的正向、未启用、越权、Schema、幂等、确认、App 隔离和取消测试到位；
- Provider Runtime 与 Executor Runtime 都有版本化协议和严格负向测试；
- SQLite 迁移只前向追加，备份/恢复保留历史完整版本；
- Windows、Linux、macOS 普通开发路径可验证，强安全边界不支持时 fail closed；
- 所有未运行的 CI 门禁和真实授权项在交付报告中明确列出。
