package packagecontract

import (
	"os"
	"path/filepath"
)

// 包格式的统一大小上限，供作者工具、安装器和宿主适配器共同执行。
const (
	MaxManifestBytes = int64(256 << 10)
	MaxLockBytes     = int64(64 << 10)
	MaxArtifactBytes = int64(1 << 30)
)

// DefaultInstallRoot 返回包管理器与 Core 共用的用户级安装根目录。
func DefaultInstallRoot() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "ailuo", "runtime")
}
