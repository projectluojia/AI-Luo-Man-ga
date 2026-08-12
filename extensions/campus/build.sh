#!/usr/bin/env sh
# 交叉编译 campus 为 wasm32-wasi 工件。
set -eu
GOOS=wasip1 GOARCH=wasm go build -trimpath -o campus.wasm .
