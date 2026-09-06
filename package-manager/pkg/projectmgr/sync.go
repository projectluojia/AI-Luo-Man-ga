// Package projectmgr 负责项目级依赖解析与同步。它只依赖公共契约、作者侧源
// 格式和包管理器，不依赖 AI珞 Core。
package projectmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packmgr"
)

const (
	maxResolvePasses      = 64
	maxResolvedPackages   = 256
	maxQueuedRequirements = 4096
)

var (
	ErrResolutionFailed = errors.New("project dependency resolution failed")
)

// Sync 按项目锁复现完整依赖闭包，构建并安装所有缺失包，然后原子更新同目录的
// ailuo.lock。需要主动重新求解时调用 SyncWithOptions 的 Update=true。
func Sync(ctx context.Context, projectFile, installRoot string, client *packmgr.GitHubClient) (projectcontract.Lock, error) {
	return SyncWithOptions(ctx, projectFile, installRoot, client, SyncOptions{})
}

// SyncOptions 控制项目依赖同步。Update 必须由调用方显式选择，表示允许重新
// 选择版本并接受本地开发源的内容更新；默认同步严格复用项目锁。
type SyncOptions struct {
	Update bool
}

// SyncWithOptions 解析项目 ailuo.toml 的完整依赖闭包并原子发布安装根与项目锁。
// 解析失败、约束冲突、来源不一致或锁定内容漂移都会直接返回错误。
func SyncWithOptions(ctx context.Context, projectFile, installRoot string, client *packmgr.GitHubClient, options SyncOptions) (result projectcontract.Lock, err error) {
	if installRoot == "" {
		return projectcontract.Lock{}, packagecontract.ErrInvalidFormat
	}
	projectFile, err = filepath.Abs(projectFile)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	projectDir := filepath.Dir(projectFile)
	installRoot, err = filepath.Abs(installRoot)
	if err != nil || filepath.Clean(installRoot) != installRoot {
		return projectcontract.Lock{}, packagecontract.ErrInvalidFormat
	}
	lockPath := filepath.Join(projectDir, "ailuo.lock")
	projectLock := packageio.ProjectLock(lockPath)
	projectLock.Lock()
	defer projectLock.Unlock()
	rootLock := packageio.InstallRootLock(installRoot)
	rootLock.Lock()
	defer rootLock.Unlock()
	if err := os.MkdirAll(filepath.Dir(installRoot), 0o750); err != nil {
		return projectcontract.Lock{}, err
	}
	fileLock, err := packageio.AcquireFileLock(ctx, packageio.InstallRootLockPath(installRoot))
	if err != nil {
		return projectcontract.Lock{}, err
	}
	defer func() { err = errors.Join(err, fileLock.Close()) }()
	transactionPath := filepath.Join(projectDir, ".ailuo-sync-transaction.json")
	if err := recoverSyncTransaction(ctx, transactionPath, installRoot, lockPath); err != nil {
		return projectcontract.Lock{}, err
	}
	manifest, err := packagefmt.ParseProject(projectFile)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	manifestDigest, err := packageio.HashFile(ctx, projectFile, packagecontract.MaxManifestBytes)
	if err != nil {
		return projectcontract.Lock{}, fmt.Errorf("读取项目清单摘要失败: %w", err)
	}
	previousLock, err := readPreviousLock(lockPath, manifest.ID)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	tempDir, err := os.MkdirTemp("", "ailuo-sync-")
	if err != nil {
		return projectcontract.Lock{}, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	resolver := &resolver{
		projectDir: projectDir,
		tempDir:    tempDir,
		client:     client,
		cache:      make(map[string]candidate),
		locked:     previousLock,
	}
	if options.Update {
		resolver.locked = map[string]projectcontract.LockedPackage{}
	}
	candidates, err := resolver.resolve(ctx, manifest.Dependencies)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	order, err := dependencyOrder(candidates)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	if err := os.MkdirAll(installRoot, 0o750); err != nil {
		return projectcontract.Lock{}, err
	}
	if err := packageio.RecoverInstallRoot(ctx, installRoot); err != nil {
		return projectcontract.Lock{}, err
	}
	installed, err := packageio.ListInstalled(ctx, installRoot)
	if err != nil {
		return projectcontract.Lock{}, fmt.Errorf("读取当前安装闭包失败: %w", err)
	}
	installedByID := make(map[string]packageio.InstalledRecord, len(installed))
	for _, record := range installed {
		installedByID[record.Manifest.ID] = record
	}
	// 先在安装根同级构造完整候选闭包。安装根是项目锁的物化视图，
	// 因而提交时整体替换，而不是把多个包逐个暴露给运行时。
	parent := filepath.Dir(installRoot)
	stageRoot, err := os.MkdirTemp(parent, ".ailuo-stage-")
	if err != nil {
		return projectcontract.Lock{}, err
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()
	for _, id := range order {
		if err := installCandidate(ctx, stageRoot, candidates[id]); err != nil {
			return projectcontract.Lock{}, err
		}
	}
	candidateIDs := make(map[string]struct{}, len(candidates))
	for id := range candidates {
		candidateIDs[id] = struct{}{}
	}
	// 安装根可能被多个项目共享；候选闭包之外的已校验包保留，但只复制
	// manifest、lock 和 lock 引用的工件，不把未知文件带入新安装根。
	for _, record := range installed {
		if _, selected := candidateIDs[record.Manifest.ID]; selected {
			continue
		}
		if err := copyInstalledPackage(ctx, record, installRoot, stageRoot); err != nil {
			return projectcontract.Lock{}, err
		}
	}
	if err := validateSameVersionContent(ctx, stageRoot, installedByID, candidates); err != nil {
		return projectcontract.Lock{}, err
	}
	if !options.Update {
		if err := validatePinnedContent(ctx, stageRoot, previousLock, candidates); err != nil {
			return projectcontract.Lock{}, err
		}
	}
	published, err := reserveInstallRootPublication(installRoot)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	transaction, err := newSyncTransaction(transactionPath, lockPath, installRoot, stageRoot, published.backup)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	if err := transaction.write("prepared"); err != nil {
		_ = transaction.cleanup()
		return projectcontract.Lock{}, err
	}
	if err := os.Rename(installRoot, published.backup); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	published.backedUp = true
	if err := transaction.write("root_backed_up"); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	if err := os.Rename(stageRoot, installRoot); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	published.published = true
	if err := transaction.write("root_published"); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	allIDs := make([]string, 0, len(candidates)+len(installed))
	for id := range candidates {
		allIDs = append(allIDs, id)
	}
	for _, record := range installed {
		if _, selected := candidateIDs[record.Manifest.ID]; !selected {
			allIDs = append(allIDs, record.Manifest.ID)
		}
	}
	if err := rebaseInstalledLocks(ctx, stageRoot, installRoot, allIDs); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	lock, err := buildLock(ctx, installRoot, manifest, manifestDigest, candidates)
	if err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	if err := writeLock(lockPath, lock); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	if err := transaction.write("committed"); err != nil {
		return projectcontract.Lock{}, errors.Join(err, transaction.rollback(published))
	}
	if err := published.commit(); err != nil {
		return projectcontract.Lock{}, err
	}
	if err := transaction.cleanup(); err != nil {
		return projectcontract.Lock{}, err
	}
	return lock, nil
}

type candidate struct {
	manifest       packagecontract.Manifest
	manifestDigest string
	sourceKey      string
	installSource  string
	sourceDir      string
}

type sourceRef struct {
	key      string
	path     string
	registry string
}

type requirement struct {
	id         string
	constraint string
	source     string
	baseDir    string
}

type mergedRequirement struct {
	source      sourceRef
	constraints map[string]struct{}
}

type resolver struct {
	projectDir string
	tempDir    string
	client     *packmgr.GitHubClient
	cache      map[string]candidate
	locked     map[string]projectcontract.LockedPackage
}

func (r *resolver) resolve(ctx context.Context, roots []projectcontract.Dependency) (map[string]candidate, error) {
	previous := map[string]candidate{}
	for pass := 0; pass < maxResolvePasses; pass++ {
		current, changed, err := r.resolvePass(ctx, roots, previous)
		if err != nil {
			return nil, err
		}
		if !changed && sameCandidates(current, previous) {
			return current, nil
		}
		previous = current
	}
	return nil, fmt.Errorf("%w: 依赖图在 %d 轮后仍未稳定", ErrResolutionFailed, maxResolvePasses)
}

func (r *resolver) resolvePass(ctx context.Context, roots []projectcontract.Dependency, previous map[string]candidate) (map[string]candidate, bool, error) {
	queue := make([]requirement, 0, len(roots))
	for _, dependency := range roots {
		queue = append(queue, requirement{
			id: dependency.ID, constraint: dependency.Constraint, source: dependency.Source, baseDir: r.projectDir,
		})
	}
	requirements := make(map[string]mergedRequirement)
	processed := make(map[string]string)
	current := make(map[string]candidate)
	changed := false
	for index := 0; index < len(queue); index++ {
		if len(queue) > maxQueuedRequirements {
			return nil, false, fmt.Errorf("%w: 依赖声明数量超过上限", ErrResolutionFailed)
		}
		item := queue[index]
		ref, err := r.resolveSource(item.baseDir, item.source)
		if err != nil {
			return nil, false, err
		}
		merged, exists := requirements[item.id]
		if !exists {
			merged = mergedRequirement{source: ref, constraints: make(map[string]struct{})}
		} else if merged.source.key != ref.key {
			return nil, false, fmt.Errorf("%w: 包 %s 同时来自 %s 与 %s", ErrResolutionFailed, item.id, merged.source.key, ref.key)
		}
		merged.constraints[item.constraint] = struct{}{}
		requirements[item.id] = merged
		if len(requirements) > maxResolvedPackages {
			return nil, false, fmt.Errorf("%w: 依赖包数量超过上限 %d", ErrResolutionFailed, maxResolvedPackages)
		}
		constraints := sortedStrings(merged.constraints)
		fingerprint := ref.key + "\x00" + strings.Join(constraints, ",")
		if processed[item.id] == fingerprint {
			continue
		}
		processed[item.id] = fingerprint
		lockedVersion := ""
		if locked, ok := r.locked[item.id]; ok && locked.Source == ref.key && versionSatisfies(locked.Version, constraints) {
			lockedVersion = locked.Version
		}
		resolved, err := r.load(ctx, item.id, ref, constraints, lockedVersion)
		if err != nil {
			return nil, false, err
		}
		current[item.id] = resolved
		previousCandidate, hadPrevious := previous[item.id]
		if !hadPrevious || previousCandidate.sourceKey != resolved.sourceKey || previousCandidate.manifest.Version != resolved.manifest.Version {
			changed = true
		}
		for _, dependency := range resolved.manifest.Dependencies {
			baseDir := resolved.sourceDir
			if strings.HasPrefix(dependency.Source, "path:") && baseDir == "" {
				return nil, false, fmt.Errorf("%w: 远端包 %s 声明了无法解析的本地依赖 %s", ErrResolutionFailed, resolved.manifest.ID, dependency.ID)
			}
			queue = append(queue, requirement{
				id: dependency.ID, constraint: dependency.Constraint, source: dependency.Source, baseDir: baseDir,
			})
		}
	}
	if !sameCandidates(current, previous) {
		changed = true
	}
	return current, changed, nil
}

func (r *resolver) resolveSource(baseDir, source string) (sourceRef, error) {
	if strings.HasPrefix(source, "path:") {
		if baseDir == "" {
			return sourceRef{}, fmt.Errorf("%w: 本地依赖来源缺少基准目录", ErrResolutionFailed)
		}
		relative := strings.TrimPrefix(source, "path:")
		absolute, err := filepath.Abs(filepath.Join(baseDir, filepath.FromSlash(relative)))
		if err != nil {
			return sourceRef{}, err
		}
		projectRelative, err := filepath.Rel(r.projectDir, absolute)
		if err != nil || projectRelative == ".." || strings.HasPrefix(projectRelative, ".."+string(filepath.Separator)) {
			return sourceRef{}, fmt.Errorf("%w: 本地依赖路径逃逸项目目录", ErrResolutionFailed)
		}
		return sourceRef{
			key: "path:" + filepath.ToSlash(projectRelative), path: absolute,
		}, nil
	}
	if strings.HasPrefix(source, "github:") {
		if err := packagecontract.ValidateSource(source); err != nil {
			return sourceRef{}, fmt.Errorf("%w: GitHub 来源 %q 非法", ErrResolutionFailed, source)
		}
		return sourceRef{key: source, registry: strings.TrimPrefix(source, "github:")}, nil
	}
	return sourceRef{}, fmt.Errorf("%w: 不支持的依赖来源 %q", ErrResolutionFailed, source)
}

func (r *resolver) load(ctx context.Context, id string, source sourceRef, constraints []string, lockedVersion string) (candidate, error) {
	cacheKey := source.key + "\x00" + strings.Join(constraints, ",") + "\x00" + lockedVersion
	if cached, ok := r.cache[cacheKey]; ok {
		return cached, nil
	}
	var (
		manifest      packagecontract.Manifest
		manifestBytes []byte
		installSource string
		sourceDir     string
	)
	switch {
	case source.path != "":
		var err error
		manifest, manifestBytes, err = packagefmt.Resolve(ctx, source.path)
		if err != nil {
			return candidate{}, err
		}
		installSource, err = packmgr.PackFromSource(ctx, source.path, r.tempDir, manifest, manifestBytes)
		if err != nil {
			return candidate{}, err
		}
		sourceDir = source.path
	case source.registry != "":
		if r.client == nil {
			return candidate{}, fmt.Errorf("%w: 解析 %s 需要 GitHub 注册表客户端", ErrResolutionFailed, source.key)
		}
		parts := strings.Split(source.registry, "/")
		if len(parts) != 2 {
			return candidate{}, fmt.Errorf("%w: GitHub 来源 %q 非法", ErrResolutionFailed, source.key)
		}
		constraint := strings.Join(constraints, ",")
		if lockedVersion != "" {
			constraint = lockedVersion
		}
		version, assetURL, err := r.client.ResolveRelease(ctx, parts[0], parts[1], constraint)
		if err != nil {
			return candidate{}, err
		}
		file, err := os.CreateTemp(r.tempDir, "package-*.tgz")
		if err != nil {
			return candidate{}, err
		}
		installSource = file.Name()
		if err := file.Close(); err != nil {
			return candidate{}, err
		}
		if err := r.client.DownloadRelease(ctx, assetURL, installSource); err != nil {
			return candidate{}, err
		}
		manifest, manifestBytes, err = packmgr.Inspect(ctx, installSource)
		if err != nil {
			return candidate{}, err
		}
		resolvedVersion, err := packagecontract.ParseVersion(version)
		if err != nil || manifest.Version != resolvedVersion.String() {
			return candidate{}, fmt.Errorf("%w: %s 发布版本与 manifest 不一致", ErrResolutionFailed, source.key)
		}
	default:
		return candidate{}, fmt.Errorf("%w: 依赖来源为空", ErrResolutionFailed)
	}
	if manifest.ID != id {
		return candidate{}, fmt.Errorf("%w: 依赖声明 %s 实际得到包 %s", ErrResolutionFailed, id, manifest.ID)
	}
	version, err := packagecontract.ParseVersion(manifest.Version)
	if err != nil {
		return candidate{}, err
	}
	for _, rawConstraint := range constraints {
		constraint, err := packagecontract.ParseConstraint(rawConstraint)
		if err != nil || !constraint.Matches(version) {
			return candidate{}, fmt.Errorf("%w: 包 %s@%s 不满足约束 %q", ErrResolutionFailed, id, manifest.Version, rawConstraint)
		}
	}
	digest := sha256.Sum256(manifestBytes)
	result := candidate{
		manifest: manifest, manifestDigest: hex.EncodeToString(digest[:]), sourceKey: source.key,
		installSource: installSource, sourceDir: sourceDir,
	}
	r.cache[cacheKey] = result
	return result, nil
}

func dependencyOrder(candidates map[string]candidate) ([]string, error) {
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	state := make(map[string]uint8, len(candidates))
	order := make([]string, 0, len(candidates))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: 检测到包依赖环 %s", ErrResolutionFailed, id)
		case 2:
			return nil
		}
		candidate, ok := candidates[id]
		if !ok {
			return fmt.Errorf("%w: 锁定闭包缺少包 %s", ErrResolutionFailed, id)
		}
		state[id] = 1
		for _, dependency := range candidate.manifest.Dependencies {
			if err := visit(dependency.ID); err != nil {
				return err
			}
		}
		state[id] = 2
		order = append(order, id)
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func installCandidate(ctx context.Context, root string, candidate candidate) error {
	_, err := packmgr.Install(ctx, root, candidate.installSource)
	return err
}

type installRootPublication struct {
	root      string
	backup    string
	backedUp  bool
	published bool
}

// reserveInstallRootPublication 预留同一文件系统内的旧安装根备份路径。
// 实际 rename 由 Sync 在写入事务记录后执行。
func reserveInstallRootPublication(installRoot string) (installRootPublication, error) {
	parent := filepath.Dir(installRoot)
	publication := installRootPublication{root: installRoot}
	if _, err := os.Lstat(installRoot); err == nil {
		backup, err := os.MkdirTemp(parent, ".ailuo-root-backup-")
		if err != nil {
			return installRootPublication{}, err
		}
		if err := os.Remove(backup); err != nil {
			return installRootPublication{}, err
		}
		publication.backup = backup
	} else if !errors.Is(err, os.ErrNotExist) {
		return installRootPublication{}, err
	}
	return publication, nil
}

func (p installRootPublication) rollback() error {
	if p.root == "" {
		return nil
	}
	var result []error
	if p.published {
		if err := os.RemoveAll(p.root); err != nil {
			result = append(result, fmt.Errorf("删除候选安装根失败: %w", err))
		}
	}
	if p.backedUp && p.backup != "" {
		if err := os.Rename(p.backup, p.root); err != nil {
			result = append(result, fmt.Errorf("恢复旧安装根失败: %w", err))
		}
	}
	return errors.Join(result...)
}

func (p installRootPublication) commit() error {
	if p.backup == "" {
		return nil
	}
	if err := os.RemoveAll(p.backup); err != nil {
		return fmt.Errorf("清理旧安装根备份失败: %w", err)
	}
	return nil
}

type syncTransaction struct {
	JournalPath    string
	TransactionDir string
	LockPath       string
	LockBackupPath string
	HadLock        bool
	InstallRoot    string
	StageRoot      string
	BackupRoot     string
}

type syncJournal struct {
	SchemaVersion  uint32 `json:"schema_version"`
	TransactionDir string `json:"transaction_dir"`
	LockPath       string `json:"lock_path"`
	LockBackupPath string `json:"lock_backup_path,omitempty"`
	HadLock        bool   `json:"had_lock"`
	InstallRoot    string `json:"install_root"`
	StageRoot      string `json:"stage_root"`
	BackupRoot     string `json:"backup_root"`
	Phase          string `json:"phase"`
}

const syncJournalSchemaVersion = 1

func newSyncTransaction(journalPath, lockPath, installRoot, stageRoot, backupRoot string) (syncTransaction, error) {
	transactionDir, err := os.MkdirTemp(filepath.Dir(journalPath), ".ailuo-sync-transaction-")
	if err != nil {
		return syncTransaction{}, err
	}
	transaction := syncTransaction{
		JournalPath: journalPath, TransactionDir: transactionDir, LockPath: lockPath,
		InstallRoot: installRoot, StageRoot: stageRoot, BackupRoot: backupRoot,
		LockBackupPath: filepath.Join(transactionDir, "project-lock.backup"),
	}
	if _, err := os.Lstat(lockPath); err == nil {
		data, readErr := packageio.ReadFileLimited(lockPath, projectcontract.MaxLockBytes)
		if readErr != nil {
			_ = os.RemoveAll(transactionDir)
			return syncTransaction{}, readErr
		}
		if err := writeSyncedFile(transaction.LockBackupPath, data, 0o600); err != nil {
			_ = os.RemoveAll(transactionDir)
			return syncTransaction{}, err
		}
		transaction.HadLock = true
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(transactionDir)
		return syncTransaction{}, err
	}
	return transaction, nil
}

func (t syncTransaction) journal(phase string) syncJournal {
	return syncJournal{
		SchemaVersion: syncJournalSchemaVersion, TransactionDir: t.TransactionDir,
		LockPath: t.LockPath, LockBackupPath: t.LockBackupPath, HadLock: t.HadLock,
		InstallRoot: t.InstallRoot, StageRoot: t.StageRoot, BackupRoot: t.BackupRoot,
		Phase: phase,
	}
}

func (t syncTransaction) write(phase string) error {
	data, err := json.Marshal(t.journal(phase))
	if err != nil {
		return err
	}
	return writeJournalFile(t.JournalPath, data)
}

func (t syncTransaction) rollback(publication installRootPublication) error {
	var result []error
	if err := publication.rollback(); err != nil {
		result = append(result, err)
	}
	if publication.published {
		if err := restoreProjectLock(t.LockPath, t.LockBackupPath, t.HadLock); err != nil {
			result = append(result, err)
		}
	}
	if len(result) == 0 {
		if err := t.cleanup(); err != nil {
			result = append(result, err)
		}
	}
	return errors.Join(result...)
}

func (t syncTransaction) cleanup() error {
	var result []error
	if err := os.Remove(t.JournalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = append(result, fmt.Errorf("清理同步事务记录失败: %w", err))
	}
	if err := os.Remove(t.JournalPath + ".backup"); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = append(result, fmt.Errorf("清理同步事务记录备份失败: %w", err))
	}
	if len(result) == 0 {
		if err := os.RemoveAll(t.TransactionDir); err != nil {
			result = append(result, fmt.Errorf("清理同步事务目录失败: %w", err))
		}
	}
	return errors.Join(result...)
}

func writeJournalFile(path string, data []byte) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return writeSyncedFile(path, data, 0o600)
	} else if err != nil {
		return err
	}
	return replaceFile(path, data)
}

