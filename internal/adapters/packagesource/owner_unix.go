//go:build unix

package packagesource

import (
	"os"
	"syscall"
)

// ownerMatchesProcess 校验文件属主与当前进程为同一用户（Unix eUID 语义）。
// path 参数在 Unix 由 FileInfo 自带的 UID 满足，保留以对齐跨平台签名。
func ownerMatchesProcess(_ string, info os.FileInfo) bool {
	metadata, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(metadata.Uid) == os.Geteuid()
}

// groupOrWorldWritable 报告组/其他可写位：Unix 语义下不允许安装内容对
// 同组或任意用户可写。
func groupOrWorldWritable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o022 != 0
}
