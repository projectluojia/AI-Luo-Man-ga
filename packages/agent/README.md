# AI珞 Executor 包

这个包实现版本化的 `executor.v1` 运行时。它是普通的 `isolated` / `executor`
组件，Core 通过安装清单发现并通过 gRPC 使用，不依赖 Core 的 Python 源码。

本地开发由 `uv` 和 `runtime/uv.lock` 管理：

```bash
uv sync --project runtime --locked
uv run --project runtime --locked python -m unittest discover -s runtime -p 'test_*.py' -v
cd runtime
uv run --project . --locked python -m agent.runtime --listen 127.0.0.1:50051
```

启动时由 Deployment 提供模型 Provider 所需的配置：`AILUO_MODEL_API_KEY_FILE`
指向受限密钥文件，`AILUO_MODEL_BASE_URL`、`AILUO_MODEL_*` 重试/限流变量按需
配置；监听地址通过 `--listen` 传入。Core 不负责解释本包的 Provider 环境变量。