func writeSyncedFile(path string, data []byte, mode os.FileMode) error {
	temporaryPath, err := createSyncedTempFile(filepath.Dir(path), ".ailuo-sync-write-", data, mode)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	return os.Rename(temporaryPath, path)
}

func createSyncedTempFile(directory, pattern string, data []byte, mode os.FileMode) (string, error) {
	temporary, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(mode); err != nil {
		cleanup()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", err
	}
	return temporaryPath, nil
}

func restoreProjectLock(path, backupPath string, hadLock bool) error {
	if !hadLock {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除候选项目锁失败: %w", err)
		}
		return nil
	}
	data, err := packageio.ReadFileLimited(backupPath, projectcontract.MaxLockBytes)
	if err != nil {
		return fmt.Errorf("读取项目锁备份失败: %w", err)
	}
	return replaceFileIfPresent(path, data)
}

func replaceFileIfPresent(path string, data []byte) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeSyncedFile(path, data, 0o600)
}

func recoverSyncTransaction(ctx context.Context, journalPath, installRoot, lockPath string) error {
	journal, exists, err := readSyncJournal(journalPath)
	if err != nil || !exists {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSyncJournal(journal, filepath.Dir(journalPath), installRoot, lockPath); err != nil {
		return err
	}
	transaction := syncTransaction{
		JournalPath: journalPath, TransactionDir: journal.TransactionDir,
		LockPath: journal.LockPath, LockBackupPath: journal.LockBackupPath,
		HadLock: journal.HadLock, InstallRoot: journal.InstallRoot,
		StageRoot: journal.StageRoot, BackupRoot: journal.BackupRoot,
	}
	if journal.Phase == "committed" {
		if err := os.RemoveAll(journal.StageRoot); err != nil {
			return fmt.Errorf("清理已提交阶段安装根失败: %w", err)
		}
		if err := os.RemoveAll(journal.BackupRoot); err != nil {
			return fmt.Errorf("清理已提交安装根备份失败: %w", err)
		}
		return transaction.cleanup()
	}
	if journal.Phase != "prepared" && journal.Phase != "root_backed_up" && journal.Phase != "root_published" {
		return fmt.Errorf("%w: 同步事务阶段非法", ErrResolutionFailed)
	}
	var result []error
	if journal.Phase == "prepared" {
		backupExists := pathExists(journal.BackupRoot)
		rootExists := pathExists(journal.InstallRoot)
		if backupExists && !rootExists {
			if err := os.Rename(journal.BackupRoot, journal.InstallRoot); err != nil {
				result = append(result, fmt.Errorf("恢复准备阶段的安装根失败: %w", err))
			}
		} else if backupExists && rootExists {
			if err := os.RemoveAll(journal.InstallRoot); err != nil {
				result = append(result, fmt.Errorf("清理重复安装根失败: %w", err))
			} else if err := os.Rename(journal.BackupRoot, journal.InstallRoot); err != nil {
				result = append(result, fmt.Errorf("恢复准备阶段的安装根失败: %w", err))
			}
		}
	} else {
		if err := os.RemoveAll(journal.InstallRoot); err != nil {
			result = append(result, fmt.Errorf("删除未提交安装根失败: %w", err))
		}
		if err := os.Rename(journal.BackupRoot, journal.InstallRoot); err != nil {
			result = append(result, fmt.Errorf("恢复旧安装根失败: %w", err))
		}
	}
	if journal.Phase == "root_published" && len(result) == 0 {
		if err := restoreProjectLock(journal.LockPath, journal.LockBackupPath, journal.HadLock); err != nil {
			result = append(result, err)
		}
	}
	if len(result) == 0 {
		result = append(result, os.RemoveAll(journal.StageRoot))
		if err := transaction.cleanup(); err != nil {
			result = append(result, err)
		}
	}
	return errors.Join(result...)
}

func readSyncJournal(path string) (syncJournal, bool, error) {
	read := func(candidate string) ([]byte, bool, error) {
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		} else if err != nil {
			return nil, false, err
		}
		data, err := packageio.ReadFileLimited(candidate, packagecontract.MaxLockBytes)
		return data, true, err
	}
	data, exists, err := read(path)
	if err != nil {
		return syncJournal{}, false, err
	}
	if !exists {
		data, exists, err = read(path + ".backup")
		if err != nil {
			return syncJournal{}, false, err
		}
	}
	if !exists {
		return syncJournal{}, false, nil
	}
	var journal syncJournal
	if err := packagecontract.DecodeStrictJSON(data, &journal); err != nil {
		return syncJournal{}, false, fmt.Errorf("%w: 同步事务记录损坏", ErrResolutionFailed)
	}
	return journal, true, nil
}

