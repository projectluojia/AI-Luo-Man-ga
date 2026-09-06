// Package packagefmt 是 AI珞 包管理器的作者侧源格式层：把 ailuo.toml（Cargo.toml
// 风格源清单）转换为中性包清单 packagecontract.Manifest。包管理器的文件发布
// 与安装实现位于 packmgr，本包只负责作者侧源格式。
//
// ailuo.toml 采用显式 Capability 声明：依赖以 `[dependencies.<id>]` 表声明版本
// 约束与来源；Capability 直接声明自己的 Schema、权限和副作用。每个 component
// 通过 `[component.build]` 声明自己的构建器和源码目录。
package packagefmt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
)

// 源清单文件名的唯一约定（作者侧包定义）。
const SourceFileName = "ailuo.toml"

var ErrSourceInvalid = errors.New("invalid ailuo.toml source manifest")

// sourceManifest 是 ailuo.toml 的完整结构。
type sourceManifest struct {
	Package      sourcePackage               `toml:"package"`
	Components   []sourceComponent           `toml:"component"`
	Storage      *sourceStorage              `toml:"storage,omitempty"`
	Dependencies map[string]sourceDependency `toml:"dependencies,omitempty"`
	Capabilities []sourceCapability          `toml:"capability,omitempty"`
}

type sourcePackage struct {
	ID          string `toml:"id"`
	Version     string `toml:"version"`
	Description string `toml:"description,omitempty"`
	Pin         bool   `toml:"pin,omitempty"`
	IdleTTLMS   uint64 `toml:"idle_ttl_ms,omitempty"`
}

type sourceComponent struct {
	ID            string                 `toml:"id"`
	Mode          string                 `toml:"mode"`
	Role          string                 `toml:"role,omitempty"`
	Entrypoint    string                 `toml:"entrypoint"`
	Process       *sourceProcess         `toml:"process,omitempty"`
	Exports       []string               `toml:"exports,omitempty"`
	HostFunctions []sourceHostedFunction `toml:"host_function,omitempty"`
	Build         *sourceBuild           `toml:"build,omitempty"`
}

type sourceBuild struct {
	Tool   string `toml:"tool"`
	Source string `toml:"source,omitempty"`
}

type sourceProcess struct {
	Path    string   `toml:"path"`
	Args    []string `toml:"args,omitempty"`
	WorkDir string   `toml:"work_dir,omitempty"`
	Address string   `toml:"address,omitempty"`
}

type sourceHostedFunction struct {
	Module  string `toml:"module"`
	Name    string `toml:"name"`
	Purpose string `toml:"purpose,omitempty"`
}

type sourceStorage struct {
	Namespace string `toml:"namespace"`
}

type sourceDependency struct {
	Version string `toml:"version"`
	Source  string `toml:"source"`
}

// sourceCapability 是一个直接对外声明的 Capability；版本继承 package。
type sourceCapability struct {
	ID            string              `toml:"id"`
	Name          string              `toml:"name"`
	Description   string              `toml:"description"`
	Schema        string              `toml:"schema"`
	Authorization sourceAuthorization `toml:"authorization"`
	Execution     sourceExecution     `toml:"execution"`
}

type sourceAuthorization struct {
	ResourceType   string `toml:"resource_type"`
	ResourceIDFrom string `toml:"resource_id_from,omitempty"`
	Principal      string `toml:"principal,omitempty"`
}

type sourceExecution struct {
	EffectTarget      string `toml:"effect_target"`
	Replay            string `toml:"replay"`
	ConfirmationFloor string `toml:"confirmation_floor"`
}

