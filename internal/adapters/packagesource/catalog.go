package packagesource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/packmgr"
)

const (
	installManifestName = "manifest.json"
	installLockName     = "lock.json"
)

type aiLuoExtensions struct {
	Tools        []capability.ToolSpec       `json:"tools"`
	Service      capability.ServiceSpec      `json:"service"`
	Capabilities []capability.CapabilitySpec `json:"capabilities"`
}

type Catalog struct {
	root string
}

func NewCatalog(root string) (*Catalog, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, loader.ErrInstallCatalogInvalid
	}
	return &Catalog{root: root}, nil
}

func (c *Catalog) Discover(ctx context.Context) ([]loader.InstalledRecord, error) {
	if c == nil || c.root == "" {
		return nil, loader.ErrInstallCatalogInvalid
	}
	if err := validateSecureDirectory(c.root); err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	if err := packmgr.RecoverInstallRoot(ctx, c.root); err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	if len(entries) > loader.MaxInstalledRuntimes {
		return nil, loader.ErrInstallCatalogInvalid
	}
	records := make([]loader.InstalledRecord, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if packmgr.IsTransientInstallDirectory(entry.Name()) {
			if !entry.IsDir() {
				return nil, loader.ErrInstallCatalogInvalid
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil, loader.ErrInstallCatalogInvalid
		}
		directory := filepath.Join(c.root, entry.Name())
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) {
			return nil, loader.ErrInstallCatalogInvalid
		}
		packageRecords, err := c.readPackage(ctx, directory)
		if err != nil {
			return nil, err
		}
		if len(packageRecords) == 0 || entry.Name() != packageRecords[0].PackageID {
			return nil, loader.ErrInstallCatalogInvalid
		}
		records = append(records, packageRecords...)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Runtime.ID < records[j].Runtime.ID })
	return records, nil
}

func (c *Catalog) VerifyRuntime(ctx context.Context, manifest loader.Manifest) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !record.Runtime.Equal(manifest) {
		return loader.ErrInstallChanged
	}
	return nil
}

func (c *Catalog) ResolveProcess(ctx context.Context, manifest loader.Manifest) (packmgr.ProcessSpec, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return packmgr.ProcessSpec{}, err
	}
	if !record.Runtime.Equal(manifest) || record.Process == nil {
		return packmgr.ProcessSpec{}, loader.ErrInstallChanged
	}
	return cloneProcessSpec(*record.Process), nil
}

func (c *Catalog) VerifyProcess(ctx context.Context, manifest loader.Manifest, process packmgr.ProcessSpec) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !record.Runtime.Equal(manifest) || record.Process == nil ||
		!reflect.DeepEqual(*record.Process, process) {
		return loader.ErrInstallChanged
	}
	return nil
}

// ReadArtifact 读取与 manifest 锁定的 hosted 工件字节。
// 每次读取都重新校验目录、属主、清单一致性、工件 digest 与大小，防止 TOCTOU 替换。
// 与 manifest 的比较只限定身份字段（ID/Version/Mode）：工件 digest 已在 readRecord
// 内重新校验，Role/Pin/IdleTTL 不参与工件装载；且外部 Runtime Host 协议身份只携带
// ID/Version，携带完整清单字段的绑定校验由内核侧 VerifyRuntime 负责。
func (c *Catalog) ReadArtifact(ctx context.Context, manifest loader.Manifest) ([]byte, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return nil, err
	}
	if !record.Runtime.SameIdentity(manifest) {
		return nil, loader.ErrInstallChanged
	}
	if record.Runtime.Mode != loader.ModeHosted {
		return nil, loader.ErrUnsupportedMode
	}
	data, err := os.ReadFile(record.ArtifactPath)
	if err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	if int64(len(data)) > packmgr.MaxArtifactBytes {
		return nil, loader.ErrInstallCatalogInvalid
	}
	return data, nil
}

