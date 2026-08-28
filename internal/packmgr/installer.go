package packmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var installLocks sync.Map

const (
	stagePrefix  = ".stage-"
	backupPrefix = ".backup-"
)

func packageInstallLock(root, id string) *sync.Mutex {
	lock, _ := installLocks.LoadOrStore(root+"\x00"+id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// Install 从源包目录或发布 tarball 安装到安装根目录：校验源（manifest.json +
// entrypoint 工件）、解析依赖（已安装包满足约束）、原子发布
// manifest+lock+artifact，并回读验证。目标已有同 ID 且版本不同时替换（升级
// 语义）；版本相同视为重复安装并返回错误。
func Install(ctx context.Context, root, sourcePath string) (InstalledRecord, error) {
	if root == "" {
		return InstalledRecord{}, ErrInvalidFormat
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return InstalledRecord{}, err
	}
	sourceDir, cleanup, err := unpackSource(sourcePath)
	if err != nil {
		return InstalledRecord{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return InstalledRecord{}, err
	}
	installLock := packageInstallLock(root, source.Manifest.ID)
	installLock.Lock()
	defer installLock.Unlock()
	if err := resolveDependencies(ctx, root, source.Manifest.Dependencies); err != nil {
		return InstalledRecord{}, err
	}
	targetDir := filepath.Join(root, source.Manifest.ID)
	if existing, err := ReadInstalled(ctx, targetDir); err == nil {
		if existing.Manifest.Version == source.Manifest.Version {
			return InstalledRecord{}, fmt.Errorf("包 %s@%s 已安装", source.Manifest.ID, source.Manifest.Version)
		}
		if err := validateDependents(ctx, root, source.Manifest.ID, source.Manifest.Version); err != nil {
			return InstalledRecord{}, err
		}
	}
	// 原子发布：先在安装根内创建临时阶段目录，再 rename 为目标。
	stageDir, err := os.MkdirTemp(root, stagePrefix)
	if err != nil {
		return InstalledRecord{}, err
	}
	defer os.RemoveAll(stageDir)
	// 写入 manifest（保留源字节，lock 中的 manifest digest 必须一致）。
	if err := os.WriteFile(filepath.Join(stageDir, "manifest.json"), source.manifestBytes, 0o640); err != nil {
		return InstalledRecord{}, err
	}
	// 写入工件并计算哈希。
	artifactName := filepath.Base(source.Manifest.Entrypoint)
	stageArtifact := filepath.Join(stageDir, artifactName)
	if err := copyFile(source.artifactPath, stageArtifact); err != nil {
		return InstalledRecord{}, err
	}
	artifactDigest, err := HashFile(ctx, stageArtifact, MaxArtifactBytes)
	if err != nil {
		return InstalledRecord{}, err
	}
	// 写入 lock：ArtifactPath 引用发布后的最终路径（stage 目录会被 rename 为目标）。
	manifestDigest := sha256.Sum256(source.manifestBytes)
	targetArtifact := filepath.Join(targetDir, artifactName)
	lock := Lock{
		SchemaVersion: SchemaVersion, PackageID: source.Manifest.ID,
		PackageVersion: source.Manifest.Version, Mode: source.Manifest.Mode,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		ArtifactSHA256: artifactDigest,
		ArtifactPath:   targetArtifact,
	}
	if source.Manifest.Mode == ModeIsolated {
		lock.Process = defaultProcessSpec(targetArtifact, targetDir)
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "lock.json"), lockBytes, 0o640); err != nil {
		return InstalledRecord{}, err
	}
	return publishStage(ctx, root, targetDir, stageDir)
}

// publishStage 把阶段目录发布为安装目录并回读验证。旧安装先移到同一安装根内
// 的备份目录，任何失败都恢复旧版本，成功后才清理备份。
func publishStage(ctx context.Context, root, targetDir, stageDir string) (InstalledRecord, error) {
	backupDir, err := reserveBackupDir(root, targetDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	published := false
	restore := func() error {
		var restoreErrors []error
		if published {
			if err := os.RemoveAll(targetDir); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("删除新安装目录: %w", err))
			}
		}
		if backupDir != "" {
			if err := os.Rename(backupDir, targetDir); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("恢复旧安装目录: %w", err))
			}
		}
		return errors.Join(restoreErrors...)
	}
	if err := os.Rename(stageDir, targetDir); err != nil {
		if rollbackErr := restore(); rollbackErr != nil {
			return InstalledRecord{}, fmt.Errorf("安装发布失败: %v；回滚失败: %w", err, rollbackErr)
		}
		return InstalledRecord{}, err
	}
	published = true
	record, err := ReadInstalled(ctx, targetDir)
	if err != nil {
		if rollbackErr := restore(); rollbackErr != nil {
			return InstalledRecord{}, fmt.Errorf("安装后验证失败: %v；回滚失败: %w", err, rollbackErr)
		}
		return InstalledRecord{}, fmt.Errorf("安装后验证失败: %w", err)
	}
	if backupDir != "" {
		if err := os.RemoveAll(backupDir); err != nil {
			return InstalledRecord{}, fmt.Errorf("清理旧版本备份失败: %w", err)
		}
	}
	return record, nil
}

