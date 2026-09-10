# 贡献指南

欢迎贡献 AI珞（爱珞）V3。本文件是分支、提交、PR、审查和贡献留存的唯一流程入口；架构、安全、数据授权及适用测试契约继续由 [AGENTS.md](AGENTS.md) 和设计文档维护。流程按三位活跃开发者的容量安排，成员记录见[版本与贡献索引](docs/版本与贡献索引.md)。

## 配置状态与待落地项

2026-09-10 核验的远端规则是：`dev` 人工批准数为 0，`main` 为 1；[`dev` 规则集](https://github.com/projectluojia/AI-Luo-Man-ga/rules/22379240)中的 merge queue 已从 `SQUASH` 改为 `MERGE`，其他规则字段不变，`main` 保护未修改。

本指南的历史保留方案要求新开发 PR、`dev → main` 和启用的 merge queue 均采用 merge commit。恢复链不得使用 squash 或 rebase；规则配置已更新不代表历史已进入主线，正常合并后仍须逐项验证实际 `main` 的原始 SHA 可达性，不绕过现有门禁。

[`pull_request_target` 工作流](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request_target)从默认分支读取。标题兼容修复的默认分支入口是 [#126](https://github.com/projectluojia/AI-Luo-Man-ga/pull/126)；该草案须经过审查合入 `main`，并由新 PR 事件触发后核实生效，不能用工作分支内存在修复或旧检查成功代替。

## 快速开始

要求：Go（版本见 `go.mod`）、[uv](https://docs.astral.sh/uv/)、Python 3.11–3.14。

```bash
make test-agent    # 按 packages/agent/runtime/uv.lock 运行 Executor 包测试
make test-campus   # 校巴 guest 的 WASI vet 与交叉编译
make test          # Core 与 contracts module 测试
make test-contracts # 公共 contracts module 测试
make test-package-manager # Package Manager module 测试
make test-race     # Go race 检测
make vet           # go vet
make test-integration  # Runtime Host 跨进程集成测试（Unix 平台）
make test-e2e      # Executor e2e（源包，Unix 平台）
```

## 分支与提交

- 分支命名使用前缀：`feat/`、`fix/`、`chore/`、`docs/`、`refactor/`。
- 提交信息和 PR 标题遵循 Conventional Commits：`<type>[optional scope][!]: <描述>`，类型为 `feat`、`fix`、`docs`、`style`、`refactor`、`perf`、`test`、`build`、`ci`、`chore`、`revert`。破坏性变更在标题使用 `!`，并在 footer 写明 `BREAKING CHANGE: <说明>`。标题检查见 [pr-title.yml](.github/workflows/pr-title.yml)。
- 有至少两个真实父提交的标准 Git 自动合并消息可以原样保留，无需为标题格式重写原 SHA；普通提交或带破坏性声明的提交仍按上述格式校验，PR 标题不适用该例外。
- 一个 commit 是一个自洽的逻辑单元：可独立构建、独立回滚（例如迁移 + 存取代码 + 测试同处一个 commit）。
- 独立工作从最新 `dev` 开始；更新个人分支时保留待保全的原始 SHA。不得未经约定 force-push、rebase 或 restack 他人的分支；共享历史重写先记录范围、参与者同意及旧 ref 备份。
- 新开发采用 merge commit，`dev → main` 也保留 merge ancestry。纯整理工作若确需 squash，先确认没有需要保留的原始 SHA、作者或来源证据；恢复历史的分支不适用该例外。

## PR 流程

1. 一个 PR 解决一个可验收的问题。使用[四栏模板](.github/PULL_REQUEST_TEMPLATE.md)：改什么、谁贡献了什么、如何验证、需要 review 的风险点。有依赖就在第一栏链接直接 base PR，只描述本层增量。人工描述用中文，命令、路径和协议标识保留原文。
2. 先运行与改动相关的本地验证；当前 head 上 CI 已执行的适用门禁给结果链接，无需每人在本机重复整套。区分通过、失败、未运行和不适用，不能把旧 head 的结果当成新 head 通过。
3. 普通 `dev` PR 不增加逐 PR 人工批准硬门。按风险或来源争议指定同伴核查，已有结论直接引用；未审部分如实标明。目标分支 required checks 及其他适用验证通过、已知阻断缺陷处理后，按现有权限合并；未完成的业务验收继续随 PR 保留，不冒充生产完成。
4. `main` 保留 1 票批准；鉴权、敏感数据、破坏性迁移、协议兼容、外部副作用和部署信任边界等高风险改动，进入 `main` 前必须有具备判断能力的非作者人工结论。整合审查聚焦新增差异、集成结果及未关闭风险，引用已有 PR 与对应 SHA 的结论。

纯文字纠错、非行为格式调整等低风险变更，可在目标分支现有规则允许时由作者自合并并写明理由；不能绕过 `main` 的 1 票规则。CI 权限、依赖、协议或运行行为变化按实质影响评估，不因文件扩展名是 Markdown 就视为低风险。提交更新后复核最新 head 的审批与门禁状态，不假定旧批准仍有效。

共同实现者不能互相冒充独立 reviewer。没有合适非作者时，记录实现者、已核查范围与待验收高风险项，并安排有能力的非作者完成上述 `main` 审查；不强迫第三人形式签字，也不把等待超时当作批准。日常合并不以管理员绕过代替门禁。

### 堆叠 PR

仅真实依赖才堆叠，新链建议不超过两层；独立工作并行开发。最底层 base 是 `dev`，上层 base 是直接依赖分支。已有长链由约定的整合者从底向上收敛，调整前保存各层 head，避免重写他人来源记录。

需要使用 `gh stack` 时，先确认其同步策略不会重写待保全历史；工具会 rebase 的链不直接执行自动同步。每次创建、调整或整合后核对 base、直接依赖、完整 diff 与需保留的 SHA。工具选择不改变原始历史保留要求，也不代替创建、推送、改 base 或合并的授权。

### 审查与修复

AI review 辅助发现问题，不替代非作者人工判断或贡献确认。同一 head 不无目的重复全量审查；修改后只复查仍未解决的问题及受影响路径，已有有效结论给链接。机器人的通过状态必须核对所审 SHA；本指南未修改机器人配置。

每条意见先核对当前代码、改动范围及实际风险：有效意见最小修复并跑受影响验证；误报或不采纳意见给依据；有效但超范围的问题记入可追踪的后续事项。合并前相关 thread 应有明确结论，再 resolve；不得只关闭讨论掩盖未处理的缺陷。

### 验证依据

完整适用测试要求和命令清单见 [AGENTS.md 的 Validation](AGENTS.md#validation)，CI 实际任务见 [workflows](.github/workflows)。合并须通过目标分支所有 required checks（含配置要求的 `ci-required`、CodeQL、安全扫描等），并核对改动涉及的独立包验证。CI 证据可以复用，检查范围、安全不变量和失败处理要求不能因流程简化而削弱；无法运行的适用检查明确列为未验证。

## 贡献与 AI 证据

- 使用本人身份提交；PR 区分原始实现、迁移适配、设计、测试和评审，沿用他人成果时给原仓库及固定 SHA/PR。真实共同创作才使用 `Co-authored-by`，不以提交数、代码行数或 AI 会话长度推定贡献。
- 每个可验收里程碑复用已有 PR/Issue 记录参与账号、实际工作、证据和验收状态，不另建日报。跨版本入口只维护[版本与贡献索引](docs/版本与贡献索引.md)，公开条目经相关参与者核对；分歧暂不写成署名或权属结论。
- AI 使用按实际环节说明工具、关键人工判断及验证；已有原始 session 由本人保留，缺失据实标明。按明确用途提供必要的脱敏片段，不把导出完整个人会话设成 PR 门禁，不收集密钥、私人聊天或未授权数据。
- 旧版本可在原仓库独立保留；已约定进入 V3 主线的原始提交另按固定 SHA 验收 ancestry。索引可见、迁移后重新署名和原始 SHA 成为祖先是不同证据，不互相替代。软件权属、论文署名及学分仍按各自适用规则确认。

## 代码规范

- 所有手写代码注释使用中文；生成文件、`//go:build` 与 `//go:embed` 指令除外。
- 用户可见日志使用清晰中文，字段键保持英文；日志与审计不得包含正文、密钥、凭据或原始错误。
- **生成文件是提交构件，禁止手工修改**：`proto/executor.proto` 或 `proto/runtime_host.proto` 变更后必须重新生成 Go 与 Python 产物并连同测试一起提交（`make generate`，CI 有漂移检查）。WASM 工件是否提交由包清单和 workflow 约定决定；零声明源包由 `ailuo pack` 构建，不要求作者提交生成的 WASM。
- 修改工程配置（CI、规则集、依赖机器人、模板）时，同步更新 `AGENTS.md` 与相关文档。
- 密钥、本地数据库、真实校巴数据、虚拟环境与临时输出不得入库。

## 依赖升级

- 依赖升级由 Renovate 管理：Go / Python / GitHub Actions 的 minor/patch 每周批量、major 单独 PR、一律人工评审。
- `grpcio-tools` / `protobuf` 属代码生成工具链，禁止自动升级；升级必须人工重新生成并提交生成物。
- GitHub Actions 固定到提交 SHA，Renovate 随版本注释自动更新 SHA，不直接修改 tag 引用。

## 安全问题

发现漏洞请走 [SECURITY.md](SECURITY.md) 的私有披露流程，不要在 issue 或 PR 中公开敏感细节。

## 数据授权

涉及真实机构数据（如智慧珞珈校巴数据）的接入必须先完成 `docs/数据需求与授权清单.md` 的授权前提；未授权数据不得抓取、逆向或导入。

**当前状态**：智慧珞珈官方数据接口尚未落地，依赖机构数据的功能无法做真实闭环测试。开发与联调一律使用 demo/mock 数据（非权威、显式标记、与生产隔离），生产环境对真实数据 fail closed；数据类 enhancement issue 的验收以书面授权接入为前提，详见 `docs/鉴权与委托授权设计.md`。