func validateSyncJournal(journal syncJournal, projectDir, installRoot, lockPath string) error {
	if journal.SchemaVersion != syncJournalSchemaVersion || journal.InstallRoot != installRoot || journal.LockPath != lockPath ||
		journal.TransactionDir == "" || journal.StageRoot == "" || journal.BackupRoot == "" ||
		!filepath.IsAbs(journal.TransactionDir) || filepath.Clean(journal.TransactionDir) != journal.TransactionDir ||
		!filepath.IsAbs(journal.StageRoot) || filepath.Clean(journal.StageRoot) != journal.StageRoot ||
		!filepath.IsAbs(journal.BackupRoot) || filepath.Clean(journal.BackupRoot) != journal.BackupRoot ||
		(journal.HadLock && (journal.LockBackupPath == "" || !filepath.IsAbs(journal.LockBackupPath) || filepath.Clean(journal.LockBackupPath) != journal.LockBackupPath)) {
		return fmt.Errorf("%w: 同步事务记录路径非法", ErrResolutionFailed)
	}
	transactionRelative, err := filepath.Rel(projectDir, journal.TransactionDir)
	if err != nil || transactionRelative == "." || transactionRelative == ".." || strings.Contains(transactionRelative, string(filepath.Separator)) {
		return fmt.Errorf("%w: 同步事务目录越界", ErrResolutionFailed)
	}
	if journal.HadLock {
		lockRelative, err := filepath.Rel(journal.TransactionDir, journal.LockBackupPath)
		if err != nil || lockRelative == ".." || strings.HasPrefix(lockRelative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: 项目锁备份路径越界", ErrResolutionFailed)
		}
	}
	installParent := filepath.Dir(installRoot)
	for index, candidate := range []string{journal.StageRoot, journal.BackupRoot} {
		relative, err := filepath.Rel(installParent, candidate)
		if err != nil || relative == "." || relative == ".." || strings.Contains(relative, string(filepath.Separator)) {
			return fmt.Errorf("%w: 安装根事务路径越界", ErrResolutionFailed)
		}
		prefix := ".ailuo-stage-"
		if index == 1 {
			prefix = ".ailuo-root-backup-"
		}
		if !strings.HasPrefix(filepath.Base(candidate), prefix) {
			return fmt.Errorf("%w: 安装根事务路径前缀非法", ErrResolutionFailed)
		}
	}
	if !strings.HasPrefix(filepath.Base(journal.TransactionDir), ".ailuo-sync-transaction-") {
		return fmt.Errorf("%w: 同步事务目录前缀非法", ErrResolutionFailed)
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// validateSameVersionContent 保持已发布版本不可变。候选闭包在空安装根中
// 构造，因此不能直接依赖 packmgr.Install 的同目录版本检查，这里比较清单
// 摘要和规范化安装 lock 摘要。
func validateSameVersionContent(ctx context.Context, stageRoot string, installed map[string]packageio.InstalledRecord, candidates map[string]candidate) error {
	for id, candidate := range candidates {
		current, exists := installed[id]
		if !exists || current.Manifest.Version != candidate.manifest.Version {
			continue
		}
		staged, err := packageio.ReadInstalled(ctx, filepath.Join(stageRoot, id))
		if err != nil {
			return fmt.Errorf("候选包 %s 校验失败: %w", id, err)
		}
		if current.Lock.ManifestSHA256 != staged.Lock.ManifestSHA256 {
			return fmt.Errorf("包 %s@%s 已安装且内容不一致", id, candidate.manifest.Version)
		}
		currentLock, err := packageio.CanonicalLockDigest(ctx, current.Directory, current.Lock)
		if err != nil {
			return fmt.Errorf("读取已安装包 %s lock 摘要失败: %w", id, err)
		}
		stagedLock, err := packageio.CanonicalLockDigest(ctx, staged.Directory, staged.Lock)
		if err != nil {
			return fmt.Errorf("读取候选包 %s lock 摘要失败: %w", id, err)
		}
		if currentLock != stagedLock {
			return fmt.Errorf("包 %s@%s 已安装且内容不一致", id, candidate.manifest.Version)
		}
	}
	return nil
}

func copyInstalledPackage(ctx context.Context, record packageio.InstalledRecord, fromRoot, toRoot string) error {
	packageID := record.Manifest.ID
	targetDir := filepath.Join(toRoot, packageID)
	if _, err := os.Lstat(targetDir); err == nil {
		return fmt.Errorf("%w: 安装阶段出现重复包 %s", ErrResolutionFailed, packageID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		return err
	}
	for _, name := range []string{"manifest.json", "lock.json"} {
		if err := copyPackageFile(filepath.Join(record.Directory, name), filepath.Join(targetDir, name)); err != nil {
			return fmt.Errorf("复制包 %s %s 失败: %w", packageID, name, err)
		}
	}
	for _, artifact := range record.Lock.Artifacts {
		relative, err := filepath.Rel(record.Directory, artifact.Path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: 包 %s 工件路径逃逸", ErrResolutionFailed, packageID)
		}
		if err := copyPackageArtifact(ctx, artifact.Path, filepath.Join(targetDir, relative)); err != nil {
			return fmt.Errorf("复制包 %s 工件失败: %w", packageID, err)
		}
	}
	if err := rebaseLockFile(ctx, filepath.Join(targetDir, "lock.json"), fromRoot, toRoot, packageID); err != nil {
		return err
	}
	if _, err := packageio.ReadInstalled(ctx, targetDir); err != nil {
		return fmt.Errorf("复制后校验包 %s 失败: %w", packageID, err)
	}
	return nil
}

func copyPackageArtifact(ctx context.Context, source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
		return packagecontract.ErrInvalidFormat
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chmod(destination, info.Mode().Perm()); err != nil {
			return err
		}
		return filepath.WalkDir(source, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return packagecontract.ErrInvalidFormat
			}
			relative, err := filepath.Rel(source, current)
			if err != nil {
				return packagecontract.ErrInvalidFormat
			}
			if relative == "." {
				return nil
			}
			if packageio.IsIgnoredArtifactPath(relative) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			target := filepath.Join(destination, relative)
			entryInfo, err := entry.Info()
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if err := os.MkdirAll(target, entryInfo.Mode().Perm()); err != nil {
					return err
				}
				return os.Chmod(target, entryInfo.Mode().Perm())
			}
			if !entryInfo.Mode().IsRegular() {
				return packagecontract.ErrInvalidFormat
			}
			return copyPackageFile(current, target)
		})
	}
	return copyPackageFile(source, destination)
}

func copyPackageFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return packagecontract.ErrInvalidFormat
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		return errors.Join(err, output.Close())
	}
	if err := output.Sync(); err != nil {
		return errors.Join(err, output.Close())
	}
	return output.Close()
}

// rebaseInstalledLocks 把阶段安装根中的绝对路径改写为最终安装根路径。
// Package lock 为宿主执行保留绝对路径，因此目录级原子切换后必须先完成这
// 一步，再计算项目 lock 的规范化摘要。
func rebaseInstalledLocks(ctx context.Context, fromRoot, toRoot string, ids []string) error {
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(toRoot, id, "lock.json")
		if err := rebaseLockFile(ctx, path, fromRoot, toRoot, id); err != nil {
			return err
		}
	}
	return nil
}

func rebaseLockFile(ctx context.Context, path, fromRoot, toRoot, packageID string) error {
	data, err := packageio.ReadFileLimited(path, packagecontract.MaxLockBytes)
	if err != nil {
		return fmt.Errorf("读取包 %s lock 失败: %w", packageID, err)
	}
	var lock packagecontract.Lock
	if err := packagecontract.DecodeStrictJSON(data, &lock); err != nil {
		return fmt.Errorf("解析包 %s lock 失败: %w", packageID, err)
	}
	for index := range lock.Artifacts {
		artifact := &lock.Artifacts[index]
		artifact.Path, err = rebasePath(fromRoot, toRoot, artifact.Path, false)
		if err != nil {
			return fmt.Errorf("改写包 %s 工件路径失败: %w", packageID, err)
		}
		if artifact.Process == nil {
			continue
		}
		process := artifact.Process
		oldAddress := process.Address
		process.Path, err = rebasePath(fromRoot, toRoot, process.Path, false)
		if err != nil {
			return fmt.Errorf("改写包 %s 进程路径失败: %w", packageID, err)
		}
		process.WorkDir, err = rebasePath(fromRoot, toRoot, process.WorkDir, true)
		if err != nil {
			return fmt.Errorf("改写包 %s 工作目录失败: %w", packageID, err)
		}
		process.Address = rebaseAddress(fromRoot, toRoot, process.Address)
		if oldAddress != process.Address {
			for argumentIndex, argument := range process.Args {
				process.Args[argumentIndex] = strings.ReplaceAll(argument, oldAddress, process.Address)
			}
		}
	}
	encoded, err := json.Marshal(lock)
	if err != nil {
		return fmt.Errorf("序列化包 %s lock 失败: %w", packageID, err)
	}
	if err := replaceFile(path, encoded); err != nil {
		return fmt.Errorf("写入包 %s lock 失败: %w", packageID, err)
	}
	return nil
}

