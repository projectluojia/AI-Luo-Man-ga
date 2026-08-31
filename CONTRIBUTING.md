# 贡献指南

欢迎贡献 AI珞（爱珞）V3。本文件是贡献流程的唯一入口；架构、安全与存储的深度契约以 `AGENTS.md` 和 `docs/` 设计文档为准。

## 快速开始

要求：Go（版本见 `go.mod`）、[uv](https://docs.astral.sh/uv/)、Python 3.11–3.14。

```bash
make test-agent    # 按 packages/agent/runtime/uv.lock 运行 Executor 包测试
make test-campus   # 校巴 guest 的 WASI vet 与交叉编译
make test          # Core Go 单元测试
make test-race     # Go race 检测
make vet           # go vet
make test-integration  # Runtime Host 跨进程集成测试（Unix 平台）
make test-e2e      # Executor e2e（源包，Unix 平台）
```

## 分支与提交

- 分支命名使用前缀：`feat/`、`fix/`、`chore/`、`docs/`、`refactor/`。
- 提交信息、PR 标题和 squash 合并消息均严格遵循 Conventional Commits；破坏性变更在标题中使用 `!`，并在 footer 写明 `BREAKING CHANGE: <说明>`（`pr-title.yml` 强制校验）。
- 一个 commit 是一个自洽的逻辑单元：可独立构建、独立回滚（例如迁移 + 存取代码 + 测试同处一个 commit）。
- `main` 只接受 squash 合并，合并前必须通过全部 required status checks。

## PR 流程

1. 从最新 `main` 切出前缀分支，合并前自行 rebase 保持 up-to-date。
2. 按 `.github/PULL_REQUEST_TEMPLATE.md` 填写变更内容、影响范围、验证证据与安全治理清单。
3. 标题格式：`<type>[optional scope][!]: <描述>`，类型限定 `feat` / `fix` / `docs` / `style` / `refactor` / `perf` / `test` / `build` / `ci` / `chore` / `revert`；破坏性变更还要有 `BREAKING CHANGE: <说明>` footer。
4. 等待 `ci-required`、CodeQL 和安全扫描等 required checks 全绿；再确认变更涉及的 Agent/Campus 独立包 workflow 通过。Core 门禁覆盖三平台核心测试、Linux 完整质量门禁和 proto 生成物漂移；Agent workflow 额外覆盖安装后 e2e。
5. 合并需 1 人审批；审批后新的 push 会使旧审批失效，需重新审批。管理员绕过仅限紧急修复，日常合并一律走规则集。

### 堆叠 PR

存在真实依赖时，用 `gh stack` 按底层到顶层维护分支和 PR：`gh stack init --base main <bottom> <next> ...` 创建新 stack，`gh stack link <stack-number> <pr-or-branch> ...` 接管已有 PR，`gh stack sync` 同步远端，`gh stack view --json` 复核完整链条。每个上层 PR 只能以直接依赖的下一层分支为 base；需要 ready 审查时使用 `--open`，否则保持 draft。不要手工改 base 后继续提交。

### 合并前本地验证（与 CI 部分重合）

以下是本地可运行的主要门禁，不等同于 CI 完整门禁。CI 另外执行 Protobuf 漂移、包发布物 `pack → install → list`、Agent 安装后 e2e、CodeQL 和安全扫描；任何未运行的适用门禁都要在交付说明中标明。

```bash
files="$(gofmt -l .)"
test -z "$files" || { echo "$files"; exit 1; }
go mod verify
go mod tidy -diff
go test ./...
make test-campus
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 '-checks=inherit,-SA1019' ./...
actionlint .github/workflows/*.yml
uv sync --project packages/agent/runtime --locked
uv run --project packages/agent/runtime --locked python -m compileall -q packages/agent/runtime
(cd packages/agent/runtime && uv run --project . --locked ruff check .)
uv run --project packages/agent/runtime --locked python -m unittest discover -s packages/agent/runtime -p 'test_*.py' -v
go test -race ./...
go vet ./...
go test -tags=integration ./internal/kernel/loader -v -timeout=30s
AILUO_EXECUTOR_PACKAGE_DIR="$PWD/packages/agent" go test -tags=integration ./e2e -v -timeout=60s
```

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
