# 交叉编译 strings.tool 为 wasm32-wasi 工件。
# Windows / Linux / macOS 均可执行（Go 交叉编译不依赖本机 wasm 工具链）。
$env:GOOS = "wasip1"
$env:GOARCH = "wasm"
try {
    go build -trimpath -o strings.tool.wasm .
    Write-Output "已生成 strings.tool.wasm"
} finally {
    $env:GOOS = ""
    $env:GOARCH = ""
}
