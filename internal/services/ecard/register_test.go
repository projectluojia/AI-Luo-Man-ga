package ecard

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	ecardtool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/ecard"
)

func TestRegisterExposesECardCapabilitiesWithConfirmationAndStrictSchemas(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "ecard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	reg := registry.New()
	if err := NewService(ecardtool.Config{Store: store, Key: bytes.Repeat([]byte{0x5a}, 32)}).Register(reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Services()) != 1 || len(reg.Tools()) != 5 || len(reg.Capabilities()) != 5 {
		t.Fatalf("services=%d tools=%d capabilities=%d", len(reg.Services()), len(reg.Tools()), len(reg.Capabilities()))
	}
	put, err := capabilitySpec(reg, CredentialsPutCapabilityID)
	if err != nil {
		t.Fatal(err)
	}
	if put.SideEffect != registry.SideEffectWrite || !put.RequiresConfirmation {
		t.Fatalf("put spec=%#v", put)
	}
	revoke, err := capabilitySpec(reg, CredentialsRevokeCapabilityID)
	if err != nil {
		t.Fatal(err)
	}
	if revoke.SideEffect != registry.SideEffectWrite || !revoke.RequiresConfirmation {
		t.Fatalf("revoke spec=%#v", revoke)
	}
	prepare, err := capabilitySpec(reg, SessionPrepareCapabilityID)
	if err != nil {
		t.Fatal(err)
	}
	if prepare.SideEffect != registry.SideEffectRead || prepare.RequiresConfirmation {
		t.Fatalf("prepare spec=%#v", prepare)
	}
	if err := reg.ValidateCapabilityInput(EntriesListCapabilityID, []byte(`{}`)); err != nil {
		t.Fatalf("valid list input rejected: %v", err)
	}
	if err := reg.ValidateCapabilityInput(EntriesListCapabilityID, []byte(`{"unexpected":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown list field err=%v", err)
	}
	if err := reg.ValidateCapabilityInput(CredentialsPutCapabilityID, []byte(`{"kind":"cas_cookie","credential_material":"x","expires_at":"2026-09-01T00:00:00Z","extra":1}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown put field err=%v", err)
	}
	oversize := `{"kind":"cas_cookie","credential_material":"` + string(bytes.Repeat([]byte{'a'}, ecardtool.MaxMaterialBytes+1)) + `","expires_at":"2026-09-01T00:00:00Z"}`
	if err := reg.ValidateCapabilityInput(CredentialsPutCapabilityID, []byte(oversize)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("oversize put err=%v", err)
	}
}

func capabilitySpec(reg *registry.Registry, id string) (registry.CapabilitySpec, error) {
	for _, spec := range reg.Capabilities() {
		if spec.ID == id {
			return spec, nil
		}
	}
	return registry.CapabilitySpec{}, errors.New("capability not registered")
}
