package packmgr

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
)

// copyArtifact 复制文件或目录工件到阶段目录；目录内只接受普通文件和目录，
// 不跟随任何符号链接，避免把包边界扩展到源目录之外。
func copyArtifact(sourcePath, destinationPath string) error {
	info, err := os.Lstat(sourcePath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return packagecontract.ErrInvalidFormat
	}
	if info.Mode().IsRegular() {
		return copyFile(sourcePath, destinationPath)
	}
	if !info.IsDir() {
		return packagecontract.ErrInvalidFormat
	}
	if err := os.MkdirAll(destinationPath, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Chmod(destinationPath, info.Mode().Perm()); err != nil {
		return err
	}
	return filepath.WalkDir(sourcePath, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return packagecontract.ErrInvalidFormat
		}
		relative, err := filepath.Rel(sourcePath, current)
		if err != nil {
			return packagecontract.ErrInvalidFormat
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destinationPath, relative)
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return packagecontract.ErrInvalidFormat
		}
		return copyFile(current, target)
	})
}

// artifactPathWithin 判断一个路径是否位于目录工件根内。
func artifactPathWithin(root, candidate string) bool {
	if root == candidate {
		return true
	}
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
