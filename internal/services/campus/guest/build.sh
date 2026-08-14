#!/bin/sh
# 将 campus guest 编译为 wasm32-wasi 工件，直接输出到同服务的内嵌目录
# internal/services/campus/builtin/（go:embed 唯一来源），不在其他位置保留副本。
set -e
GOOS=wasip1 GOARCH=wasm go build -trimpath -o ../builtin/campus.wasm .
echo "campus.wasm 已生成到 internal/services/campus/builtin/"
