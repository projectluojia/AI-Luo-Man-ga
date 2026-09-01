package loader_test

import (
	"context"
	"errors"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestRegisterInstalledRejectsCapabilityPackageWithoutUniquePrimaryService(t *testing.T) {
	cases := []struct {
		name    string
		records []loader.InstalledRecord
	}{
		{
			name: "missing primary",
			records: []loader.InstalledRecord{capabilityRecord(
				"broken.missing", "broken-package", "component", 1, "broken.missing.cap", capability.ServiceSpec{},
			)},
		},
		{
			name: "duplicate primary",
			records: []loader.InstalledRecord{
				capabilityRecord("broken.one", "broken-package", "one", 0, "broken.one.cap", capability.ServiceSpec{ID: "broken", Version: "1.2.3"}),
				capabilityRecord("broken.two", "broken-package", "two", 0, "broken.two.cap", capability.ServiceSpec{ID: "broken", Version: "1.2.3"}),
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manager, err := loader.New(&fakeHost{})
			if err != nil {
				t.Fatal(err)
			}
			if err := loader.RegisterInstalled(context.Background(), manager, registry.New(), test.records); !errors.Is(err, loader.ErrInvalidInstalledRecord) {
				t.Fatalf("RegisterInstalled error=%v, want ErrInvalidInstalledRecord", err)
			}
			if err := manager.EnsureLoaded(context.Background(), test.records[0].Runtime.ID); !errors.Is(err, loader.ErrNotFound) {
				t.Fatalf("invalid package was registered in Loader: %v", err)
			}
		})
	}
}

func capabilityRecord(runtimeID, packageID, componentID string, order int, capabilityID string, service capability.ServiceSpec) loader.InstalledRecord {
	return loader.InstalledRecord{
		Runtime: loader.Manifest{
			ID: runtimeID, Version: "1.2.3", Mode: loader.ModeHosted,
			Role: loader.RoleCapability, LockedDigest: digest,
		},
		PackageID: packageID, ComponentID: componentID, ComponentOrder: order,
		Service: service,
		Capabilities: []capability.CapabilitySpec{{
			ID: capabilityID, Version: "1.2.3", ServiceID: service.ID,
			InputSchemaJSON: `{"type":"object","additionalProperties":false}`,
			SideEffect:      capability.SideEffectRead,
		}},
	}
}
