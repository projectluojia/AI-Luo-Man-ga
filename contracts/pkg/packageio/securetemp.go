package packageio

import (
	"os"
	"path/filepath"
)

// CreateSecureDirectory 创建一个可交给安装事务使用的安全临时目录。
// 安全属性由当前平台实现，失败时删除已创建目录。
func CreateSecureDirectory(parent, pattern string) (string, error) {
	path, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	if err := secureCreatedDirectory(path); err != nil {
		_ = os.RemoveAll(path)
		return "", err
	}
	return filepath.Clean(path), nil
}
