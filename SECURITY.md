# 安全策略

AI珞（爱珞）V3 是多入口 AI 助手平台，内核负责鉴权、App 隔离、密钥处理与外部访问。本文件定义安全问题的披露渠道与处理承诺。

## 报告漏洞

请通过 **GitHub 私有漏洞披露**（Private vulnerability reporting）提交，不要公开在 issue 或 PR 中：

- 入口：仓库 `Security` 页签 → `Report a vulnerability`，或直接访问
  <https://github.com/projectluojia/AI-Luo-Man-ga/security/advisories/new>

提交时请尽量包含：受影响的版本/提交、复现步骤、影响评估与建议修复。请勿在报告中包含真实用户数据、生产密钥或凭据。

## 处理承诺

- 确认收到后 3 个工作日内回复；
- 高危问题优先修复，修复与披露节奏由维护者协调；
- 修复落地的同时补充回归测试与受影响文档。

## 关注范围

- 鉴权与授权、App 隔离与权限收窄；
- 跨进程信任边界（gRPC、SSE、wasm 沙箱）与输入校验；
- 密钥加载与轮换、日志与审计的 redaction；
- 状态机原子性与可恢复性、幂等与重放；
- 依赖供应链漏洞（Go 模块图与 Python 锁定文件由 `security-scan.yml` 定期扫描）。

## 不在范围内

- 未获校方书面授权的数据抓取、逆向或导入（见 `docs/数据需求与授权清单.md`）；
- 产品前端（本仓库不拥有前端产物）的一般性问题，请走 issue。
