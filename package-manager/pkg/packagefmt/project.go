package packagefmt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
)

// ProjectFileName 是项目根目录的依赖清单文件名。
const ProjectFileName = "ailuo.toml"

type projectSourceManifest struct {
	Project      projectSourceHeader                `toml:"project"`
	Dependencies map[string]projectSourceDependency `toml:"dependencies,omitempty"`
}

type projectSourceHeader struct {
	ID string `toml:"id"`
}

type projectSourceDependency struct {
	Version  string `toml:"version"`
	Path     string `toml:"path,omitempty"`
	Registry string `toml:"registry,omitempty"`
}

// ParseProject 读取项目根目录 ailuo.toml，转换为公共项目依赖清单并严格校验。
// 项目依赖使用 Cargo 风格的 [dependencies."package.id"] 表；path 与 registry
// 必须二选一，解析结果使用 projectcontract 的显式来源表示。
func ParseProject(path string) (projectcontract.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return projectcontract.Manifest{}, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	if int64(len(data)) > packagecontract.MaxManifestBytes {
		return projectcontract.Manifest{}, fmt.Errorf("%w: 项目清单超过大小上限", ErrSourceInvalid)
	}
	var source projectSourceManifest
	meta, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&source)
	if err != nil {
		return projectcontract.Manifest{}, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return projectcontract.Manifest{}, fmt.Errorf("%w: 未知字段 %s", ErrSourceInvalid, undecoded[0])
	}
	manifest := projectcontract.Manifest{
		SchemaVersion: projectcontract.SchemaVersion,
		ID:            source.Project.ID,
	}
	ids := make([]string, 0, len(source.Dependencies))
	for id := range source.Dependencies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		dependency, err := convertProjectDependency(path, id, source.Dependencies[id])
		if err != nil {
			return projectcontract.Manifest{}, err
		}
		manifest.Dependencies = append(manifest.Dependencies, dependency)
	}
	if err := projectcontract.ValidateManifest(manifest); err != nil {
		return projectcontract.Manifest{}, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	return manifest, nil
}

func convertProjectDependency(manifestPath, id string, source projectSourceDependency) (projectcontract.Dependency, error) {
	if !capability.IsStableID(id) || source.Version == "" {
		return projectcontract.Dependency{}, fmt.Errorf("%w: 依赖 %q 的 version 或 id 非法", ErrSourceInvalid, id)
	}
	if _, err := packagecontract.ParseConstraint(source.Version); err != nil {
		return projectcontract.Dependency{}, fmt.Errorf("%w: 依赖 %q 的 version 非法: %v", ErrSourceInvalid, id, err)
	}
	if (source.Path == "") == (source.Registry == "") {
		return projectcontract.Dependency{}, fmt.Errorf("%w: 依赖 %q 必须且只能声明 path 或 registry", ErrSourceInvalid, id)
	}
	resolvedSource := source.Registry
	if source.Path != "" {
		if !packagecontract.IsPackagePath(source.Path) || source.Path == "." {
			return projectcontract.Dependency{}, fmt.Errorf("%w: 依赖 %q 的 path 非法", ErrSourceInvalid, id)
		}
		projectDir := filepath.Dir(manifestPath)
		if _, err := ResolveLocalDependencyPath(projectDir, projectDir, source.Path); err != nil {
			return projectcontract.Dependency{}, fmt.Errorf("%w: 依赖 %q 的 path 无法安全解析: %v", ErrSourceInvalid, id, err)
		}
		resolvedSource = "path:" + source.Path
	}
	if err := packagecontract.ValidateSource(resolvedSource); err != nil {
		return projectcontract.Dependency{}, fmt.Errorf("%w: 依赖 %q 的来源非法", ErrSourceInvalid, id)
	}
	return projectcontract.Dependency{ID: id, Constraint: source.Version, Source: resolvedSource}, nil
}

// ResolveLocalDependencyPath 解析项目内的本地依赖，并同时检查词法路径与
// 已存在符号链接的物理路径，防止依赖通过链接逃逸项目边界。末尾路径尚不存在
// 时保留其路径，允许后续构建阶段创建工件。
func ResolveLocalDependencyPath(projectRoot, baseDir, relative string) (string, error) {
	if relative == "" || relative == "." || !packagecontract.IsPackagePath(relative) {
		return "", ErrSourceInvalid
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	candidate, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	root, err = resolveExistingPath(root)
	if err != nil {
		return "", err
	}
	base, err = resolveExistingPath(base)
	if err != nil {
		return "", err
	}
	candidate, err = resolveExistingPath(candidate)
	if err != nil {
		return "", err
	}
	if !pathWithin(root, base) || !pathWithin(root, candidate) {
		return "", fmt.Errorf("%w: 本地依赖路径逃逸项目目录", ErrSourceInvalid)
	}
	return candidate, nil
}

func resolveExistingPath(path string) (string, error) {
	original := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if info, lstatErr := os.Lstat(path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return original, nil
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
