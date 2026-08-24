package loader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

const (
	installManifestName  = "manifest.json"
	installLockName      = "lock.json"
	maxInstalledRuntimes = 256
	maxInstalledSpecs    = 4096
)

var (
	ErrInstallCatalogInvalid = errors.New("installed runtime catalog is invalid")
	ErrInstallChanged        = errors.New("installed runtime changed after discovery")
)

// aiLuoExtensions 是 AI珞 宿主扩展段（packmgr.Manifest.Extensions）的严格结构：
// 中性包清单只保留语言/宿主无关的核心字段，Tool/Service/Capability 语义由
// 内核解释并严格解码，未知字段与重复键一律拒绝。
type aiLuoExtensions struct {
	Tools        []registry.ToolSpec       `json:"tools"`
	Service      registry.ServiceSpec      `json:"service"`
	Capabilities []registry.CapabilitySpec `json:"capabilities"`
}

// InstalledRecord 是安装目录中单个组件（运行单元）的内核记录。
// 一个包产生多条记录（每组件一条）；Runtime.ID 是包命名空间内的稳定组件标识。
type InstalledRecord struct {
	Directory    string
	ArtifactPath string
	Runtime      Manifest
	PackageID    string
	ComponentID  string
	// ComponentOrder 是该组件在包内的依赖拓扑序号（Provider 小号在前）。
	ComponentOrder int
	Tools          []registry.ToolSpec
	Service        registry.ServiceSpec
	Capabilities   []registry.CapabilitySpec
	Process        *packmgr.ProcessSpec
	Storage        *packmgr.Storage
}

type Catalog struct {
	root string
}

func NewCatalog(root string) (*Catalog, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInstallCatalogInvalid
	}
	return &Catalog{root: root}, nil
}

func (c *Catalog) Discover(ctx context.Context) ([]InstalledRecord, error) {
	if c == nil || c.root == "" {
		return nil, ErrInstallCatalogInvalid
	}
	if err := validateSecureDirectory(c.root); err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	if len(entries) > maxInstalledRuntimes {
		return nil, ErrInstallCatalogInvalid
	}
	records := make([]InstalledRecord, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil, ErrInstallCatalogInvalid
		}
		directory := filepath.Join(c.root, entry.Name())
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) {
			return nil, ErrInstallCatalogInvalid
		}
		packageRecords, err := c.readPackage(ctx, directory)
		if err != nil {
			return nil, err
		}
		if len(packageRecords) == 0 || entry.Name() != packageRecords[0].PackageID {
			return nil, ErrInstallCatalogInvalid
		}
		records = append(records, packageRecords...)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Runtime.ID < records[j].Runtime.ID })
	if err := validateInstalledRecords(records); err != nil {
		return nil, err
	}
	return records, nil
}

func (c *Catalog) VerifyRuntime(ctx context.Context, manifest Manifest) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !sameRuntimeManifest(record.Runtime, manifest) {
		return ErrInstallChanged
	}
	return nil
}

func (c *Catalog) ResolveProcess(ctx context.Context, manifest Manifest) (packmgr.ProcessSpec, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return packmgr.ProcessSpec{}, err
	}
	if !sameRuntimeManifest(record.Runtime, manifest) || record.Process == nil {
		return packmgr.ProcessSpec{}, ErrInstallChanged
	}
	return cloneProcessSpec(*record.Process), nil
}

func (c *Catalog) VerifyProcess(ctx context.Context, manifest Manifest, process packmgr.ProcessSpec) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !sameRuntimeManifest(record.Runtime, manifest) || record.Process == nil ||
		!reflect.DeepEqual(cloneProcessSpec(*record.Process), cloneProcessSpec(process)) {
		return ErrInstallChanged
	}
	return nil
}

// ReadArtifact 读取与 manifest 锁定的 hosted 工件字节。
// 每次读取都重新校验目录、属主、清单一致性、工件 digest 与大小，防止 TOCTOU 替换。
// 与 manifest 的比较只限定身份字段（ID/Version/Mode）：工件 digest 已在 readRecord
// 内重新校验，Role/Pin/IdleTTL 不参与工件装载；且外部 Runtime Host 协议身份只携带
// ID/Version，携带完整清单字段的绑定校验由内核侧 VerifyRuntime 负责。
func (c *Catalog) ReadArtifact(ctx context.Context, manifest Manifest) ([]byte, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return nil, err
	}
	if !sameRuntimeIdentity(record.Runtime, manifest) {
		return nil, ErrInstallChanged
	}
	if record.Runtime.Mode != ModeHosted {
		return nil, ErrUnsupportedMode
	}
	data, err := os.ReadFile(record.ArtifactPath)
	if err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	if int64(len(data)) > packmgr.MaxArtifactBytes {
		return nil, ErrInstallCatalogInvalid
	}
	return data, nil
}