// reserveBackupDir 把已有安装目录移到同一安装根内的唯一备份目录。
func reserveBackupDir(root, targetDir string) (string, error) {
	if _, err := os.Lstat(targetDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	backupDir, err := os.MkdirTemp(root, backupPrefix)
	if err != nil {
		return "", err
	}
	if err := os.Remove(backupDir); err != nil {
		return "", err
	}
	if err := os.Rename(targetDir, backupDir); err != nil {
		return "", err
	}
	return backupDir, nil
}

// defaultProcessSpec 为 isolated 安装记录生成与工件绑定的最小进程规格。
func defaultProcessSpec(artifactPath, workDir string) *ProcessSpec {
	base := strings.TrimSuffix(filepath.Base(artifactPath), filepath.Ext(artifactPath))
	return &ProcessSpec{Path: artifactPath, WorkDir: workDir, Address: "unix:" + filepath.Join(workDir, base+".sock")}
}

// Upgrade 要求包已安装且源目录版本号不同，然后安装。
func Upgrade(ctx context.Context, root, id, sourceDir string) (InstalledRecord, error) {
	if root == "" || !stableLowerPattern.MatchString(id) {
		return InstalledRecord{}, fmt.Errorf("包 %q 标识非法", id)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return InstalledRecord{}, err
	}
	existing, err := ReadInstalled(ctx, filepath.Join(absoluteRoot, id))
	if err != nil {
		return InstalledRecord{}, fmt.Errorf("包 %q 未安装", id)
	}
	source, err := readSourceManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	if source.Manifest.ID != id {
		return InstalledRecord{}, fmt.Errorf("源目录包 ID %q 与升级目标 %q 不一致", source.Manifest.ID, id)
	}
	if source.Manifest.Version == existing.Manifest.Version {
		return InstalledRecord{}, fmt.Errorf("包 %s 已安装版本 %s，升级目标版本相同", id, existing.Manifest.Version)
	}
	return Install(ctx, root, sourceDir)
}

// Uninstall 删除已安装包目录。仅当目录包含 manifest.json 时删除（安全防护）。
func Uninstall(ctx context.Context, root, id string) error {
	if root == "" || !stableLowerPattern.MatchString(id) {
		return fmt.Errorf("包 %q 标识非法", id)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absoluteRoot, 0o750); err != nil {
		return err
	}
	lock := packageInstallLock(absoluteRoot, id)
	lock.Lock()
	defer lock.Unlock()
	target := filepath.Join(absoluteRoot, id)
	relative, err := filepath.Rel(absoluteRoot, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrInvalidFormat
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("包 %q 未安装", id)
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("目标 %q 不是目录", target)
	}
	manifestInfo, err := os.Lstat(filepath.Join(target, "manifest.json"))
	if err != nil || !manifestInfo.Mode().IsRegular() {
		return fmt.Errorf("目录 %q 不是安装包（缺少 manifest.json）", target)
	}
	if err := validateDependents(ctx, absoluteRoot, id, ""); err != nil {
		return err
	}
	return os.RemoveAll(target)
}

// unpackSource 解析安装源：目录直接使用；.tgz 发布物严格解压到临时目录。
func unpackSource(source string) (string, func(), error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", nil, err
	}
	if info.IsDir() {
		return source, nil, nil
	}
	if !strings.HasSuffix(strings.ToLower(source), ".tgz") {
		return "", nil, fmt.Errorf("安装源必须是包目录或 .tgz 发布物")
	}
	temp, err := os.MkdirTemp("", "ailuo-unpack-")
	if err != nil {
		return "", nil, err
	}
	if err := unpackTarball(source, temp); err != nil {
		os.RemoveAll(temp)
		return "", nil, err
	}
	return temp, func() { os.RemoveAll(temp) }, nil
}

