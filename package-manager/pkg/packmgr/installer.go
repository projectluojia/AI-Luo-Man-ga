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
	"runtime"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
)

// InstalledRecord 是安装操作返回的已安装包记录；目录读取由公共契约层的
// packageio 负责。
type InstalledRecord = packageio.InstalledRecord

// Inspect 校验一个已打包源的 manifest 与组件工件，但不修改安装根目录。
// 项目解析器用它读取远端 tarball 的依赖闭包；最终安装仍必须再次经过 Install
// 的完整原子发布路径。
func Inspect(ctx context.Context, sourcePath string) (packagecontract.Manifest, []byte, error) {
	sourceDir, cleanup, err := unpackSource(sourcePath)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	source, err := readManifest(sourceDir)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	if err := validatePackagedSource(ctx, sourceDir, source); err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	if _, err := readSourceArtifacts(ctx, sourceDir, source.Manifest); err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	return source.Manifest, source.manifestBytes, nil
}

// Install 从源包目录或发布 tarball 安装到安装根目录（以整个 Package 为单位）：
// 校验源（manifest + 每组件 entrypoint 工件）、解析依赖、原子发布
// manifest+lock+全部组件工件，并回读验证。目标已有同 ID 且版本不同时替换；
// 版本相同且内容相同则幂等返回，否则拒绝覆盖已发布内容。
func Install(ctx context.Context, root, sourcePath string) (record InstalledRecord, err error) {
	if root == "" {
		return InstalledRecord{}, packagecontract.ErrInvalidFormat
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.MkdirAll(filepath.Dir(absoluteRoot), 0o750); err != nil {
		return InstalledRecord{}, err
	}
	rootLock := packageio.InstallRootLock(absoluteRoot)
	rootLock.Lock()
	defer rootLock.Unlock()
	fileLock, err := packageio.AcquireFileLock(ctx, packageio.InstallRootLockPath(absoluteRoot))
	if err != nil {
		return InstalledRecord{}, err
	}
	defer func() { err = errors.Join(err, fileLock.Close()) }()
	return install(ctx, root, sourcePath, "", "")
}

func install(ctx context.Context, root, sourcePath, expectedID, expectedVersion string) (InstalledRecord, error) {
	if root == "" {
		return InstalledRecord{}, packagecontract.ErrInvalidFormat
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
	source, err := readManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := validatePackagedSource(ctx, sourceDir, source); err != nil {
		return InstalledRecord{}, err
	}
	if (expectedID != "" && source.Manifest.ID != expectedID) ||
		(expectedVersion != "" && source.Manifest.Version != expectedVersion) {
		return InstalledRecord{}, fmt.Errorf("源包身份在校验后发生变化")
	}
	artifacts, err := readSourceArtifacts(ctx, sourceDir, source.Manifest)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return InstalledRecord{}, err
	}
	if err := packageio.RecoverInstallRoot(ctx, root); err != nil {
		return InstalledRecord{}, err
	}
	if err := resolveDependencies(ctx, root, source.Manifest.Dependencies); err != nil {
		return InstalledRecord{}, err
	}
	installLock := packageio.InstallLock(root, source.Manifest.ID)
	installLock.Lock()
	defer installLock.Unlock()
	targetDir := filepath.Join(root, source.Manifest.ID)
	_, targetErr := os.Lstat(targetDir)
	switch {
	case targetErr == nil:
		existing, err := packageio.ReadInstalled(ctx, targetDir)
		if err != nil {
			return InstalledRecord{}, fmt.Errorf("已安装包 %s 校验失败: %w", source.Manifest.ID, err)
		}
		if existing.Manifest.Version == source.Manifest.Version {
			same, err := sameInstalledPackage(ctx, existing, source.manifestBytes, artifacts)
			if err != nil {
				return InstalledRecord{}, err
			}
			if same {
				return existing, nil
			}
			return InstalledRecord{}, fmt.Errorf("包 %s@%s 已安装且内容不一致", source.Manifest.ID, source.Manifest.Version)
		}
		if err := validateDependents(ctx, root, source.Manifest.ID, source.Manifest.Version); err != nil {
			return InstalledRecord{}, err
		}
	case !errors.Is(targetErr, os.ErrNotExist):
		return InstalledRecord{}, fmt.Errorf("检查已安装包 %s 失败: %w", source.Manifest.ID, targetErr)
	}
	// 原子发布：先在安装根内创建临时阶段目录，再 rename 为目标。
	stageDir, err := os.MkdirTemp(root, packageio.StagePrefix)
	if err != nil {
		return InstalledRecord{}, err
	}
	defer func() { _ = os.RemoveAll(stageDir) }()
	// 写入 manifest（保留源字节，lock 中的 manifest digest 必须一致）。
	if err := os.WriteFile(filepath.Join(stageDir, "manifest.json"), source.manifestBytes, 0o640); err != nil {
		return InstalledRecord{}, err
	}
	// 复制每组件工件、计算哈希、为 isolated 组件生成安装期进程规格。
	lockedArtifacts := make([]packagecontract.LockedArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifactName := filepath.Base(artifact.path)
		stageArtifact := filepath.Join(stageDir, artifactName)
		if err := copyArtifact(artifact.path, stageArtifact); err != nil {
			return InstalledRecord{}, err
		}
		stageDigest, err := packageio.HashArtifact(ctx, stageArtifact, packagecontract.MaxArtifactBytes)
		if err != nil {
			return InstalledRecord{}, err
		}
		if stageDigest != artifact.digest {
			return InstalledRecord{}, fmt.Errorf("组件 %s 工件在安装期间发生变化", artifact.componentID)
		}
		locked := packagecontract.LockedArtifact{
			ComponentID: artifact.componentID,
			Path:        filepath.Join(targetDir, artifactName),
			SHA256:      stageDigest,
		}
		if component, ok := packagecontract.FindComponent(source.Manifest, artifact.componentID); ok && component.Mode == packagecontract.ModeIsolated {
			locked.Process, err = resolveProcessSpec(component, filepath.Join(targetDir, artifactName), targetDir, artifact.directory)
			if err != nil {
				return InstalledRecord{}, err
			}
		}
		lockedArtifacts = append(lockedArtifacts, locked)
	}
	// 写入 lock。
	manifestDigest := sha256.Sum256(source.manifestBytes)
	lockBytes, err := json.Marshal(packagecontract.Lock{
		SchemaVersion: packagecontract.SchemaVersion, PackageID: source.Manifest.ID,
		PackageVersion: source.Manifest.Version,
		ManifestSHA256: hex.EncodeToString(manifestDigest[:]),
		Artifacts:      lockedArtifacts,
	})
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "lock.json"), lockBytes, 0o640); err != nil {
		return InstalledRecord{}, err
	}
	// 原子发布 + 回滚：旧版本先移到备份目录而非直接删除，rename 或发布后回读
	// 失败都恢复原安装，绝不留下"包消失"或"目录无效"的中间态。
	return publishStage(ctx, root, targetDir, stageDir)
}