func (c *Catalog) readRecordByID(ctx context.Context, id string) (InstalledRecord, error) {
	if c == nil || !stableIDPattern.MatchString(id) {
		return InstalledRecord{}, ErrInstallCatalogInvalid
	}
	if err := validateSecureDirectory(c.root); err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return InstalledRecord{}, errors.Join(ErrInstallCatalogInvalid, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		records, err := c.readPackage(ctx, filepath.Join(c.root, entry.Name()))
		if err != nil {
			return InstalledRecord{}, err
		}
		for _, record := range records {
			if record.Runtime.ID == id {
				return record, nil
			}
		}
	}
	return InstalledRecord{}, ErrNotFound
}

// readPackage 读取一个包目录并产出每组件一条的内核记录。中性格式（manifest +
// lock + 每组件工件哈希）由 packmgr.ReadInstalled 完成；本函数叠加部署属主
// 校验、解析 AI珞 扩展段并按组件 exports 映射 Capability 到组件运行时。
func (c *Catalog) readPackage(ctx context.Context, directory string) ([]InstalledRecord, error) {
	if err := validateSecureDirectory(directory); err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	// 部署级属主/权限校验叠加在格式层读取之上。
	for _, name := range []string{installManifestName, installLockName} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !ownerMatchesProcess(info) || info.Mode().Perm()&0o022 != 0 {
			return nil, ErrInstallCatalogInvalid
		}
	}
	neutral, err := packmgr.ReadInstalled(ctx, directory)
	if err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	var extensions aiLuoExtensions
	if len(neutral.Manifest.Extensions) > 0 {
		if err := packmgr.DecodeStrictJSON(neutral.Manifest.Extensions, &extensions); err != nil {
			return nil, errors.Join(ErrInstallCatalogInvalid, err)
		}
	}
	order, err := packmgr.ComponentOrder(neutral.Manifest.Components)
	if err != nil {
		return nil, errors.Join(ErrInstallCatalogInvalid, err)
	}
	orderIndex := make(map[string]int, len(order))
	for index, componentID := range order {
		orderIndex[componentID] = index
	}
	artifactsByComponent := make(map[string]packmgr.LockedArtifact, len(neutral.Lock.Artifacts))
	for _, artifact := range neutral.Lock.Artifacts {
		artifactsByComponent[artifact.ComponentID] = artifact
	}
	records := make([]InstalledRecord, 0, len(neutral.Manifest.Components))
	for _, component := range neutral.Manifest.Components {
		runtimeID := neutral.Manifest.ID + "." + component.ID
		if !stableIDPattern.MatchString(runtimeID) || len(runtimeID) > 128 {
			return nil, ErrInstallCatalogInvalid
		}
		artifact, ok := artifactsByComponent[component.ID]
		if !ok {
			return nil, ErrInstallCatalogInvalid
		}
		runtimeManifest := Manifest{
			ID: runtimeID, Version: neutral.Manifest.Version, Mode: component.Mode,
			Role: RoleCapability, LockedDigest: artifact.SHA256,
			Pin: neutral.Manifest.Pin, IdleTTL: time.Duration(neutral.Manifest.IdleTTLMS) * time.Millisecond,
			HostFunctions: packmgr.CloneHostedFunctions(component.HostFunctions),
		}
		if err := validateManifest(runtimeManifest); err != nil {
			return nil, err
		}
		exported := make(map[string]struct{}, len(component.Exports))
		for _, capabilityID := range component.Exports {
			exported[capabilityID] = struct{}{}
		}
		capabilities := make([]registry.CapabilitySpec, 0, len(component.Exports))
		for _, spec := range extensions.Capabilities {
			if _, isExport := exported[spec.ID]; isExport {
				capabilities = append(capabilities, cloneCapabilitySpec(spec))
			}
		}
		record := InstalledRecord{
			Directory: directory, ArtifactPath: artifact.Path,
			Runtime: runtimeManifest, PackageID: neutral.Manifest.ID,
			ComponentID: component.ID, ComponentOrder: orderIndex[component.ID],
			Capabilities: capabilities, Process: artifact.Process,
			Storage: cloneInstalledStorage(neutral.Manifest.Storage),
		}
		// Service 与 Tools 路由到依赖拓扑第一个组件（Provider 基座）。
		if orderIndex[component.ID] == 0 {
			record.Service = cloneInstalledService(extensions.Service)
			record.Tools = cloneToolSpecs(extensions.Tools)
		}
		if err := validateInstalledRecord(record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func RegisterInstalled(ctx context.Context, manager *Manager, target *registry.Registry, records []InstalledRecord) error {
	if manager == nil || target == nil || len(records) == 0 || len(records) > maxInstalledRuntimes {
		return ErrInstallCatalogInvalid
	}
	for _, record := range records {
		if err := validateRecordSpecs(record); err != nil {
			return errors.Join(ErrInstallCatalogInvalid, err)
		}
	}
	manifests := make([]Manifest, 0, len(records))
	tools := make([]registry.ToolRegistration, 0)
	// 按包分组：合并每包内所有组件的 Capability 到一条 Service 注册。
	serviceByPackage := make(map[string]registry.ServiceRegistration)
	for _, record := range records {
		manifests = append(manifests, record.Runtime)
		handler := manager.Handler(record.Runtime.ID)
		for _, spec := range record.Tools {
			tools = append(tools, registry.ToolRegistration{Spec: spec, Handler: handler})
		}
		entry := serviceByPackage[record.PackageID]
		if entry.Capabilities == nil {
			entry.Capabilities = make(map[string]struct {
				Spec    registry.CapabilitySpec
				Handler registry.Handler
			})
		}
		if record.ComponentOrder == 0 {
			entry.Spec = record.Service
		}
		for _, spec := range record.Capabilities {
			entry.Capabilities[spec.ID] = struct {
				Spec    registry.CapabilitySpec
				Handler registry.Handler
			}{Spec: spec, Handler: handler}
		}
		serviceByPackage[record.PackageID] = entry
	}
	if err := manager.RegisterBatch(ctx, manifests); err != nil {
		return err
	}
	services := make([]registry.ServiceRegistration, 0, len(serviceByPackage))
	for _, service := range serviceByPackage {
		if service.Spec.ID != "" {
			services = append(services, service)
		}
	}
	if len(tools) == 0 && len(services) == 0 {
		return nil
	}
	if err := target.RegisterBatch(tools, services); err != nil {
		return errors.Join(err, manager.rollbackRegistered(manifests))
	}
	// 记录包分组（组件已注册）：按 PackageID 分组，按依赖拓扑序排序。
	orderByPackage := make(map[string][]componentWithOrder)
	for _, record := range records {
		orderByPackage[record.PackageID] = append(orderByPackage[record.PackageID], componentWithOrder{
			id: record.Runtime.ID, order: record.ComponentOrder,
		})
	}
	for pkgID, components := range orderByPackage {
		sort.Slice(components, func(i, j int) bool { return components[i].order < components[j].order })
		ordered := make([]string, 0, len(components))
		for _, component := range components {
			ordered = append(ordered, component.id)
		}
		if err := manager.RegisterPackage(pkgID, ordered); err != nil {
			return err
		}
	}
	return nil
}

type componentWithOrder struct {
	id    string
	order int
}

func validateInstalledRecords(records []InstalledRecord) error {
	if len(records) == 0 {
		return nil
	}
	totalSpecs := 0
	seenRuntimes := make(map[string]struct{}, len(records))
	for _, record := range records {
		if _, duplicate := seenRuntimes[record.Runtime.ID]; duplicate {
			return ErrDuplicateID
		}
		seenRuntimes[record.Runtime.ID] = struct{}{}
		totalSpecs += len(record.Tools) + len(record.Capabilities) + 1
		if totalSpecs > maxInstalledSpecs {
			return ErrInstallCatalogInvalid
		}
		if err := validateInstalledRecord(record); err != nil {
			return err
		}
	}
	validation := registry.New()
	tools := make([]registry.ToolRegistration, 0)
	services := make([]registry.ServiceRegistration, 0, len(records))
	for _, record := range records {
		for _, spec := range record.Tools {
			tools = append(tools, registry.ToolRegistration{Spec: spec, Handler: noopInstalledHandler})
		}
		capabilities := make(map[string]struct {
			Spec    registry.CapabilitySpec
			Handler registry.Handler
		}, len(record.Capabilities))
		for _, spec := range record.Capabilities {
			capabilities[spec.ID] = struct {
				Spec    registry.CapabilitySpec
				Handler registry.Handler
			}{Spec: spec, Handler: noopInstalledHandler}
		}
		// 仅 Provider 基座（携带 Service）的组件注册 Service；其余组件只注册 Capabilities。
		if record.Service.ID != "" {
			services = append(services, registry.ServiceRegistration{Spec: record.Service, Capabilities: capabilities})
		}
	}
	if err := validation.RegisterBatch(tools, services); err != nil {
		return errors.Join(ErrInstallCatalogInvalid, err)
	}
	return nil
}

func validateInstalledRecord(record InstalledRecord) error {
	if record.Directory == "" || len(record.Capabilities) == 0 {
		return ErrInstallCatalogInvalid
	}
	if record.Runtime.Mode == ModeIsolated && record.Process == nil {
		return ErrInstallCatalogInvalid
	}
	if record.Runtime.Mode == ModeHosted && record.Process != nil {
		return ErrInstallCatalogInvalid
	}
	return validateRecordSpecs(record)
}

// validateRecordSpecs 校验内置包与 installed 包共用的规格契约：运行时清单、
// 宿主函数声明、storage 声明，以及 Tool/Service/Capability 与运行时版本一致。
// 运行时专用记录（如内置 agent）不携带 Service 规格，只校验运行时清单与声明。
func validateRecordSpecs(record InstalledRecord) error {
	if err := validateManifest(record.Runtime); err != nil {
		return errors.Join(ErrInstallCatalogInvalid, err)
	}
	if record.Storage != nil {
		if err := packmgr.ValidateStorage(*record.Storage); err != nil {
			return err
		}
	}
	if record.Service.ID != "" {
		if record.Service.Version != record.Runtime.Version {
			return ErrInstallCatalogInvalid
		}
		for _, tool := range record.Tools {
			if tool.Version != record.Runtime.Version {
				return ErrInstallCatalogInvalid
			}
		}
	}
	for _, capability := range record.Capabilities {
		if capability.Version != record.Runtime.Version {
			return ErrInstallCatalogInvalid
		}
	}
	return nil
}

func validateSecureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) {
		return ErrInstallCatalogInvalid
	}
	return nil
}

