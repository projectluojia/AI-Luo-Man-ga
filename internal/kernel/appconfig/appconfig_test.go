package appconfig_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
)

func TestNormalizeProducesCanonicalContentRevision(t *testing.T) {
	config := validConfig()
	config.CapabilityGrants = grants("campus.bus.stops.search", "campus.bus.routes.list")
	first, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	config.CapabilityGrants = grants("campus.bus.routes.list", "campus.bus.stops.search")
	second, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != second.Revision || first.Revision == "" ||
		first.CapabilityGrants[0].CapabilityID != "campus.bus.routes.list" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	second.ExecutorConfig = json.RawMessage(`{"strategy":"other"}`)
	second, err = appconfig.Normalize(second)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == second.Revision {
		t.Fatal("different opaque executor configuration reused a revision")
	}
	config = validConfig()
	config.CapabilityGrants = grants("institution.bus.read")
	empty, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if empty.CapabilityGrants == nil {
		t.Fatalf("empty collections were not canonical JSON arrays: %#v", empty)
	}
}

func TestNormalizeRejectsUnsafeOrUnboundedConfiguration(t *testing.T) {
	tests := []appconfig.Config{
		func() appconfig.Config { value := validConfig(); value.AppID = "../other"; return value }(),
		func() appconfig.Config { value := validConfig(); value.ExecutorID = "executor\nsecret"; return value }(),
		func() appconfig.Config { value := validConfig(); value.ExecutorID = "executor/other"; return value }(),
		func() appconfig.Config {
			value := validConfig()
			value.ExecutorConfig = json.RawMessage(`{`)
			return value
		}(),
		func() appconfig.Config { value := validConfig(); value.MaxSteps = 65; return value }(),
		func() appconfig.Config { value := validConfig(); value.MaxExecutionUnits = 0; return value }(),
		func() appconfig.Config {
			value := validConfig()
			value.CapabilityGrants = grants("a.capability", "a.capability")
			return value
		}(),
		func() appconfig.Config {
			value := validConfig()
			value.CapabilityGrants = []capability.Grant{{
				ID: strings.Repeat("a", 129), AppID: "campus-services", Principal: capability.PrincipalAny,
				CapabilityID: "a.capability", Resource: capability.ResourceScope{Type: "capability.resource"},
				ExpiresAt: time.Now().Add(time.Hour), MaxCalls: 1,
			}}
			return value
		}(),
	}
	for _, config := range tests {
		if _, err := appconfig.Normalize(config); err == nil {
			t.Fatalf("invalid config accepted: %#v", config)
		}
	}
}

func TestOpaqueExecutorConfigEntersRevisionDigest(t *testing.T) {
	config := validConfig()
	first, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	config.ExecutorConfig = json.RawMessage(`{"strategy":"workflow","steps":[]}`)
	second, err := appconfig.Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision == second.Revision {
		t.Fatal("opaque executor configuration change did not change revision")
	}
}

func TestVerifyRejectsMismatchedIdentityAndRevisionContent(t *testing.T) {
	config, err := appconfig.Normalize(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	config.Generation = 1
	if err := appconfig.VerifyCurrent(config, config.AppID); err != nil {
		t.Fatal(err)
	}
	withoutGeneration := config
	withoutGeneration.Generation = 0
	if err := appconfig.VerifyCurrent(withoutGeneration, config.AppID); err == nil {
		t.Fatal("current configuration without a generation passed verification")
	}
	if err := appconfig.Verify(config, config.AppID, config.Revision); err != nil {
		t.Fatal(err)
	}
	if err := appconfig.Verify(config, "other-app", config.Revision); err == nil {
		t.Fatal("mismatched App identity passed verification")
	}
	if err := appconfig.Verify(config, config.AppID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched expected revision passed verification")
	}
	config.ExecutorID = "executor.other"
	if err := appconfig.Verify(config, config.AppID, config.Revision); err == nil {
		t.Fatal("tampered revision content passed verification")
	}
	snapshot := appconfig.Snapshot(config)
	snapshot.AppID = "other-app"
	if err := snapshot.Verify("campus-services"); err == nil {
		t.Fatal("mismatched policy snapshot passed verification")
	}
	snapshot = appconfig.PolicySnapshot{
		AppID: "campus-services", Revision: "static", Generation: 1, Enabled: true,
		CapabilityGrants: []capability.Grant{grant("z"), grant("a")},
	}
	if err := snapshot.Verify("campus-services"); err == nil {
		t.Fatal("non-canonical policy snapshot passed verification")
	}
}

func validConfig() appconfig.Config {
	return appconfig.Config{
		AppID: "campus-services", Enabled: true, ExecutorID: "executor.test",
		ExecutorConfig: json.RawMessage(`{"strategy":"test"}`),
		MaxSteps:       8, MaxCapabilityCalls: 8, MaxExecutionUnits: 40960,
		MaxOutputBytes: 65536, ExecutionTimeout: 30 * time.Second,
		CapabilityGrants: grants("campus.bus.routes.list"),
	}
}

func grant(capabilityID string) capability.Grant {
	return capability.Grant{
		ID: "grant-" + capabilityID, AppID: "campus-services", Principal: capability.PrincipalAny,
		CapabilityID: capabilityID, Resource: capability.ResourceScope{Type: "capability.resource"},
		ExpiresAt: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), MaxCalls: 100,
		PolicyRevision: "test",
	}
}

func grants(ids ...string) []capability.Grant {
	result := make([]capability.Grant, 0, len(ids))
	for _, id := range ids {
		result = append(result, grant(id))
	}
	return result
}