// sourcePackage 是源包目录读取结果：清单字节原样保留以锁定 digest。
type sourcePackage struct {
	Manifest      Manifest
	artifactPath  string
	manifestBytes []byte
}

// readSourceManifest 读取源包目录的 manifest.json 与 entrypoint 工件。
func readSourceManifest(sourceDir string) (sourcePackage, error) {
	manifestBytes, err := ReadFileLimited(filepath.Join(sourceDir, "manifest.json"), MaxManifestBytes)
	if err != nil {
		return sourcePackage{}, err
	}
	var manifest Manifest
	if err := DecodeStrictJSON(manifestBytes, &manifest); err != nil {
		return sourcePackage{}, err
	}
	if err := ValidateManifest(manifest); err != nil {
		return sourcePackage{}, err
	}
	artifactPath := filepath.Join(sourceDir, manifest.Entrypoint)
	relative, err := filepath.Rel(sourceDir, artifactPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return sourcePackage{}, fmt.Errorf("entrypoint 超出源目录")
	}
	info, err := os.Lstat(artifactPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxArtifactBytes {
		return sourcePackage{}, fmt.Errorf("entrypoint 工件无效: %w", err)
	}
	return sourcePackage{
		Manifest: manifest, artifactPath: artifactPath, manifestBytes: manifestBytes,
	}, nil
}

// resolveDependencies 检查每个依赖在安装根内存在已安装包且版本满足约束。
func resolveDependencies(ctx context.Context, root string, deps []Dependency) error {
	if len(deps) == 0 {
		return nil
	}
	installed, err := ListInstalled(ctx, root)
	if err != nil {
		return err
	}
	versionByID := make(map[string]string, len(installed))
	for _, record := range installed {
		versionByID[record.Manifest.ID] = record.Manifest.Version
	}
	var missing []string
	for _, dep := range deps {
		versionText, ok := versionByID[dep.ID]
		if !ok {
			missing = append(missing, dep.ID+" (未安装)")
			continue
		}
		candidate, err := ParseVersion(versionText)
		if err != nil {
			return fmt.Errorf("已安装包 %s 版本 %q 非法: %w", dep.ID, versionText, err)
		}
		constraint, err := ParseConstraint(dep.Constraint)
		if err != nil {
			return fmt.Errorf("依赖约束 %s@%s 非法: %w", dep.ID, dep.Constraint, err)
		}
		if !constraint.Matches(candidate) {
			missing = append(missing, fmt.Sprintf("%s@%s (已安装 %s)", dep.ID, dep.Constraint, versionText))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少依赖: %s", strings.Join(missing, "; "))
	}
	return nil
}

// validateDependents 拒绝会破坏已安装直接依赖者的卸载或升级。
func validateDependents(ctx context.Context, root, id, replacement string) error {
	installed, err := ListInstalled(ctx, root)
	if err != nil {
		return err
	}
	var replacementVersion Version
	if replacement != "" {
		replacementVersion, err = ParseVersion(replacement)
		if err != nil {
			return err
		}
	}
	for _, record := range installed {
		if record.Manifest.ID == id {
			continue
		}
		for _, dependency := range record.Manifest.Dependencies {
			if dependency.ID != id {
				continue
			}
			if replacement == "" {
				return fmt.Errorf("无法变更包 %s：已安装包 %s 依赖它", id, record.Manifest.ID)
			}
			constraint, err := ParseConstraint(dependency.Constraint)
			if err != nil || !constraint.Matches(replacementVersion) {
				return fmt.Errorf("无法升级包 %s：已安装包 %s 的依赖约束不再满足", id, record.Manifest.ID)
			}
		}
	}
	return nil
}

// copyFile 复制文件并保留源权限位。
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	dest, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dest, source); err != nil {
		return errors.Join(err, dest.Close())
	}
	if err := dest.Sync(); err != nil {
		return errors.Join(err, dest.Close())
	}
	return dest.Close()
}
