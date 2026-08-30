package packmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 安装目录格式的容量上限（与宿主发现校验一致）。
const (
	MaxManifestBytes = int64(256 << 10)
	MaxLockBytes     = int64(64 << 10)
	MaxArtifactBytes = int64(1 << 30)
)

// ReadInstalled 从中性角度读取安装目录：严格解析 manifest + lock、校验内部
// 一致性与工件哈希。部署级安全（目录属主/符号链接/权限位）由宿主在发现时
// 叠加校验，本函数面向 CLI 工具、依赖解析与安装回读验证。
func ReadInstalled(ctx context.Context, directory string) (InstalledRecord, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return InstalledRecord{}, err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return InstalledRecord{}, os.ErrNotExist
	}
	if err != nil {
		return InstalledRecord{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return InstalledRecord{}, ErrInvalidFormat
	}
	manifestBytes, err := ReadFileLimited(filepath.Join(directory, "manifest.json"), MaxManifestBytes)
	if err != nil {
		return InstalledRecord{}, err
	}
	lockBytes, err := ReadFileLimited(filepath.Join(directory, "lock.json"), MaxLockBytes)
	if err != nil {
		return InstalledRecord{}, err
	}
	var manifest Manifest
	if err := DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		return InstalledRecord{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return InstalledRecord{}, err
	}
	var lock Lock
	if err := DecodeStrictJSON(lockBytes, &lock); err != nil {
		return InstalledRecord{}, err
	}
	if err := ValidateLock(lock); err != nil {
		return InstalledRecord{}, err
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	if lock.PackageID != manifest.ID || lock.PackageVersion != manifest.Version ||
		lock.Mode != manifest.Mode || lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) {
		return InstalledRecord{}, ErrInvalidFormat
	}
	artifactPath, err := filepath.Abs(lock.ArtifactPath)
	if err != nil || filepath.Clean(artifactPath) != artifactPath {
		return InstalledRecord{}, ErrInvalidFormat
	}
	relative, err := filepath.Rel(directory, artifactPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return InstalledRecord{}, ErrInvalidFormat
	}
	artifactDigest, err := HashFile(ctx, artifactPath, MaxArtifactBytes)
	if err != nil || artifactDigest != lock.ArtifactSHA256 {
		return InstalledRecord{}, ErrInvalidFormat
	}
	record := InstalledRecord{
		Directory: directory, ArtifactPath: artifactPath,
		Manifest: manifest, Process: lock.Process,
	}
	if manifest.Mode == ModeIsolated {
		if lock.Process == nil || lock.Process.Path != artifactPath {
			return InstalledRecord{}, ErrInvalidFormat
		}
		if err := ValidateProcessSpec(*lock.Process); err != nil {
			return InstalledRecord{}, err
		}
	}
	if manifest.Mode == ModeHosted && lock.Process != nil {
		return InstalledRecord{}, ErrInvalidFormat
	}
	return record, nil
}

// ListInstalled 列出安装根目录内的全部已安装包（按 ID 排序）。
// 目录名必须等于清单 ID，ID 去重；不属于包目录的目录（缺 manifest）按
// 目录不存在处理还是报错？——安装根由本工具维护，异常目录报错（fail closed）。
func ListInstalled(ctx context.Context, root string) ([]InstalledRecord, error) {
	if root == "" {
		return nil, ErrInvalidFormat
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := waitForMutationUnlock(ctx, absoluteRoot); err != nil {
		return nil, err
	}
	return listInstalled(ctx, absoluteRoot)
}

// listInstalled 在调用方已持有安装根变更锁时读取包，避免锁内读操作等待自身。
func listInstalled(ctx context.Context, root string) ([]InstalledRecord, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []InstalledRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]InstalledRecord, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Name() == rootMutationLockName || strings.HasPrefix(entry.Name(), stagePrefix) || strings.HasPrefix(entry.Name(), backupPrefix) {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") || !entry.IsDir() {
			return nil, fmt.Errorf("%w: 安装根包含非包目录 %q", ErrInvalidFormat, entry.Name())
		}
		record, err := ReadInstalled(ctx, filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if record.Manifest.ID != entry.Name() {
			return nil, fmt.Errorf("%w: 目录 %q 与清单 ID %q 不一致", ErrInvalidFormat, entry.Name(), record.Manifest.ID)
		}
		if _, duplicate := seen[record.Manifest.ID]; duplicate {
			return nil, fmt.Errorf("%w: 重复包 ID %q", ErrInvalidFormat, record.Manifest.ID)
		}
		seen[record.Manifest.ID] = struct{}{}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Manifest.ID < records[j].Manifest.ID })
	return records, nil
}
