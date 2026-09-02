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

const maxResolvePasses = 64

var (
	ErrResolutionFailed = errors.New("project dependency resolution failed")
)

// Sync 解析项目 ailuo.toml 的完整依赖闭包，构建并安装所有缺失包，然后原子
// 更新同目录的 ailuo.lock。解析失败、约束冲突、来源不一致或安装包漂移都会
// 直接返回错误，绝不从当前安装集合猜测版本。
func Sync(ctx context.Context, projectFile, installRoot string, client *packmgr.GitHubClient) (projectcontract.Lock, error) {
	projectFile, err := filepath.Abs(projectFile)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	projectDir := filepath.Dir(projectFile)
	installRoot, err = filepath.Abs(installRoot)
	if err != nil || filepath.Clean(installRoot) != installRoot {
		return projectcontract.Lock{}, packagecontract.ErrInvalidFormat
	}
	manifest, err := packagefmt.ParseProject(projectFile)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	manifestDigest, err := packageio.HashFile(ctx, projectFile, packagecontract.MaxManifestBytes)
	if err != nil {
		return projectcontract.Lock{}, fmt.Errorf("读取项目清单摘要失败: %w", err)
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
	for _, id := range order {
		if err := installCandidate(ctx, installRoot, candidates[id]); err != nil {
			return projectcontract.Lock{}, err
		}
	}
	lock, err := buildLock(ctx, installRoot, manifest, manifestDigest, candidates)
	if err != nil {
		return projectcontract.Lock{}, err
	}
	lockPath := filepath.Join(projectDir, "ailuo.lock")
	if err := writeLock(lockPath, lock); err != nil {
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
		constraints := sortedStrings(merged.constraints)
		fingerprint := ref.key + "\x00" + strings.Join(constraints, ",")
		if processed[item.id] == fingerprint {
			continue
		}
		processed[item.id] = fingerprint
		resolved, err := r.load(ctx, item.id, ref, constraints)
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

func (r *resolver) load(ctx context.Context, id string, source sourceRef, constraints []string) (candidate, error) {
	cacheKey := source.key + "\x00" + strings.Join(constraints, ",")
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
		version := ""
		if _, err := os.Stat(packagefmt.SourcePath(source.path)); errors.Is(err, fs.ErrNotExist) {
			version, err = exactVersion(constraints)
			if err != nil {
				return candidate{}, err
			}
		} else if err != nil {
			return candidate{}, err
		}
		var err error
		manifest, manifestBytes, err = packagefmt.Resolve(ctx, source.path, version)
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
	temporary, err := os.CreateTemp(directory, ".ailuo-lock-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
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

func sortedStrings(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func exactVersion(constraints []string) (string, error) {
	if len(constraints) == 0 {
		return "", fmt.Errorf("%w: 零声明包缺少版本约束", ErrResolutionFailed)
	}
	var exact string
	for _, raw := range constraints {
		version, err := packagecontract.ParseVersion(raw)
		if err != nil {
			return "", fmt.Errorf("%w: 零声明包必须使用同一个精确版本，收到 %q", ErrResolutionFailed, raw)
		}
		if exact == "" {
			exact = version.String()
		} else if exact != version.String() {
			return "", fmt.Errorf("%w: 零声明包存在冲突版本 %s 与 %s", ErrResolutionFailed, exact, version)
		}
	}
	return exact, nil
}
