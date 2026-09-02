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

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packagecontract"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/packageio"
	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/projectcontract"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
)

const (
	installManifestName = "manifest.json"
	installLockName     = "lock.json"
)

var (
	ErrInvalidCatalog = errors.New("installed package catalog is invalid")
	ErrChanged        = errors.New("installed package changed after discovery")
)

type aiLuoExtensions struct {
	Tools        []capability.ToolSpec       `json:"tools"`
	Service      capability.ServiceSpec      `json:"service"`
	Capabilities []capability.CapabilitySpec `json:"capabilities"`
}

type Catalog struct {
	root string
}

// ReadProjectLock 读取并校验项目级 ailuo.lock，同时用 ailuo.toml 的文件摘要
// 拒绝过期锁。项目清单的 TOML 语义由 package-manager 解析；Core 只消费已经
// 生成的锁和其完整性摘要。
func ReadProjectLock(ctx context.Context, projectRoot string) (projectcontract.Lock, error) {
	if projectRoot == "" || !filepath.IsAbs(projectRoot) || filepath.Clean(projectRoot) != projectRoot {
		return projectcontract.Lock{}, ErrInvalidCatalog
	}
	manifestSHA, err := packageio.HashFile(ctx, filepath.Join(projectRoot, "ailuo.toml"), packagecontract.MaxManifestBytes)
	if err != nil {
		return projectcontract.Lock{}, errors.Join(ErrInvalidCatalog, err)
	}
	lockBytes, err := packageio.ReadFileLimited(filepath.Join(projectRoot, "ailuo.lock"), projectcontract.MaxLockBytes)
	if err != nil {
		return projectcontract.Lock{}, errors.Join(ErrInvalidCatalog, err)
	}
	var lock projectcontract.Lock
	if err := packagecontract.DecodeStrictJSON(lockBytes, &lock); err != nil ||
		projectcontract.ValidateLockShape(lock) != nil || lock.ProjectManifestSHA256 != manifestSHA {
		return projectcontract.Lock{}, ErrInvalidCatalog
	}
	return lock, nil
}

type installedRecord struct {
	record       loader.InstalledRecord
	artifactPath string
	process      *packagecontract.ProcessSpec
}

func NewCatalog(root string) (*Catalog, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrInvalidCatalog
	}
	return &Catalog{root: root}, nil
}

// DiscoverLocked 按项目锁定结果读取运行时。未出现在 projectLock 中的安装包
// 永远不会进入 Core Registry；每个锁定包的版本、manifest 摘要、安装 lock 摘要
// 及其依赖闭包都在装载前重新验证。
func (c *Catalog) DiscoverLocked(ctx context.Context, projectLock projectcontract.Lock) ([]loader.InstalledRecord, error) {
	if c == nil || c.root == "" || projectcontract.ValidateLockShape(projectLock) != nil {
		return nil, ErrInvalidCatalog
	}
	if len(projectLock.Packages) > loader.MaxRegisteredRuntimes {
		return nil, ErrInvalidCatalog
	}
	if err := packageio.RecoverInstallRoot(ctx, c.root); err != nil {
		return nil, errors.Join(ErrInvalidCatalog, err)
	}
	lockedIDs := make(map[string]projectcontract.LockedPackage, len(projectLock.Packages))
	for _, locked := range projectLock.Packages {
		if _, duplicate := lockedIDs[locked.ID]; duplicate {
			return nil, ErrInvalidCatalog
		}
		lockedIDs[locked.ID] = locked
	}
	ids := make([]string, 0, len(lockedIDs))
	for id := range lockedIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	records := make([]loader.InstalledRecord, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		locked := lockedIDs[id]
		directory := filepath.Join(c.root, id)
		installed, err := packageio.ReadInstalled(ctx, directory)
		if err != nil {
			return nil, errors.Join(ErrInvalidCatalog, err)
		}
		if installed.Manifest.ID != id || installed.Manifest.Version != locked.Version {
			return nil, ErrInvalidCatalog
		}
		manifestSHA, err := packageio.HashFile(ctx, filepath.Join(directory, installManifestName), packagecontract.MaxManifestBytes)
		if err != nil || manifestSHA != locked.ManifestSHA256 {
			return nil, ErrInvalidCatalog
		}
		lockSHA, err := packageio.CanonicalLockDigest(ctx, directory, installed.Lock)
		if err != nil || lockSHA != locked.LockSHA256 {
			return nil, ErrInvalidCatalog
		}
		for _, dependency := range installed.Manifest.Dependencies {
			dependencyPackage, ok := lockedIDs[dependency.ID]
			if !ok {
				return nil, ErrInvalidCatalog
			}
			version, err := packagecontract.ParseVersion(dependencyPackage.Version)
			if err != nil {
				return nil, ErrInvalidCatalog
			}
			constraint, err := packagecontract.ParseConstraint(dependency.Constraint)
			if err != nil || !constraint.Matches(version) {
				return nil, ErrInvalidCatalog
			}
		}
		packageRecords, err := c.readPackage(ctx, directory)
		if err != nil {
			return nil, err
		}
		if len(packageRecords) == 0 || packageRecords[0].record.PackageID != id {
			return nil, ErrInvalidCatalog
		}
		for _, record := range packageRecords {
			records = append(records, record.record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Runtime.ID < records[j].Runtime.ID })
	return records, nil
}

func (c *Catalog) VerifyRuntime(ctx context.Context, manifest loader.Manifest) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !record.record.Runtime.Equal(manifest) {
		return ErrChanged
	}
	return nil
}

