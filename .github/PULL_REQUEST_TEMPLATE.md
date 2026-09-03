<!-- PR 的人工描述统一使用中文；命令、路径、API/协议名称、Conventional Commits 关键字和 BREAKING CHANGE 可保留原文。堆叠 PR 只描述本层增量，不复制父层或子层内容。 -->

## 变更内容

<!-- 描述本次 PR 做了什么、为什么。标题须符合 Conventional Commits（如 feat:/fix:/refactor:/chore:）；破坏性变更使用 ! 并在正文 footer 写明 BREAKING CHANGE:。 -->

## 堆叠关系

- 直接 base：
- 本 PR 增量：

## 影响范围

- [ ] 涉及公共契约（HTTP API / SSE / Protobuf / Capability Schema / Tool 规范 / 持久化事件形状）：已在设计文档与 OpenAPI 中更新，破坏性变更附兼容/迁移方案
- [ ] 涉及持久化或 Schema：已补充当前契约与升级/恢复验证
- [ ] 涉及新敏感字段：已包含 redaction 负向测试（控制台与 JSON 输出均不出现）
- [ ] 涉及 App 隔离 / 权限 / 信任边界：读写路径均有 App 作用域测试
- [ ] 涉及工程配置（CI / 规则集 / 依赖机器人 / 模板）：已同步 AGENTS.md 与相关文档

## 验证证据

- [ ] `gofmt` 无差异
- [ ] `go test ./...` 通过
- [ ] `go test -race ./...` 通过
- [ ] `go vet ./...` 通过
- [ ] Executor 包已通过 Python compileall、Ruff 和单元测试
- [ ] Campus 包已通过 WASI vet 与交叉编译
- [ ] package pack/install/list/start smoke 已通过
- [ ] workflow lint 已通过
- [ ] Runtime Host 集成测试通过（`go test -tags=integration ./internal/kernel/loader -v -timeout=30s`）
- [ ] executor 包 e2e 集成测试通过（`AILUO_EXECUTOR_PACKAGE_DIR="$PWD/packages/agent" go test -tags=integration ./e2e -v -timeout=60s`）

## 安全与治理

- [ ] 信任边界、App 隔离、取消传播、并发与失败行为已考虑
- [ ] 状态变更原子且可恢复（如适用），重复投递/重放不产生重复副作用
- [ ] 外部错误稳定、不泄露内部细节；可观测性不泄露受保护内容
- [ ] 背景清理有界，无无限期后台工作

## 备注

<!-- 遗留问题、需人工复核的决策、后续 PR 预告等。 -->
