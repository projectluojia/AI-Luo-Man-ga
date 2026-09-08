//go:build !wasip1

package guestkit

import "unsafe"

// 原生构建（仅测试使用）的宿主函数桩：没有 wasmimport 绑定，任何调用都以
// 宿主侧失败返回。guest 生产构建必须带 wasip1 标签（go-wasm 构建器默认）。
func storeGet(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint32    { return HostFunctionError }
func storeList(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint32   { return HostFunctionError }
func storePut(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint32    { return HostFunctionError }
func storeDelete(unsafe.Pointer, uint32, unsafe.Pointer, uint32) uint32 { return HostFunctionError }
