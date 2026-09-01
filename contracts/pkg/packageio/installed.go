// Package packageio 提供与宿主和包管理器共用的已安装包目录读取边界。
package packageio

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
	"sync"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// InstalledRecord 是已安装包的格式层记录。
type InstalledRecord struct {
	Directory string
	Manifest  packagecontract.Manifest
	Lock      packagecontract.Lock
}

var installLocks sync.Map

const (
	// StagePrefix 是安装原子发布阶段目录的前缀。
	StagePrefix = ".stage-"
	// BackupPrefix 是安装原子发布备份目录的前缀。
	BackupPrefix = ".backup-"
)

// InstallLock 返回安装目录内同一包共用的进程内锁，避免恢复中间目录与
// 原子发布在同一进程中并发交错。
func InstallLock(root, id string) *sync.Mutex {
	lock, _ := installLocks.LoadOrStore(root+"\x00"+id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// IsTransientInstallDirectory 判断安装根内的阶段/备份工作目录。
func IsTransientInstallDirectory(name string) bool {
	return strings.HasPrefix(name, StagePrefix) || strings.HasPrefix(name, BackupPrefix)
}

// ReadInstalled 从中性角度读取安装目录：严格解析 manifest + lock、校验内部
// 一致性与每个组件的工件哈希。部署级安全（目录属主/符号链接/权限位）由宿主
// 在发现时叠加校验，本函数面向 CLI 工具、依赖解析与安装回读验证。
func ReadInstalled(ctx context.Context, directory string) (InstalledRecord, error) {
	if directory == "" {
		return InstalledRecord{}, packagecontract.ErrInvalidFormat
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return InstalledRecord{}, err
	}
	source, err := readManifest(directory)
	if err != nil {
		return InstalledRecord{}, err
	}
	lockBytes, err := ReadFileLimited(filepath.Join(directory, "lock.json"), packagecontract.MaxLockBytes)
	if err != nil {
		return InstalledRecord{}, err
	}
	manifest := source.Manifest
	var lock packagecontract.Lock
	if err := packagecontract.DecodeStrictJSON(lockBytes, &lock); err != nil {
		return InstalledRecord{}, err
	}
	if err := packagecontract.ValidateLock(lock, manifest); err != nil {
		return InstalledRecord{}, err
	}
	manifestDigest := sha256.Sum256(source.manifestBytes)
	if lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) {
		return InstalledRecord{}, packagecontract.ErrInvalidFormat
	}
	if err := VerifyInstalledArtifacts(ctx, root, lock.Artifacts); err != nil {
		return InstalledRecord{}, err
	}
	return InstalledRecord{Directory: directory, Manifest: manifest, Lock: lock}, nil
}

// VerifyInstalledArtifacts 复核安装目录内每个锁定工件的路径边界和摘要。
func VerifyInstalledArtifacts(ctx context.Context, root string, artifacts []packagecontract.LockedArtifact) error {
	for _, artifact := range artifacts {
		relative, err := filepath.Rel(root, filepath.Clean(artifact.Path))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return packagecontract.ErrInvalidFormat
		}
		artifactDigest, err := HashArtifact(ctx, artifact.Path, packagecontract.MaxArtifactBytes)
		if err != nil || artifactDigest != artifact.SHA256 {
			return packagecontract.ErrInvalidFormat
		}
	}
	return nil
}