func sameRuntimeManifest(left, right Manifest) bool {
	return left.ID == right.ID && left.Version == right.Version && left.Mode == right.Mode &&
		left.Role == right.Role && left.LockedDigest == right.LockedDigest &&
		left.Pin == right.Pin && left.IdleTTL == right.IdleTTL &&
		packmgr.EqualHostedFunctions(left.HostFunctions, right.HostFunctions)
}

// sameRuntimeIdentity 只比较运行时身份字段（ID/Version/Mode），供 ReadArtifact
// 在装载工件时使用；其余清单字段不参与工件装载，完整清单的一致性由
// VerifyRuntime/readRecord 在各自边界校验。
func sameRuntimeIdentity(left, right Manifest) bool {
	return left.ID == right.ID && left.Version == right.Version && left.Mode == right.Mode
}

func cloneProcessSpec(spec packmgr.ProcessSpec) packmgr.ProcessSpec {
	spec.Args = append([]string(nil), spec.Args...)
	spec.Env = append([]string(nil), spec.Env...)
	return spec
}

func cloneToolSpecs(specs []registry.ToolSpec) []registry.ToolSpec {
	cloned := append([]registry.ToolSpec(nil), specs...)
	for index := range cloned {
		cloned[index].RequiredPermissions = append([]string(nil), cloned[index].RequiredPermissions...)
	}
	return cloned
}

func cloneCapabilitySpec(spec registry.CapabilitySpec) registry.CapabilitySpec {
	spec.RequiredPermissions = append([]string(nil), spec.RequiredPermissions...)
	return spec
}

func cloneInstalledService(spec registry.ServiceSpec) registry.ServiceSpec {
	spec.ToolDependencies = append([]string(nil), spec.ToolDependencies...)
	spec.RequestedPermissions = append([]string(nil), spec.RequestedPermissions...)
	return spec
}

func cloneInstalledStorage(storage *packmgr.Storage) *packmgr.Storage {
	if storage == nil {
		return nil
	}
	cloned := *storage
	return &cloned
}

var noopInstalledHandler registry.Handler = func(context.Context, contracts.RequestContext, json.RawMessage) (json.RawMessage, error) {
	return nil, ErrUnavailable
}
