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
	"strconv"
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
var installRootLocks sync.Map
var projectLocks sync.Map

var ErrFileLocked = errors.New("package operation is already locked")

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

// InstallRootLock 返回同一安装根共用的进程内事务锁。项目同步、安装、升级和
// 卸载必须在根级串行，避免不同操作分别通过包级锁拼出半套安装闭包。
func InstallRootLock(root string) *sync.Mutex {
	lock, _ := installRootLocks.LoadOrStore(root, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// ProjectLock 返回同一项目锁文件共用的进程内事务锁，避免不同安装根的同步
// 操作同时改写同一份 ailuo.lock。
func ProjectLock(path string) *sync.Mutex {
	lock, _ := projectLocks.LoadOrStore(path, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// InstallRootLockPath 返回安装根的跨进程锁文件路径。锁文件放在安装根外，
// 不会被 ListInstalled 当成未知安装条目。
func InstallRootLockPath(root string) string {
	return root + ".ailuo-install.lock"
}

// FileLock 是一个基于 O_EXCL 的跨进程独占锁。锁文件在进程崩溃后会保留，
// 后续操作必须显式确认并清理，不能把未知持有者静默当成已退出。
type FileLock struct {
	path string
	file *os.File
}

// AcquireFileLock 创建独占锁文件；已有锁直接失败，不等待也不删除。
func AcquireFileLock(ctx context.Context, path string) (*FileLock, error) {
	if path == "" {
		return nil, packagecontract.ErrInvalidFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrFileLocked, path)
	}
	if err != nil {
		return nil, err
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &FileLock{path: path, file: file}, nil
}

// RemoveFileLock 显式删除遗留锁。调用方必须先确认没有其他包管理进程仍在运行；
// 正常流程不调用此函数，也不会依据锁内 PID 自动删除未知锁。
func RemoveFileLock(path string) error {
	if path == "" {
		return packagecontract.ErrInvalidFormat
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Close 释放锁文件；释放失败会返回错误，调用方不能把它当作成功清理。
func (l *FileLock) Close() error {
	if l == nil {
		return nil
	}
	var result []error
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			result = append(result, err)
		}
		l.file = nil
	}
	if err := RemoveFileLock(l.path); err != nil {
		result = append(result, err)
	}
	return errors.Join(result...)
}

// IsTransientInstallDirectory 判断安装根内的阶段/备份工作目录。
func IsTransientInstallDirectory(name string) bool {
	return strings.HasPrefix(name, StagePrefix) || strings.HasPrefix(name, BackupPrefix)
}

// ReadInstalled 读取安装目录：严格解析 manifest + lock、校验内部一致性、
// 每个组件的工件哈希和统一的文件系统安全策略。
func ReadInstalled(ctx context.Context, directory string) (InstalledRecord, error) {
	if directory == "" {
		return InstalledRecord{}, packagecontract.ErrInvalidFormat
	}
	if err := ValidateSecureTree(ctx, directory); err != nil {
		return InstalledRecord{}, err
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
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := ValidateSecureDirectory(root); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
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
		if errors.Is(err, packagecontract.ErrInvalidFormat) {
			children, readErr := os.ReadDir(backup)
			if readErr == nil && len(children) == 0 {
				return nil
			}
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
	if err := ValidateSecureTree(ctx, backup); err != nil {
		return InstalledRecord{}, err
	}
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