// RecoverInstallRoot 恢复安装发布过程中遗留的备份目录。
// 发布先把旧目录移到备份再放入新目录；进程可能在两次 rename 之间退出，
// 启动或列表读取时必须先恢复旧安装，不能把临时目录静默当作不存在。
func RecoverInstallRoot(ctx context.Context, root string) error {
	if root == "" {
		return packagecontract.ErrInvalidFormat
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	root = absoluteRoot
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(entry.Name(), BackupPrefix) {
			continue
		}
		if !entry.IsDir() {
			return fmt.Errorf("%w: 备份路径不是目录 %q", packagecontract.ErrInvalidFormat, entry.Name())
		}
		if err := recoverInstallBackup(ctx, root, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func recoverInstallBackup(ctx context.Context, root, name string) error {
	backup := filepath.Join(root, name)
	source, err := validateInstallBackup(ctx, root, backup)
	if err != nil {
		children, readErr := os.ReadDir(backup)
		if readErr == nil && len(children) == 0 {
			return nil
		}
		return fmt.Errorf("恢复安装备份 %q 失败: %w", name, err)
	}
	lock := InstallLock(root, source.Manifest.ID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("检查安装备份 %q 失败: %w", name, err)
	}
	target := filepath.Join(root, source.Manifest.ID)
	_, targetErr := os.Lstat(target)
	switch {
	case errors.Is(targetErr, os.ErrNotExist):
		if err := os.Rename(backup, target); err != nil {
			return fmt.Errorf("恢复安装备份 %q 失败: %w", name, err)
		}
	case targetErr != nil:
		return fmt.Errorf("检查安装目录 %q 失败: %w", target, targetErr)
	default:
		if _, err := ReadInstalled(ctx, target); err != nil {
			return fmt.Errorf("安装目录 %q 与备份同时存在且校验失败: %w", target, err)
		}
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("清理已发布安装备份失败: %w", err)
		}
	}
	return nil
}

func validateInstallBackup(ctx context.Context, root, backup string) (InstalledRecord, error) {
	source, err := readManifest(backup)
	if err != nil {
		return InstalledRecord{}, err
	}
	lockBytes, err := ReadFileLimited(filepath.Join(backup, "lock.json"), packagecontract.MaxLockBytes)
	if err != nil {
		return InstalledRecord{}, err
	}
	var lock packagecontract.Lock
	if err := packagecontract.DecodeStrictJSON(lockBytes, &lock); err != nil {
		return InstalledRecord{}, err
	}
	manifestDigest := sha256.Sum256(source.manifestBytes)
	if lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) ||
		packagecontract.ValidateLock(lock, source.Manifest) != nil {
		return InstalledRecord{}, packagecontract.ErrInvalidFormat
	}
	originalRoot := filepath.Join(root, source.Manifest.ID)
	relocated := lock
	relocated.Artifacts = append([]packagecontract.LockedArtifact(nil), lock.Artifacts...)
	for index := range relocated.Artifacts {
		artifact := &relocated.Artifacts[index]
		artifact.Path, err = relocatePath(originalRoot, backup, artifact.Path)
		if err != nil {
			return InstalledRecord{}, err
		}
		if artifact.Process != nil {
			process := *artifact.Process
			process.Path, err = relocatePath(originalRoot, backup, process.Path)
			if err != nil {
				return InstalledRecord{}, err
			}
			process.WorkDir, err = relocatePath(originalRoot, backup, process.WorkDir)
			if err != nil {
				return InstalledRecord{}, err
			}
			artifact.Process = &process
		}
	}
	if err := packagecontract.ValidateLock(relocated, source.Manifest); err != nil {
		return InstalledRecord{}, err
	}
	if err := VerifyInstalledArtifacts(ctx, backup, relocated.Artifacts); err != nil {
		return InstalledRecord{}, err
	}
	return InstalledRecord{Directory: backup, Manifest: source.Manifest, Lock: lock}, nil
}

func relocatePath(from, to, value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", packagecontract.ErrInvalidFormat
	}
	relative, err := filepath.Rel(from, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", packagecontract.ErrInvalidFormat
	}
	return filepath.Join(to, relative), nil
}

// ListInstalled 列出安装根目录内的全部已安装包（按 ID 排序）。
func ListInstalled(ctx context.Context, root string) ([]InstalledRecord, error) {
	if root == "" {
		return nil, packagecontract.ErrInvalidFormat
	}
	if err := RecoverInstallRoot(ctx, root); err != nil {
		return nil, err
	}
	return listInstalled(ctx, root)
}

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
		// Install 的阶段/备份目录建在安装根内（同文件系统才能原子 rename）。
		// 崩溃后可能残留，跳过而不是报错——否则一次失败的安装会让列表与依赖
		// 解析永久失败。其他非包条目仍然 fail closed。
		if IsTransientInstallDirectory(entry.Name()) {
			if !entry.IsDir() {
				return nil, fmt.Errorf("%w: 临时安装路径不是目录 %q", packagecontract.ErrInvalidFormat, entry.Name())
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") || !entry.IsDir() {
			return nil, fmt.Errorf("%w: 安装根包含非包目录 %q", packagecontract.ErrInvalidFormat, entry.Name())
		}
		record, err := ReadInstalled(ctx, filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if record.Manifest.ID != entry.Name() {
			return nil, fmt.Errorf("%w: 目录 %q 与清单 ID %q 不一致", packagecontract.ErrInvalidFormat, entry.Name(), record.Manifest.ID)
		}
		if _, duplicate := seen[record.Manifest.ID]; duplicate {
			return nil, fmt.Errorf("%w: 重复包 ID %q", packagecontract.ErrInvalidFormat, record.Manifest.ID)
		}
		seen[record.Manifest.ID] = struct{}{}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Manifest.ID < records[j].Manifest.ID })
	return records, nil
}

type manifestFile struct {
	Manifest      packagecontract.Manifest
	manifestBytes []byte
}

func readManifest(directory string) (manifestFile, error) {
	manifestBytes, err := ReadFileLimited(filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
	if err != nil {
		return manifestFile{}, err
	}
	var manifest packagecontract.Manifest
	if err := packagecontract.DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		return manifestFile{}, err
	}
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		return manifestFile{}, err
	}
	return manifestFile{Manifest: manifest, manifestBytes: manifestBytes}, nil
}