func (c *Catalog) ResolveProcess(ctx context.Context, manifest loader.Manifest) (packagecontract.ProcessSpec, error) {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return packagecontract.ProcessSpec{}, err
	}
	if !record.record.Runtime.Equal(manifest) || record.process == nil {
		return packagecontract.ProcessSpec{}, ErrChanged
	}
	return cloneProcessSpec(*record.process), nil
}

func (c *Catalog) VerifyProcess(ctx context.Context, manifest loader.Manifest, process packagecontract.ProcessSpec) error {
	record, err := c.readRecordByID(ctx, manifest.ID)
	if err != nil {
		return err
	}
	if !record.record.Runtime.Equal(manifest) || record.process == nil ||
		!reflect.DeepEqual(*record.process, process) {
		return ErrChanged
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
	if !record.record.Runtime.SameIdentity(manifest) {
		return nil, ErrChanged
	}
	if record.record.Runtime.Mode != loader.ModeHosted {
		return nil, loader.ErrUnsupportedMode
	}
	data, err := os.ReadFile(record.artifactPath)
	if err != nil {
		return nil, errors.Join(ErrInvalidCatalog, err)
	}
	if int64(len(data)) > packagecontract.MaxArtifactBytes {
		return nil, ErrInvalidCatalog
	}
	return data, nil
}

func (c *Catalog) readRecordByID(ctx context.Context, id string) (installedRecord, error) {
	if c == nil || !capability.IsStableID(id) {
		return installedRecord{}, ErrInvalidCatalog
	}
	if err := packageio.RecoverInstallRoot(ctx, c.root); err != nil {
		return installedRecord{}, errors.Join(ErrInvalidCatalog, err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return installedRecord{}, errors.Join(ErrInvalidCatalog, err)
	}
	if len(entries) > loader.MaxRegisteredRuntimes {
		return installedRecord{}, ErrInvalidCatalog
	}
	var matched installedRecord
	found := false
	for _, entry := range entries {
		if packageio.IsTransientInstallDirectory(entry.Name()) {
			if !entry.IsDir() {
				return installedRecord{}, ErrInvalidCatalog
			}
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") || !entry.IsDir() {
			return installedRecord{}, ErrInvalidCatalog
		}
		records, err := c.readPackage(ctx, filepath.Join(c.root, entry.Name()))
		if err != nil {
			return installedRecord{}, err
		}
		for _, record := range records {
			if record.record.Runtime.ID == id {
				if found {
					return installedRecord{}, ErrInvalidCatalog
				}
				matched = record
				found = true
			}
		}
	}
	if found {
		return matched, nil
	}
	return installedRecord{}, loader.ErrNotFound
}

// readPackage 读取一个包目录并产出每组件一条的内核记录。中性格式（manifest +
// lock + 每组件工件哈希）由 packageio.ReadInstalled 完成；本函数解析 AI珞
// 扩展段并按组件 exports 映射 Capability 到组件运行时。
func (c *Catalog) readPackage(ctx context.Context, directory string) ([]installedRecord, error) {
	neutral, err := packageio.ReadInstalled(ctx, directory)
	if err != nil {
		return nil, errors.Join(ErrInvalidCatalog, err)
	}
	var extensions aiLuoExtensions
	if len(neutral.Manifest.Extensions) > 0 {
		if err := packagecontract.DecodeStrictJSON(neutral.Manifest.Extensions, &extensions); err != nil {
			return nil, errors.Join(ErrInvalidCatalog, err)
		}
	}
	order, err := packagecontract.ComponentOrder(neutral.Manifest.Components)
	if err != nil {
		return nil, errors.Join(ErrInvalidCatalog, err)
	}
	orderIndex := make(map[string]int, len(order))
	for index, componentID := range order {
		orderIndex[componentID] = index
	}
	primaryComponentID := ""
	for _, componentID := range order {
		component, ok := packagecontract.FindComponent(neutral.Manifest, componentID)
		if ok && component.Role != packagecontract.RoleExecutor {
			primaryComponentID = componentID
			break
		}
	}
	artifactsByComponent := make(map[string]packagecontract.LockedArtifact, len(neutral.Lock.Artifacts))
	for _, artifact := range neutral.Lock.Artifacts {
		artifactsByComponent[artifact.ComponentID] = artifact
	}
	records := make([]installedRecord, 0, len(neutral.Manifest.Components))
	for _, component := range neutral.Manifest.Components {
		runtimeID := neutral.Manifest.ID + "." + component.ID
		if !capability.IsStableID(runtimeID) || len(runtimeID) > 128 {
			return nil, ErrInvalidCatalog
		}
		artifact, ok := artifactsByComponent[component.ID]
		if !ok {
			return nil, ErrInvalidCatalog
		}
		role := loader.RoleCapability
		if component.Role != "" {
			role = component.Role
		}
		runtimeManifest := loader.Manifest{
			ID: runtimeID, Version: neutral.Manifest.Version, Mode: component.Mode,
			Role: role, LockedDigest: artifact.SHA256,
			Pin: neutral.Manifest.Pin, IdleTTL: time.Duration(neutral.Manifest.IdleTTLMS) * time.Millisecond,
			HostFunctions: slices.Clone(component.HostFunctions),
			Storage:       cloneStorage(neutral.Manifest.Storage),
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
			Runtime: runtimeManifest, PackageID: neutral.Manifest.ID,
			ComponentID: component.ID, ComponentOrder: orderIndex[component.ID],
			Capabilities: capabilities,
		}
		// Service 与 Tools 只挂到包内第一个能力组件；执行者组件不进入
		// Registry，不能因为拓扑顺序靠前而吞掉包级能力面。
		if component.ID == primaryComponentID {
			record.Service = cloneInstalledService(extensions.Service)
			record.Tools = cloneToolSpecs(extensions.Tools)
		}
		if err := loader.ValidateInstalledRecord(record); err != nil {
			return nil, errors.Join(ErrInvalidCatalog, err)
		}
		var process *packagecontract.ProcessSpec
		if artifact.Process != nil {
			cloned := cloneProcessSpec(*artifact.Process)
			process = &cloned
		}
		records = append(records, installedRecord{
			record: record, artifactPath: artifact.Path, process: process,
		})
	}
	return records, nil
}

func cloneProcessSpec(spec packagecontract.ProcessSpec) packagecontract.ProcessSpec {
	spec.Args = slices.Clone(spec.Args)
	return spec
}

func cloneStorage(storage *packagecontract.Storage) *packagecontract.Storage {
	if storage == nil {
		return nil
	}
	cloned := *storage
	return &cloned
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
	spec.CapabilityImports = slices.Clone(spec.CapabilityImports)
	return spec
}
