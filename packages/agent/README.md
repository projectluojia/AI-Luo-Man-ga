# AI珞 Executor 包

这个包实现版本化的 `executor.v1` 运行时。它是普通的 `isolated` / `executor`
组件，Core 通过安装清单发现并通过 gRPC 使用，不依赖 Core 的 Python 源码。
`ailuo.toml` 中的 `[component.process]` 只声明一次启动模板；`ailuo-pm install` 会自动
把它解析为安装目录内的绝对进程规格，部署者不需要手工修改 `lock.json`。

本地开发由 `uv` 和 `runtime/uv.lock` 管理：

```bash
uv sync --project runtime --locked
uv run --project runtime --locked ruff check runtime
uv run --project runtime --locked python -m unittest discover -s runtime -p 'test_*.py' -v
cd runtime
uv run --project . --locked python -m agent.runtime --listen 127.0.0.1:50051
```

启动时由 Deployment 提供模型 Provider 所需的配置：`AILUO_MODEL_API_KEY_FILE`
指向受限密钥文件，`AILUO_MODEL_BASE_URL`、`AILUO_MODEL_*` 重试/限流变量按需
配置；监听地址通过 `--listen` 传入。当前应由 Agent 自己的 Deployment/监督器
启动该进程，再由 Core 按安装 lock 连接；Core 不负责解释或注入本包的 Provider
环境变量。日志配置中的非法值会使进程直接退出，不会静默回退到默认值。当前不要
将 `AILUO_MANAGE_EXECUTOR=true` 用于本包；Core 托管启动不继承这些配置，缺少
Provider 配置会按未就绪处理。