func rebasePath(fromRoot, toRoot, value string, allowRoot bool) (string, error) {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", packagecontract.ErrInvalidFormat
	}
	relative, err := filepath.Rel(fromRoot, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		(relative == "." && !allowRoot) {
		return "", packagecontract.ErrInvalidFormat
	}
	return filepath.Join(toRoot, relative), nil
}

func rebaseAddress(fromRoot, toRoot, address string) string {
	if !strings.HasPrefix(address, "unix:") {
		return address
	}
	socketPath := strings.TrimPrefix(address, "unix:")
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return address
	}
	relative, err := filepath.Rel(fromRoot, socketPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return address
	}
	return "unix:" + filepath.Join(toRoot, relative)
}

func replaceFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporaryPath, err := createSyncedTempFile(directory, ".ailuo-lock-", data, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	backup := path + ".backup"
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, backup); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if restoreErr := os.Rename(backup, path); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}
	if err := os.Remove(backup); err != nil {
		return err
	}
	return nil
}

func buildLock(ctx context.Context, root string, manifest projectcontract.Manifest, manifestDigest string, candidates map[string]candidate) (projectcontract.Lock, error) {
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	lock := projectcontract.Lock{
		SchemaVersion: projectcontract.SchemaVersion, ProjectID: manifest.ID,
		ProjectManifestSHA256: manifestDigest, Packages: make([]projectcontract.LockedPackage, 0, len(ids)),
	}
	for _, id := range ids {
		candidate := candidates[id]
		directory := filepath.Join(root, id)
		record, err := packageio.ReadInstalled(ctx, directory)
		if err != nil {
			return projectcontract.Lock{}, fmt.Errorf("同步后包 %s 校验失败: %w", id, err)
		}
		if record.Manifest.Version != candidate.manifest.Version {
			return projectcontract.Lock{}, fmt.Errorf("同步后包 %s 版本漂移", id)
		}
		manifestSHA, err := packageio.HashFile(ctx, filepath.Join(directory, "manifest.json"), packagecontract.MaxManifestBytes)
		if err != nil {
			return projectcontract.Lock{}, err
		}
		if manifestSHA != candidate.manifestDigest {
			return projectcontract.Lock{}, fmt.Errorf("同步后包 %s 清单摘要漂移", id)
		}
		lockSHA, err := packageio.CanonicalLockDigest(ctx, directory, record.Lock)
		if err != nil {
			return projectcontract.Lock{}, err
		}
		lock.Packages = append(lock.Packages, projectcontract.LockedPackage{
			ID: id, Version: candidate.manifest.Version, Source: candidate.sourceKey,
			ManifestSHA256: manifestSHA, LockSHA256: lockSHA,
		})
	}
	if err := projectcontract.ValidateLock(lock, manifest); err != nil {
		return projectcontract.Lock{}, err
	}
	return lock, nil
}

