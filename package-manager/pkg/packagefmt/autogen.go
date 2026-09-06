package packagefmt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/package-manager/pkg/packagefmt/schemaextract"
)

// AutoExtract 从包目录源码自动提取 capabilities，并返回对应的构建器：
// main.go 使用 Go wasm，main.ts 使用 AssemblyScript。包 id 取目录名。
func AutoExtract(ctx context.Context, sourceDir string) ([]schemaextract.Capability, string, error) {
	absolute, err := filepath.Abs(sourceDir)
	if err != nil {
		return nil, "", err
	}
	id := filepath.Base(absolute)
	source, err := os.ReadFile(filepath.Join(sourceDir, "main.go"))
	if err == nil {
		capabilities, analyzeErr := schemaextract.AnalyzeGo(source, id)
		return capabilities, BuildToolGoWasm, analyzeErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("packagefmt: 读取 %q 失败: %w", filepath.Join(sourceDir, "main.go"), err)
	}
	source, err = os.ReadFile(filepath.Join(sourceDir, "main.ts"))
	if err == nil {
		capabilities, analyzeErr := schemaextract.AnalyzeTS(ctx, source, id, sourceDir)
		return capabilities, BuildToolAssemblyScript, analyzeErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("packagefmt: 读取 %q 失败: %w", filepath.Join(sourceDir, "main.ts"), err)
	}
	return nil, "", fmt.Errorf("packagefmt: %q 未找到 main.go 或 main.ts", sourceDir)
}

func capabilitiesSource(packageID, version string, capabilities []schemaextract.Capability) (sourceManifest, []string, error) {
	if !capability.IsStableID(packageID) {
		return sourceManifest{}, nil, fmt.Errorf("packagefmt: 包 id %q 不合法", packageID)
	}
	if len(capabilities) == 0 {
		return sourceManifest{}, nil, fmt.Errorf("packagefmt: 源码未提取到任何 capability")
	}
	source := sourceManifest{
		Package: sourcePackage{ID: packageID, Version: version},
		Tools:   make(map[string]sourceTool, len(capabilities)),
	}
	exportIDs := make([]string, 0, len(capabilities))
	for _, spec := range capabilities {
		source.Tools[spec.ID] = sourceTool{
			Description: spec.Description,
			Schema:      string(spec.InputSchema),
			SideEffect:  capability.SideEffectRead,
		}
		source.Capabilities = append(source.Capabilities, sourceCapability{
			ID: spec.ID, Tool: spec.ID,
			Description: spec.Description,
		})
		exportIDs = append(exportIDs, spec.ID)
	}
	sort.Strings(exportIDs)
	return source, exportIDs, nil
}

// ExtensionsFromCapabilities 只构造 SDK 所需的宿主扩展段，不生成安装清单。
// SDK 源码可以由零声明包直接生成，无需虚构或持久化包版本。
func ExtensionsFromCapabilities(packageID string, capabilities []schemaextract.Capability) (json.RawMessage, error) {
	source, _, err := capabilitiesSource(packageID, "", capabilities)
	if err != nil {
		return nil, err
	}
	return source.buildExtensions()
}

// ManifestFromCapabilities 从源码提取的 capabilities 自动生成纯计算包清单
// （ailuo pack 在无 ailuo.toml 时使用）：作者只写 guest 源码，component/build/
// capability 全部自动推导。工具 schema 来自提取器，side_effect 统一 read
// （纯计算无副作用；需要写/宿主能力的包仍用 ailuo.toml 显式声明）。

func ManifestFromCapabilities(packageID, version string, capabilities []schemaextract.Capability) (packagecontract.Manifest, []byte, error) {
	if _, err := packagecontract.ParseVersion(version); err != nil {
		return packagecontract.Manifest{}, nil, fmt.Errorf("packagefmt: 包版本 %q 不合法", version)
	}
	source, exportIDs, err := capabilitiesSource(packageID, version, capabilities)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	extensions, err := source.buildExtensions()
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	manifest := packagecontract.Manifest{
		SchemaVersion: packagecontract.SchemaVersion,
		ID:            packageID,
		Version:       version,
		Components: []packagecontract.Component{{
			ID: "main", Mode: "hosted", Entrypoint: "main.wasm", Exports: exportIDs,
		}},
		Extensions: extensions,
	}
	if err := packagecontract.ValidateManifest(manifest); err != nil {
		return packagecontract.Manifest{}, nil, fmt.Errorf("packagefmt: 自动生成清单校验失败: %w", err)
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return packagecontract.Manifest{}, nil, err
	}
	return manifest, manifestBytes, nil
}