func (c *Catalog) readRecordByID(ctx context.Context, id string) (loader.InstalledRecord, error) {
	if c == nil || !capability.IsStableID(id) {
		return loader.InstalledRecord{}, loader.ErrInstallCatalogInvalid
	}
	if err := validateSecureDirectory(c.root); err != nil {
		return loader.InstalledRecord{}, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	if err := packmgr.RecoverInstallRoot(ctx, c.root); err != nil {
		return loader.InstalledRecord{}, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return loader.InstalledRecord{}, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	for _, entry := range entries {
		if packmgr.IsTransientInstallDirectory(entry.Name()) {
			continue
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		records, err := c.readPackage(ctx, filepath.Join(c.root, entry.Name()))
		if err != nil {
			return loader.InstalledRecord{}, err
		}
		for _, record := range records {
			if record.Runtime.ID == id {
				return record, nil
			}
		}
	}
	return loader.InstalledRecord{}, loader.ErrNotFound
}

// readPackage 读取一个包目录并产出每组件一条的内核记录。中性格式（manifest +
// lock + 每组件工件哈希）由 packmgr.ReadInstalled 完成；本函数叠加部署属主
// 校验、解析 AI珞 扩展段并按组件 exports 映射 Capability 到组件运行时。
func (c *Catalog) readPackage(ctx context.Context, directory string) ([]loader.InstalledRecord, error) {
	if err := validateSecureDirectory(directory); err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	// 部署级属主/权限校验叠加在格式层读取之上。
	for _, name := range []string{installManifestName, installLockName} {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil || !ownerMatchesProcess(info) || info.Mode().Perm()&0o022 != 0 {
			return nil, loader.ErrInstallCatalogInvalid
		}
	}
	neutral, err := packmgr.ReadInstalled(ctx, directory)
	if err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	var extensions aiLuoExtensions
	if len(neutral.Manifest.Extensions) > 0 {
		if err := packmgr.DecodeStrictJSON(neutral.Manifest.Extensions, &extensions); err != nil {
			return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
		}
	}
	order, err := packmgr.ComponentOrder(neutral.Manifest.Components)
	if err != nil {
		return nil, errors.Join(loader.ErrInstallCatalogInvalid, err)
	}
	orderIndex := make(map[string]int, len(order))
	for index, componentID := range order {
		orderIndex[componentID] = index
	}
	artifactsByComponent := make(map[string]packmgr.LockedArtifact, len(neutral.Lock.Artifacts))
	for _, artifact := range neutral.Lock.Artifacts {
		artifactsByComponent[artifact.ComponentID] = artifact
	}
	records := make([]loader.InstalledRecord, 0, len(neutral.Manifest.Components))
	for _, component := range neutral.Manifest.Components {
		runtimeID := neutral.Manifest.ID + "." + component.ID
		if !capability.IsStableID(runtimeID) || len(runtimeID) > 128 {
			return nil, loader.ErrInstallCatalogInvalid
		}
		artifact, ok := artifactsByComponent[component.ID]
		if !ok {
			return nil, loader.ErrInstallCatalogInvalid
		}
		runtimeManifest := loader.Manifest{
			ID: runtimeID, Version: neutral.Manifest.Version, Mode: component.Mode,
			Role: loader.RoleCapability, LockedDigest: artifact.SHA256,
			Pin: neutral.Manifest.Pin, IdleTTL: time.Duration(neutral.Manifest.IdleTTLMS) * time.Millisecond,
			HostFunctions: slices.Clone(component.HostFunctions),
		}
		if err := loader.ValidateManifest(runtimeManifest); err != nil {
			return nil, err
		}
		exported := make(map[string]struct{}, len(component.Exports))
		for _, capabilityID := range component.Exports {
			exported[capabilityID] = struct{}{}
		}
		capabilities := make([]capability.CapabilitySpec, 0, len(component.Exports))
		for _, spec := range extensions.Capabilities {
			if _, isExport := exported[spec.ID]; isExport {
				capabilities = append(capabilities, cloneCapabilitySpec(spec))
			}
		}
		record := loader.InstalledRecord{
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
		if err := loader.ValidateInstalledRecord(record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func validateSecureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 || !ownerMatchesProcess(info) {
		return loader.ErrInstallCatalogInvalid
	}
	return nil
}

func cloneProcessSpec(spec packmgr.ProcessSpec) packmgr.ProcessSpec {
	spec.Args = slices.Clone(spec.Args)
	spec.Env = slices.Clone(spec.Env)
	return spec
}

func cloneToolSpecs(specs []capability.ToolSpec) []capability.ToolSpec {
	cloned := slices.Clone(specs)
	for index := range cloned {
		cloned[index].RequiredPermissions = slices.Clone(cloned[index].RequiredPermissions)
	}
	return cloned
}

func cloneCapabilitySpec(spec capability.CapabilitySpec) capability.CapabilitySpec {
	spec.RequiredPermissions = slices.Clone(spec.RequiredPermissions)
	return spec
}

func cloneInstalledService(spec capability.ServiceSpec) capability.ServiceSpec {
	spec.ToolDependencies = slices.Clone(spec.ToolDependencies)
	spec.RequestedPermissions = slices.Clone(spec.RequestedPermissions)
	return spec
}

func cloneInstalledStorage(storage *packmgr.Storage) *packmgr.Storage {
	if storage == nil {
		return nil
	}
	cloned := *storage
	return &cloned
}
