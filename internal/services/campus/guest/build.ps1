# 将 campus guest 编译为 wasm32-wasi 工件（UTF-8 with BOM，兼容 Windows PowerShell 5.1+）
# 工件直接输出到同服务的内嵌目录 internal/services/campus/builtin/（go:embed 唯一来源），
# 不在任何其他位置保留副本。
$ErrorActionPreference = "Stop"
$env:GOOS = "wasip1"
$env:GOARCH = "wasm"
try {
    go build -trimpath -o ../builtin/campus.wasm .
    Write-Output "campus.wasm 已生成到 internal/services/campus/builtin/"
} finally {
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
}
