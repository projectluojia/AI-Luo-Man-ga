// Package packagefmt 是 AI珞 包管理器的作者侧源格式层：把 ailuo.toml（Cargo.toml
// 风格源清单）转换为中性包清单 packagecontract.Manifest。包管理器的文件发布
// 与安装实现位于 packmgr，本包只负责作者侧源格式。
//
// ailuo.toml 采用继承式精简：tool 以 `[tool.<id>]` 表声明（id 即键，不重复）；
// dependency 以 `[dependencies.<id>]` 表声明版本约束与显式来源；capability 只
// 声明 id 与引用的 tool，其余字段（schema、side_effect、name、description）全部
// 继承自 tool；service 段省略时自动生成（id/version/description 继承 package，
// tool_dependencies 默认为全部 tool id）。
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
	Tools        map[string]sourceTool       `toml:"tool,omitempty"`
	Service      *sourceService              `toml:"service,omitempty"`
	Capabilities []sourceCapability          `toml:"capability,omitempty"`
	Build        *BuildSpec                  `toml:"build,omitempty"`
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
	Imports       []string               `toml:"imports,omitempty"`
	HostFunctions []sourceHostedFunction `toml:"host_function,omitempty"`
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
	Namespace     string `toml:"namespace"`
	SchemaVersion uint32 `toml:"schema_version"`
	Sensitivity   string `toml:"sensitivity"`
	Retention     string `toml:"retention"`
}

type sourceDependency struct {
	Version string `toml:"version"`
	Source  string `toml:"source"`
}

// sourceTool 是 `[tool.<id>]` 表的字段；id 由键提供，version 继承 package。
type sourceTool struct {
	Description          string   `toml:"description"`
	Schema               string   `toml:"schema"`
	SideEffect           string   `toml:"side_effect"`
	RequiresConfirmation bool     `toml:"requires_confirmation,omitempty"`
	RequiredPermissions  []string `toml:"required_permissions,omitempty"`
}

// sourceService 继承 package 的 id/version/description；tool_dependencies 省略
// 时默认为全部 tool id（按字母序）。
type sourceService struct {
	ToolDependencies     []string `toml:"tool_dependencies,omitempty"`
	RequestedPermissions []string `toml:"requested_permissions,omitempty"`
}

// sourceCapability 引用 tool 并继承其 schema/side_effect/name/description。
type sourceCapability struct {
	ID          string `toml:"id"`
	Tool        string `toml:"tool"`
	Name        string `toml:"name,omitempty"`
	Description string `toml:"description,omitempty"`
}

// Parse 读取并解析 ailuo.toml：严格 TOML 解码（未知字段/重复键拒绝），转换为
// 中性包清单并校验。返回的 manifestBytes 是序列化后的清单字节（供 pack 时
// 原样写入与 digest 锁定，与 packmgr 安装路径一致）；build 为 `[build]` 声明
// （nil 表示源清单未声明构建，工件由作者预置）。
func Parse(path string) (manifest packagecontract.Manifest, manifestBytes []byte, build *BuildSpec, err error) {
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
	if source.Build != nil {
		build = &BuildSpec{Tool: source.Build.Tool, Source: source.Build.Source}
	}
	return manifest, manifestBytes, build, nil
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
			Namespace:     s.Storage.Namespace,
			SchemaVersion: s.Storage.SchemaVersion,
			Sensitivity:   s.Storage.Sensitivity,
			Retention:     s.Storage.Retention,
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
			Exports: append([]string(nil), component.Exports...),
			Imports: append([]string(nil), component.Imports...), HostFunctions: decls,
		})
	}
	extensions, err := s.buildExtensions()
	if err != nil {
		return packagecontract.Manifest{}, err
	}
	manifest.Extensions = extensions
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		return packagecontract.Manifest{}, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	return manifest, nil
}

// buildExtensions 组装宿主扩展段（tools/service/capabilities），应用继承规则：
// 版本继承 package；capability 从 tool 继承 schema/side_effect/name/description；
// service 缺省时自动生成。输出与内核 registry 规格字段一致的 JSON。
func (s sourceManifest) buildExtensions() (json.RawMessage, error) {
	if len(s.Tools) == 0 && len(s.Capabilities) == 0 && s.Service == nil {
		return nil, nil
	}
	toolIDs := make([]string, 0, len(s.Tools))
	for id := range s.Tools {
		toolIDs = append(toolIDs, id)
	}
	sort.Strings(toolIDs)
	tools := make([]capability.ToolSpec, 0, len(toolIDs))
	for _, id := range toolIDs {
		tool := s.Tools[id]
		tools = append(tools, capability.ToolSpec{
			ID: id, Version: s.Package.Version, Description: tool.Description,
			InputSchemaJSON: tool.Schema, SideEffect: tool.SideEffect,
			RequiresConfirmation: tool.RequiresConfirmation,
			RequiredPermissions:  tool.RequiredPermissions,
		})
	}
	service := capability.ServiceSpec{
		ID: s.Package.ID, Version: s.Package.Version, Description: s.Package.Description,
	}
	if s.Service != nil {
		service.ToolDependencies = s.Service.ToolDependencies
		service.RequestedPermissions = s.Service.RequestedPermissions
	}
	if len(service.ToolDependencies) == 0 {
		service.ToolDependencies = append([]string(nil), toolIDs...)
	}
	capabilities := make([]capability.CapabilitySpec, 0, len(s.Capabilities))
	for _, declaration := range s.Capabilities {
		tool, ok := s.Tools[declaration.Tool]
		if !ok {
			return nil, fmt.Errorf("%w: capability %s 引用不存在的 tool %s", ErrSourceInvalid, declaration.ID, declaration.Tool)
		}
		name := declaration.Name
		if name == "" {
			name = tool.Description
		}
		description := declaration.Description
		if description == "" {
			description = tool.Description
		}
		capabilities = append(capabilities, capability.CapabilitySpec{
			ID: declaration.ID, Version: s.Package.Version, Name: name,
			Description: description, ServiceID: s.Package.ID,
			InputSchemaJSON: tool.Schema, SideEffect: tool.SideEffect,
			RequiresConfirmation: tool.RequiresConfirmation,
			RequiredPermissions:  tool.RequiredPermissions,
			ToolID:               declaration.Tool,
		})
	}
	extensions := struct {
		Tools        []capability.ToolSpec       `json:"tools,omitempty"`
		Service      capability.ServiceSpec      `json:"service,omitempty"`
		Capabilities []capability.CapabilitySpec `json:"capabilities,omitempty"`
	}{Tools: tools, Service: service, Capabilities: capabilities}
	data, err := json.Marshal(extensions)
	if err != nil {
		return nil, fmt.Errorf("%w: 扩展段序列化失败: %v", ErrSourceInvalid, err)
	}
	return data, nil
}