// publishStage 把阶段目录发布为安装目录并回读验证。旧安装先 rename 为安装根内
// 的备份目录：任一步失败都把备份恢复回原位，成功后才删除备份。
// （Windows 不支持 rename 覆盖已存在目录，因此走"移走旧的→移入新的"两步。）
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
	record, err := packageio.ReadInstalled(ctx, targetDir)
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

// reserveBackupDir 把已存在的安装目录移到安装根内的备份目录并返回其路径；
// 目标原本不存在时返回空字符串（无需备份，失败时直接删除新目录即可）。
func reserveBackupDir(root, targetDir string) (string, error) {
	if _, err := os.Lstat(targetDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	// MkdirTemp 只用于取唯一名字；rename 要求目标不存在，因此先删掉空目录。
	backupDir, err := os.MkdirTemp(root, packageio.BackupPrefix)
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

// resolveProcessSpec 把 isolated 组件的相对进程模板解析为安装期绝对规格。
func resolveProcessSpec(component packagecontract.Component, artifactPath, packageDir string, artifactDirectory bool) (*packagecontract.ProcessSpec, error) {
	if component.Process == nil {
		return nil, packagecontract.ErrInvalidFormat
	}
	artifactRoot := packageDir
	if artifactDirectory {
		artifactRoot = artifactPath
	}
	// 文件工件只会复制并锁定 entrypoint；其他进程路径没有对应工件，
	// 既不能启动也不能通过 ValidateLock 的路径闭合校验。
	if !artifactDirectory && component.Process.Path != component.Entrypoint {
		return nil, packagecontract.ErrInvalidFormat
	}
	executable, err := resolveProcessPath(artifactRoot, component.Process.Path)
	if err != nil {
		return nil, err
	}
	process := &packagecontract.ProcessSpec{Path: executable, WorkDir: artifactRoot, Address: component.Process.Address}
	process.Args = append([]string(nil), component.Process.Args...)
	process.WorkDir = artifactRoot
	if component.Process.WorkDir != "" {
		process.WorkDir = filepath.Join(artifactRoot, filepath.FromSlash(component.Process.WorkDir))
	}
	for index, argument := range process.Args {
		process.Args[index] = strings.ReplaceAll(argument, "${address}", process.Address)
	}
	return process, nil
}

// resolveProcessPath 解析模板中的包内可执行路径；.venv/python 是跨平台的
// Python 虚拟环境入口，其他路径按组件工件根目录的普通相对路径处理。
func resolveProcessPath(artifactRoot, relative string) (string, error) {
	if relative == ".venv/python" {
		if runtime.GOOS == "windows" {
			return filepath.Join(artifactRoot, ".venv", "Scripts", "python.exe"), nil
		}
		return filepath.Join(artifactRoot, ".venv", "bin", "python"), nil
	}
	path := filepath.Join(artifactRoot, filepath.FromSlash(relative))
	if !artifactPathWithin(artifactRoot, path) {
		return "", packagecontract.ErrInvalidFormat
	}
	return path, nil
}

// Upgrade 要求包已安装且源包版本号不同，然后安装。
func Upgrade(ctx context.Context, root, id, sourcePath string) (record InstalledRecord, err error) {
	if root == "" || !capability.IsStableID(id) {
		return InstalledRecord{}, fmt.Errorf("包 %q 标识非法", id)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return InstalledRecord{}, err
	}
	if err := os.MkdirAll(filepath.Dir(absoluteRoot), 0o750); err != nil {
		return InstalledRecord{}, err
	}
	rootLock := packageio.InstallRootLock(absoluteRoot)
	rootLock.Lock()
	defer rootLock.Unlock()
	fileLock, err := packageio.AcquireFileLock(ctx, packageio.InstallRootLockPath(absoluteRoot))
	if err != nil {
		return InstalledRecord{}, err
	}
	defer func() { err = errors.Join(err, fileLock.Close()) }()
	if err := packageio.RecoverInstallRoot(ctx, absoluteRoot); err != nil {
		return InstalledRecord{}, err
	}
	existing, err := packageio.ReadInstalled(ctx, filepath.Join(absoluteRoot, id))
	if err != nil {
		return InstalledRecord{}, fmt.Errorf("包 %q 未安装", id)
	}
	sourceDir, cleanup, err := unpackSource(sourcePath)
	if err != nil {
		return InstalledRecord{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	source, err := readManifest(sourceDir)
	if err != nil {
		return InstalledRecord{}, err
	}
	if source.Manifest.ID != id {
		return InstalledRecord{}, fmt.Errorf("源目录包 ID %q 与升级目标 %q 不一致", source.Manifest.ID, id)
	}
	if source.Manifest.Version == existing.Manifest.Version {
		return InstalledRecord{}, fmt.Errorf("包 %s 已安装版本 %s，升级目标版本相同", id, existing.Manifest.Version)
	}
	return install(ctx, root, sourceDir, source.Manifest.ID, source.Manifest.Version)
}

// Uninstall 删除已安装包目录。仅当目录包含 manifest.json 时删除（安全防护）。
func Uninstall(ctx context.Context, root, id string) (err error) {
	if root == "" || !capability.IsStableID(id) {
		return fmt.Errorf("包 %q 标识非法", id)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absoluteRoot), 0o750); err != nil {
		return err
	}
	rootLock := packageio.InstallRootLock(absoluteRoot)
	rootLock.Lock()
	defer rootLock.Unlock()
	fileLock, err := packageio.AcquireFileLock(ctx, packageio.InstallRootLockPath(absoluteRoot))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, fileLock.Close()) }()
	if err := os.MkdirAll(absoluteRoot, 0o750); err != nil {
		return err
	}
	if err := packageio.RecoverInstallRoot(ctx, absoluteRoot); err != nil {
		return err
	}
	lock := packageio.InstallLock(absoluteRoot, id)
	lock.Lock()
	defer lock.Unlock()
	target := filepath.Join(absoluteRoot, id)
	relative, err := filepath.Rel(absoluteRoot, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return packagecontract.ErrInvalidFormat
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("包 %q 未安装", id)
	}
	if err != nil {
		return fmt.Errorf("读取 %q 失败: %w", target, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q 不是目录，不是安装包", target)
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

// manifestFile 是包目录读取结果：清单字节原样保留以锁定 digest。
type manifestFile struct {
	Manifest      packagecontract.Manifest
	manifestBytes []byte
}

// sourceArtifact 是源包中单个组件的工件。
type sourceArtifact struct {
	componentID string
	path        string
	directory   bool
	digest      string
}

// readManifest 读取包目录的 manifest.json 并校验。
func readManifest(directory string) (manifestFile, error) {
	manifestBytes, err := packageio.ReadFileLimited(filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
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

// readSourceArtifacts 校验并收集清单声明的每组件文件或目录工件。安装与打包
// 都按 basename 平铺工件，因此这里拒绝 basename 冲突，避免组件互相覆盖。
func readSourceArtifacts(ctx context.Context, sourceDir string, manifest packagecontract.Manifest) ([]sourceArtifact, error) {
	artifacts := make([]sourceArtifact, 0, len(manifest.Components))
	ownerByName := make(map[string]string, len(manifest.Components))
	for _, component := range manifest.Components {
		artifactPath := filepath.Join(sourceDir, component.Entrypoint)
		info, err := os.Lstat(artifactPath)
		if err != nil {
			return nil, fmt.Errorf("组件 %s entrypoint 工件不可读: %w", component.ID, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return nil, fmt.Errorf("组件 %s entrypoint 工件类型不受支持", component.ID)
		}
		digest, err := packageio.HashArtifact(ctx, artifactPath, packagecontract.MaxArtifactBytes)
		if err != nil {
			return nil, fmt.Errorf("组件 %s entrypoint 工件无效: %w", component.ID, err)
		}
		name := filepath.Base(artifactPath)
		if owner, exists := ownerByName[name]; exists {
			return nil, fmt.Errorf("%w: 组件 %s 与 %s 的 entrypoint 同名 %q（工件按 basename 平铺，不可冲突）",
				packagecontract.ErrInvalidFormat, owner, component.ID, name)
		}
		ownerByName[name] = component.ID
		artifacts = append(artifacts, sourceArtifact{
			componentID: component.ID, path: artifactPath, directory: info.IsDir(), digest: digest,
		})
	}
	return artifacts, nil
}

// sameInstalledPackage 判断同版本安装是否是同一份发布内容。版本一旦发布，
// 同版本不同清单或工件必须拒绝覆盖；完全相同则允许 sync 幂等重放。
func sameInstalledPackage(ctx context.Context, existing InstalledRecord, manifestBytes []byte, artifacts []sourceArtifact) (bool, error) {
	manifestDigest := sha256.Sum256(manifestBytes)
	if existing.Lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) || len(existing.Lock.Artifacts) != len(artifacts) {
		return false, nil
	}
	lockedByID := make(map[string]packagecontract.LockedArtifact, len(existing.Lock.Artifacts))
	for _, artifact := range existing.Lock.Artifacts {
		lockedByID[artifact.ComponentID] = artifact
	}
	for _, artifact := range artifacts {
		locked, ok := lockedByID[artifact.componentID]
		if !ok || locked.SHA256 != artifact.digest {
			return false, nil
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	return true, nil
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
		_ = os.RemoveAll(temp)
		return "", nil, err
	}
	return temp, func() { _ = os.RemoveAll(temp) }, nil
}

func validatePackagedSource(ctx context.Context, sourceDir string, source manifestFile) error {
	lockPath := filepath.Join(sourceDir, "lock.json")
	if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	lockBytes, err := packageio.ReadFileLimited(lockPath, packagecontract.MaxLockBytes)
	if err != nil {
		return err
	}
	var lock packagecontract.Lock
	if err := packagecontract.DecodeStrictJSON(lockBytes, &lock); err != nil {
		return err
	}
	manifestDigest := sha256.Sum256(source.manifestBytes)
	if lock.ManifestSHA256 != hex.EncodeToString(manifestDigest[:]) {
		return packagecontract.ErrInvalidFormat
	}
	for index := range lock.Artifacts {
		if !packagecontract.IsPackagePath(lock.Artifacts[index].Path) || lock.Artifacts[index].Path == "." {
			return packagecontract.ErrInvalidFormat
		}
		lock.Artifacts[index].Path = filepath.Join(sourceDir, lock.Artifacts[index].Path)
	}
	if err := validatePackagedLock(lock, source.Manifest); err != nil {
		return err
	}
	return packageio.VerifyInstalledArtifacts(ctx, sourceDir, lock.Artifacts)
}

// resolveDependencies 检查每个依赖在安装根内存在已安装包且版本满足约束。
func resolveDependencies(ctx context.Context, root string, deps []packagecontract.Dependency) error {
	if len(deps) == 0 {
		return nil
	}
	installed, err := packageio.ListInstalled(ctx, root)
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
		candidate, err := packagecontract.ParseVersion(versionText)
		if err != nil {
			return fmt.Errorf("已安装包 %s 版本 %q 非法: %w", dep.ID, versionText, err)
		}
		constraint, err := packagecontract.ParseConstraint(dep.Constraint)
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
	installed, err := packageio.ListInstalled(ctx, root)
	if err != nil {
		return err
	}
	var replacementVersion packagecontract.Version
	if replacement != "" {
		replacementVersion, err = packagecontract.ParseVersion(replacement)
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
			constraint, err := packagecontract.ParseConstraint(dependency.Constraint)
			if err != nil || !constraint.Matches(replacementVersion) {
				return fmt.Errorf("无法升级包 %s：已安装包 %s 的依赖约束不再满足", id, record.Manifest.ID)
			}
		}
	}
	return nil
}

// copyFile 复制文件并保留源权限位。写入路径必须 Sync + 检查 Close：ENOSPC 这类
// 错误只在 flush/close 时才浮出来，丢掉它会让一个被截断的工件照样通过后续哈希
// 并被写进 lock（自洽但内容错误）。
func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
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
