//go:build wasip1

package guestkit

import "unsafe"

// ailuo.store 宿主函数声明桩（仅 wasip1 guest 内有效）：请求与响应都位于
// guest 线性内存，返回响应长度，HostFunctionError 表示宿主侧失败。
//
//go:wasmimport ailuo.store get
func storeGet(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32

//go:wasmimport ailuo.store list
func storeList(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32

//go:wasmimport ailuo.store put
func storePut(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32

//go:wasmimport ailuo.store delete
func storeDelete(requestPtr unsafe.Pointer, requestLen uint32, responsePtr unsafe.Pointer, responseCap uint32) uint32
