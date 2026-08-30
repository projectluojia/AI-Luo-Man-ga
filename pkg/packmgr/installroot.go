package packmgr

import (
	"os"
	"path/filepath"
)

// DefaultInstallRoot 返回包管理器使用的用户级安装根目录。
func DefaultInstallRoot() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "ailuo", "runtime")
}
