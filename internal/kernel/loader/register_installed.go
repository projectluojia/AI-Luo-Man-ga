package loader

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

const MaxRegisteredRuntimes = 256

var (
	ErrInvalidInstalledRecord = errors.New("invalid installed runtime record")
)

// InstalledRecord 是安装目录中单个组件的内核记录。PackageID 和 ComponentID
// 只用于绑定依赖拓扑与运行时身份；组件对外只发布 Capability。
type InstalledRecord struct {
	Runtime     Manifest
	PackageID   string
	ComponentID string
	// ComponentOrder 是组件在 Package 清单中的声明顺序。
	ComponentOrder int
}

// RegisterInstalled 在一次装配流程中校验并注册安装包的所有运行时和 Capability。
func RegisterInstalled(ctx context.Context, manager *Manager, target *registry.Registry, records []InstalledRecord) error {
	if manager == nil || target == nil || len(records) == 0 || len(records) > MaxRegisteredRuntimes {
		return ErrInvalidInstalledRecord
	}
	if err := validatePackageGroups(records); err != nil {
		return err
	}
	manifests := make([]Manifest, 0, len(records))
	registrations := make([]registry.CapabilityRegistration, 0)
	for _, record := range records {
		if err := ValidateInstalledRecord(record); err != nil {
			return err
		}
		manifests = append(manifests, record.Runtime)
		handler := manager.Handler(record.Runtime.ID)
		for _, spec := range record.Runtime.Capabilities {
			registrations = append(registrations, registry.CapabilityRegistration{
				Spec: spec, Handler: handler,
			})
		}
	}
	if err := manager.RegisterBatch(ctx, manifests); err != nil {
		return err
	}
	groups := make(map[string][]componentWithOrder)
	for _, record := range records {
		groups[record.PackageID] = append(groups[record.PackageID], componentWithOrder{
			id: record.Runtime.ID, order: record.ComponentOrder,
		})
	}
	packageIDs := make([]string, 0, len(groups))
	packageComponents := make(map[string][]string, len(groups))
	for packageID, components := range groups {
		sort.Slice(components, func(i, j int) bool { return components[i].order < components[j].order })
		ordered := make([]string, 0, len(components))
		for index, component := range components {
			if component.order != index {
				return errors.Join(ErrInvalidInstalledRecord, manager.rollbackRegistered(manifests))
			}
			ordered = append(ordered, component.id)
		}
		packageIDs = append(packageIDs, packageID)
		packageComponents[packageID] = ordered
	}
	if err := manager.RegisterPackages(packageComponents); err != nil {
		return errors.Join(err, manager.rollbackRegistered(manifests))
	}
	if len(registrations) > 0 {
		if err := target.RegisterBatch(registrations); err != nil {
			return errors.Join(err, manager.rollbackPackageRegistration(manifests, packageIDs))
		}
	}
	return nil
}

func validatePackageGroups(records []InstalledRecord) error {
	groups := make(map[string]map[string]struct{})
	orders := make(map[string]map[int]struct{})
	for _, record := range records {
		if !capability.IsStableID(record.PackageID) || !capability.IsStableID(record.ComponentID) || record.ComponentOrder < 0 {
			return ErrInvalidInstalledRecord
		}
		if groups[record.PackageID] == nil {
			groups[record.PackageID] = make(map[string]struct{})
			orders[record.PackageID] = make(map[int]struct{})
		}
		if _, duplicate := groups[record.PackageID][record.ComponentID]; duplicate {
			return fmt.Errorf("%w: package %q component %q 重复", ErrInvalidInstalledRecord, record.PackageID, record.ComponentID)
		}
		if _, duplicate := orders[record.PackageID][record.ComponentOrder]; duplicate {
			return fmt.Errorf("%w: package %q component order %d 重复", ErrInvalidInstalledRecord, record.PackageID, record.ComponentOrder)
		}
		groups[record.PackageID][record.ComponentID] = struct{}{}
		orders[record.PackageID][record.ComponentOrder] = struct{}{}
	}
	return nil
}

type componentWithOrder struct {
	id    string
	order int
}

func ValidateInstalledRecord(record InstalledRecord) error {
	if !capability.IsStableID(record.PackageID) || !capability.IsStableID(record.ComponentID) {
		return ErrInvalidInstalledRecord
	}
	if record.Runtime.PackageID != "" && record.Runtime.PackageID != record.PackageID {
		return ErrInvalidInstalledRecord
	}
	if err := ValidateManifest(record.Runtime); err != nil {
		return errors.Join(ErrInvalidInstalledRecord, err)
	}
	if record.Runtime.Role == RoleExecutor {
		if len(record.Runtime.Capabilities) > 0 {
			return ErrInvalidInstalledRecord
		}
		return nil
	}
	if len(record.Runtime.Capabilities) == 0 {
		return ErrInvalidInstalledRecord
	}
	for _, spec := range record.Runtime.Capabilities {
		if spec.Version != record.Runtime.Version {
			return ErrInvalidInstalledRecord
		}
	}
	return nil
}
