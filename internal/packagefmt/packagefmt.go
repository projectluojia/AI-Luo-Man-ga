// Package packagefmt 是 AI珞 包管理器的作者侧源格式层：把 ailuo.toml（Cargo.toml
// 风格源清单）转换为中性包清单 packmgr.Manifest。与 packmgr（stdlib-only 中立
// 格式层）分离：TOML 解析依赖只存在于本包，packmgr 保持可整体迁移。
//
// ailuo.toml 采用继承式精简：tool 以 `[tool.<id>]` 表声明（id 即键，不重复）；
// capability 只声明 id 与引用的 tool，其余字段（schema、side_effect、name、
// description）全部继承自 tool；service 段省略时自动生成（id/version/description
// 继承 package，tool_dependencies 默认为全部 tool id）。
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

	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

// 源清单文件名的唯一约定（作者侧包定义）。
const SourceFileName = "ailuo.toml"

var ErrSourceInvalid = errors.New("invalid ailuo.toml source manifest")

// sourceManifest 是 ailuo.toml 的完整结构。
type sourceManifest struct {
	Package      sourcePackage         `toml:"package"`
	Components   []sourceComponent     `toml:"component"`
	Storage      *sourceStorage        `toml:"storage,omitempty"`
	Dependencies []sourceDependency    `toml:"dependency,omitempty"`
	Tools        map[string]sourceTool `toml:"tool,omitempty"`
	Service      *sourceService        `toml:"service,omitempty"`
	Capabilities []sourceCapability    `toml:"capability,omitempty"`
	Build        *BuildSpec            `toml:"build,omitempty"`
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
	Entrypoint    string                 `toml:"entrypoint"`
	Exports       []string               `toml:"exports,omitempty"`
	Imports       []string               `toml:"imports,omitempty"`
	HostFunctions []sourceHostedFunction `toml:"host_function,omitempty"`
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
	ID         string `toml:"id"`
	Constraint string `toml:"constraint"`
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
func Parse(path string) (manifest packmgr.Manifest, manifestBytes []byte, build *BuildSpec, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return packmgr.Manifest{}, nil, nil, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	if int64(len(data)) > packmgr.MaxManifestBytes {
		return packmgr.Manifest{}, nil, nil, fmt.Errorf("%w: 源清单超过大小上限", ErrSourceInvalid)
	}
	var source sourceManifest
	meta, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&source)
	if err != nil {
		return packmgr.Manifest{}, nil, nil, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return packmgr.Manifest{}, nil, nil, fmt.Errorf("%w: 未知字段 %s", ErrSourceInvalid, undecoded[0])
	}
	manifest, err = source.convert()
	if err != nil {
		return packmgr.Manifest{}, nil, nil, err
	}
	manifestBytes, err = json.Marshal(manifest)
	if err != nil {
		return packmgr.Manifest{}, nil, nil, fmt.Errorf("%w: 清单序列化失败: %v", ErrSourceInvalid, err)
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

// convert 把 TOML 源结构转换为中性包清单，并执行与 packmgr.ValidateManifest
// 一致的严格校验。
func (s sourceManifest) convert() (packmgr.Manifest, error) {
	manifest := packmgr.Manifest{
		SchemaVersion: packmgr.SchemaVersion,
		ID:            s.Package.ID,
		Version:       s.Package.Version,
		Pin:           s.Package.Pin,
		IdleTTLMS:     s.Package.IdleTTLMS,
	}
	if s.Storage != nil {
		manifest.Storage = &packmgr.Storage{
			Namespace:     s.Storage.Namespace,
			SchemaVersion: s.Storage.SchemaVersion,
			Sensitivity:   s.Storage.Sensitivity,
			Retention:     s.Storage.Retention,
		}
	}
	for _, dep := range s.Dependencies {
		manifest.Dependencies = append(manifest.Dependencies, packmgr.Dependency{
			ID: dep.ID, Constraint: dep.Constraint,
		})
	}
	if len(s.Components) == 0 {
		return packmgr.Manifest{}, fmt.Errorf("%w: 未声明任何 component", ErrSourceInvalid)
	}
	for _, component := range s.Components {
		decls := make([]packmgr.HostedFunctionDecl, 0, len(component.HostFunctions))
		for _, decl := range component.HostFunctions {
			decls = append(decls, packmgr.HostedFunctionDecl{
				Module: decl.Module, Name: decl.Name, Purpose: decl.Purpose,
			})
		}
		manifest.Components = append(manifest.Components, packmgr.Component{
			ID:            component.ID,
			Mode:          component.Mode,
			Entrypoint:    component.Entrypoint,
			Exports:       append([]string(nil), component.Exports...),
			Imports:       append([]string(nil), component.Imports...),
			HostFunctions: decls,
		})
	}
	extensions, err := s.buildExtensions()
	if err != nil {
		return packmgr.Manifest{}, err
	}
	manifest.Extensions = extensions
	if err := packmgr.ValidateManifest(manifest); err != nil {
		return packmgr.Manifest{}, fmt.Errorf("%w: %v", ErrSourceInvalid, err)
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
	tools := make([]jsonTool, 0, len(toolIDs))
	for _, id := range toolIDs {
		tool := s.Tools[id]
		tools = append(tools, jsonTool{
			ID: id, Version: s.Package.Version, Description: tool.Description,
			InputSchemaJSON: tool.Schema, SideEffect: tool.SideEffect,
			RequiresConfirmation: tool.RequiresConfirmation,
			RequiredPermissions:  tool.RequiredPermissions,
		})
	}
	service := jsonService{
		ID: s.Package.ID, Version: s.Package.Version, Description: s.Package.Description,
	}
	if s.Service != nil {
		service.ToolDependencies = s.Service.ToolDependencies
		service.RequestedPermissions = s.Service.RequestedPermissions
	}
	if len(service.ToolDependencies) == 0 {
		service.ToolDependencies = append([]string(nil), toolIDs...)
	}
	capabilities := make([]jsonCapability, 0, len(s.Capabilities))
	for _, capability := range s.Capabilities {
		tool, ok := s.Tools[capability.Tool]
		if !ok {
			return nil, fmt.Errorf("%w: capability %s 引用不存在的 tool %s", ErrSourceInvalid, capability.ID, capability.Tool)
		}
		name := capability.Name
		if name == "" {
			name = tool.Description
		}
		description := capability.Description
		if description == "" {
			description = tool.Description
		}
		capabilities = append(capabilities, jsonCapability{
			ID: capability.ID, Version: s.Package.Version, Name: name,
			Description: description, ServiceID: s.Package.ID,
			InputSchemaJSON: tool.Schema, SideEffect: tool.SideEffect,
			RequiresConfirmation: tool.RequiresConfirmation,
			RequiredPermissions:  tool.RequiredPermissions,
			ToolID:               capability.Tool,
		})
	}
	extensions := struct {
		Tools        []jsonTool       `json:"tools,omitempty"`
		Service      jsonService      `json:"service,omitempty"`
		Capabilities []jsonCapability `json:"capabilities,omitempty"`
	}{Tools: tools, Service: service, Capabilities: capabilities}
	data, err := json.Marshal(extensions)
	if err != nil {
		return nil, fmt.Errorf("%w: 扩展段序列化失败: %v", ErrSourceInvalid, err)
	}
	return data, nil
}

// jsonTool/jsonService/jsonCapability 是内核 registry 规格的 JSON 表达
// （字段名与 registry.ToolSpec/ServiceSpec/CapabilitySpec 一致）。
type jsonTool struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
}

type jsonService struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Description          string   `json:"description"`
	ToolDependencies     []string `json:"tool_dependencies,omitempty"`
	RequestedPermissions []string `json:"requested_permissions,omitempty"`
}

type jsonCapability struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	ServiceID            string   `json:"service_id"`
	InputSchemaJSON      string   `json:"input_schema_json"`
	SideEffect           string   `json:"side_effect"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiredPermissions  []string `json:"required_permissions,omitempty"`
	ToolID               string   `json:"tool_id,omitempty"`
}
