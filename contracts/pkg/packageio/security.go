package packageio

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrInsecurePath 表示路径不符合当前平台的安装安全策略。
var ErrInsecurePath = errors.New("path violates package security policy")

// ValidateSecurePath 校验单个安装路径的文件类型、属主和平台安全权限。
func ValidateSecurePath(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return ErrInsecurePath
	}
	return validatePlatformPath(path, info)
}

// ValidateSecureDirectory 校验一个受信安装目录。
func ValidateSecureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInsecurePath
	}
	return validatePlatformPath(path, info)
}

// ValidateSecureTree 校验目录及其全部节点，禁止符号链接和非受信主体修改
// 安装内容。调用方应在原子替换、恢复和运行时装载前重复调用。
func ValidateSecureTree(ctx context.Context, path string) error {
	return filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrInsecurePath
		}
		info, err := entry.Info()
		if err != nil || (current == path && !info.IsDir()) || (!info.IsDir() && !info.Mode().IsRegular()) {
			return ErrInsecurePath
		}
		return validatePlatformPath(current, info)
	})
}