func writeLock(path string, lock projectcontract.Lock) error {
	data, err := json.Marshal(lock)
	if err != nil {
		return err
	}
	if int64(len(data)) > projectcontract.MaxLockBytes {
		return projectcontract.ErrInvalid
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	temporaryPath, err := createSyncedTempFile(directory, ".ailuo-lock-", data, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	backup := path + ".backup"
	if _, err := os.Lstat(backup); err == nil {
		return fmt.Errorf("%w: 上一次项目锁更新未完成，请先恢复 %s", ErrResolutionFailed, backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Rename(path, backup); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		if _, backupErr := os.Lstat(backup); backupErr == nil {
			if restoreErr := os.Rename(backup, path); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		return err
	}
	if _, err := os.Lstat(backup); err == nil {
		if err := os.Remove(backup); err != nil {
			return err
		}
	}
	return nil
}

func sameCandidates(left, right map[string]candidate) bool {
	if len(left) != len(right) {
		return false
	}
	for id, candidate := range left {
		other, ok := right[id]
		if !ok || candidate.sourceKey != other.sourceKey || candidate.manifest.Version != other.manifest.Version {
			return false
		}
	}
	return true
}

func readPreviousLock(path, projectID string) (map[string]projectcontract.LockedPackage, error) {
	if _, err := os.Lstat(path + ".backup"); err == nil {
		return nil, fmt.Errorf("%w: 项目锁存在未完成的更新备份 %s", ErrResolutionFailed, path+".backup")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return map[string]projectcontract.LockedPackage{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("检查项目锁失败: %w", err)
	}
	data, err := packageio.ReadFileLimited(path, projectcontract.MaxLockBytes)
	if err != nil {
		return nil, fmt.Errorf("读取项目锁失败: %w", err)
	}
	var lock projectcontract.Lock
	if err := packagecontract.DecodeStrictJSON(data, &lock); err != nil {
		return nil, fmt.Errorf("解析项目锁失败: %w", err)
	}
	if projectcontract.ValidateLockShape(lock) != nil || lock.ProjectID != projectID {
		return nil, fmt.Errorf("%w: 项目锁与当前项目不匹配", ErrResolutionFailed)
	}
	locked := make(map[string]projectcontract.LockedPackage, len(lock.Packages))
	for _, packageLock := range lock.Packages {
		locked[packageLock.ID] = packageLock
	}
	return locked, nil
}

func versionSatisfies(versionText string, constraints []string) bool {
	version, err := packagecontract.ParseVersion(versionText)
	if err != nil {
		return false
	}
	for _, rawConstraint := range constraints {
		constraint, err := packagecontract.ParseConstraint(rawConstraint)
		if err != nil || !constraint.Matches(version) {
			return false
		}
	}
	return true
}

// validatePinnedContent 实现 lock 的校验和语义：同一来源、同一版本若已
// 写入项目锁，重新下载/构建得到的 manifest 或安装 lock 必须完全一致。
func validatePinnedContent(ctx context.Context, root string, locked map[string]projectcontract.LockedPackage, candidates map[string]candidate) error {
	for id, candidate := range candidates {
		packageLock, ok := locked[id]
		if !ok || packageLock.Source != candidate.sourceKey || packageLock.Version != candidate.manifest.Version {
			continue
		}
		record, err := packageio.ReadInstalled(ctx, filepath.Join(root, id))
		if err != nil {
			return fmt.Errorf("读取锁定包 %s 失败: %w", id, err)
		}
		if record.Lock.ManifestSHA256 != packageLock.ManifestSHA256 {
			return fmt.Errorf("%w: 包 %s@%s 的清单摘要与项目锁不一致", ErrResolutionFailed, id, candidate.manifest.Version)
		}
		lockDigest, err := packageio.CanonicalLockDigest(ctx, record.Directory, record.Lock)
		if err != nil {
			return fmt.Errorf("读取锁定包 %s 安装摘要失败: %w", id, err)
		}
		if lockDigest != packageLock.LockSHA256 {
			return fmt.Errorf("%w: 包 %s@%s 的安装摘要与项目锁不一致", ErrResolutionFailed, id, candidate.manifest.Version)
		}
	}
	return nil
}

func sortedStrings(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