// Parse 读取并解析 ailuo.toml：严格 TOML 解码（未知字段/重复键拒绝），转换为
// 中性包清单并校验。返回的 manifestBytes 是序列化后的清单字节（供 pack 时
// 原样写入与 digest 锁定，与 packmgr 安装路径一致）；builds 是按 component 排列
// 的构建计划（为空表示所有工件由作者预置）。
func Parse(path string) (manifest packagecontract.Manifest, manifestBytes []byte, builds []BuildSpec, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return packagecontract.Manifest{}, nil, nil, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	if int64(len(data)) > packagecontract.MaxManifestBytes {
		return packagecontract.Manifest{}, nil, nil, fmt.Errorf("%w: 源清单超过大小上限", ErrSourceInvalid)
	}
	var source sourceManifest
	meta, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&source)
	if err != nil {
		return packagecontract.Manifest{}, nil, nil, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return packagecontract.Manifest{}, nil, nil, fmt.Errorf("%w: 未知字段 %s", ErrSourceInvalid, undecoded[0])
	}
	manifest, err = source.convert()
	if err != nil {
		return packagecontract.Manifest{}, nil, nil, err
	}
	manifestBytes, err = json.Marshal(manifest)
	if err != nil {
		return packagecontract.Manifest{}, nil, nil, fmt.Errorf("%w: 清单序列化失败: %v", ErrSourceInvalid, err)
	}
	for _, component := range source.Components {
		if component.Build == nil {
			continue
		}
		builds = append(builds, BuildSpec{
			Tool: component.Build.Tool, Source: component.Build.Source,
			Components: []string{component.ID},
		})
	}
	return manifest, manifestBytes, builds, nil
}

// SourcePath 返回源包目录中的 ailuo.toml 路径（若存在）。
func SourcePath(sourceDir string) string {
	return filepath.Join(sourceDir, SourceFileName)
}

// convert 把 TOML 源结构转换为中性包清单，并执行契约校验。
func (s sourceManifest) convert() (packagecontract.Manifest, error) {
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion,
		ID:            s.Package.ID,
		Version:       s.Package.Version,
		Pin:           s.Package.Pin,
		IdleTTLMS:     s.Package.IdleTTLMS,
	}
	if s.Storage != nil {
		manifest.Storage = &packagecontract.Storage{
			Namespace: s.Storage.Namespace,
		}
	}
	dependencyIDs := make([]string, 0, len(s.Dependencies))
	for id := range s.Dependencies {
		dependencyIDs = append(dependencyIDs, id)
	}
	sort.Strings(dependencyIDs)
	for _, id := range dependencyIDs {
		dep := s.Dependencies[id]
		manifest.Dependencies = append(manifest.Dependencies, packagecontract.Dependency{
			ID: id, Constraint: dep.Version, Source: dep.Source,
		})
	}
	if len(s.Components) == 0 {
		return packagecontract.Manifest{}, fmt.Errorf("%w: 未声明任何 component", ErrSourceInvalid)
	}
	for _, component := range s.Components {
		decls := make([]packagecontract.HostedFunctionDecl, 0, len(component.HostFunctions))
		for _, decl := range component.HostFunctions {
			decls = append(decls, packagecontract.HostedFunctionDecl{
				Module: decl.Module, Name: decl.Name, Purpose: decl.Purpose,
			})
		}
		var process *packagecontract.ProcessTemplate
		if component.Process != nil {
			process = &packagecontract.ProcessTemplate{
				Path: component.Process.Path, Args: append([]string(nil), component.Process.Args...),
				WorkDir: component.Process.WorkDir, Address: component.Process.Address,
			}
		}
		manifest.Components = append(manifest.Components, packagecontract.Component{
			ID: component.ID, Mode: component.Mode, Role: component.Role,
			Entrypoint: component.Entrypoint, Process: process,
			Exports: append([]string(nil), component.Exports...), HostFunctions: decls,
		})
	}
	capabilities, err := s.buildCapabilities()
	if err != nil {
		return packagecontract.Manifest{}, err
	}
	manifest.Capabilities = capabilities
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		return packagecontract.Manifest{}, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	return manifest, nil
}

// buildCapabilities 将作者清单中的 Capability 转换为中性契约。
func (s sourceManifest) buildCapabilities() ([]capability.CapabilitySpec, error) {
	capabilities := make([]capability.CapabilitySpec, 0, len(s.Capabilities))
	for _, declaration := range s.Capabilities {
		capabilities = append(capabilities, capability.CapabilitySpec{
			ID: declaration.ID, Version: s.Package.Version, Name: declaration.Name,
			Description: declaration.Description, InputSchemaJSON: declaration.Schema,
			Authorization: capability.AuthorizationSpec{
				ResourceType:   declaration.Authorization.ResourceType,
				ResourceIDFrom: declaration.Authorization.ResourceIDFrom,
				Principal:      declaration.Authorization.Principal,
			},
			Execution: capability.ExecutionSpec{
				EffectTarget:      declaration.Execution.EffectTarget,
				Replay:            declaration.Execution.Replay,
				ConfirmationFloor: declaration.Execution.ConfirmationFloor,
			},
		})
	}
	return capabilities, nil
}
