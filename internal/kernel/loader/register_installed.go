package loader

import (
	"context"
	"errors"
	"sort"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/pkg/capability"
)

const MaxRegisteredRuntimes = 256

var (
	ErrInvalidInstalledRecord = errors.New("invalid installed runtime record")
)

// InstalledRecord 是安装目录中单个组件（运行单元）的内核记录。
// 一个包产生多条记录（每组件一条）；Runtime.ID 是包命名空间内的稳定组件标识。
type InstalledRecord struct {
	Runtime     Manifest
	PackageID   string
	ComponentID string
	// ComponentOrder 是该组件在包内的依赖拓扑序号（Provider 小号在前）。
	ComponentOrder int
	Tools          []capability.ToolSpec
	Service        capability.ServiceSpec
	Capabilities   []capability.CapabilitySpec
}

func RegisterInstalled(ctx context.Context, manager *Manager, target *registry.Registry, records []InstalledRecord) error {
	if manager == nil || target == nil || len(records) == 0 || len(records) > MaxRegisteredRuntimes {
		return ErrInvalidInstalledRecord
	}
	for _, record := range records {
		if err := ValidateInstalledRecord(record); err != nil {
			return err
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
				Spec    capability.CapabilitySpec
				Handler registry.Handler
			})
		}
		if record.ComponentOrder == 0 {
			entry.Spec = record.Service
		}
		for _, spec := range record.Capabilities {
			entry.Capabilities[spec.ID] = struct {
				Spec    capability.CapabilitySpec
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

func ValidateInstalledRecord(record InstalledRecord) error {
	// executor 记录不携带能力面：会话客户端经 executor 契约取用，
	// 不进 Registry——注册管线对其只校验运行时清单。
	if record.Runtime.Role == RoleExecutor {
		if err := ValidateManifest(record.Runtime); err != nil {
			return errors.Join(ErrInvalidInstalledRecord, err)
		}
		return nil
	}
	if len(record.Capabilities) == 0 {
		return ErrInvalidInstalledRecord
	}
	return validateRecordSpecs(record)
}

// validateRecordSpecs 校验内置包与 installed 包共用的规格契约：运行时清单、
// 宿主函数声明，以及 Tool/Service/Capability 与运行时版本一致。
// 运行时专用记录（如内置 agent）不携带 Service 规格，只校验运行时清单与声明。
func validateRecordSpecs(record InstalledRecord) error {
	if err := ValidateManifest(record.Runtime); err != nil {
		return errors.Join(ErrInvalidInstalledRecord, err)
	}
	if record.Service.ID != "" {
		if record.Service.Version != record.Runtime.Version {
			return ErrInvalidInstalledRecord
		}
		for _, tool := range record.Tools {
			if tool.Version != record.Runtime.Version {
				return ErrInvalidInstalledRecord
			}
		}
	}
	for _, capability := range record.Capabilities {
		if capability.Version != record.Runtime.Version {
			return ErrInvalidInstalledRecord
		}
	}
	return nil
}
