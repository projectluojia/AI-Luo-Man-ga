package libraryseat

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	libraryseattool "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/libraryseat"
)

func TestRegisterExposesLibrarySeatCapabilitiesWithConfirmationOnWrites(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "library-seat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	reg := registry.New()
	if err := NewService(store).Register(reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Services()) != 1 || len(reg.Tools()) != 5 || len(reg.Capabilities()) != 5 {
		t.Fatalf("services=%d tools=%d capabilities=%d", len(reg.Services()), len(reg.Tools()), len(reg.Capabilities()))
	}
	for _, spec := range reg.Capabilities() {
		switch spec.ID {
		case ReservationsCreateCapabilityID, ReservationsCancelCapabilityID:
			if spec.SideEffect != registry.SideEffectWrite || !spec.RequiresConfirmation {
				t.Fatalf("write capability %s confirmation=%v side=%s", spec.ID, spec.RequiresConfirmation, spec.SideEffect)
			}
		default:
			if spec.SideEffect != registry.SideEffectRead || spec.RequiresConfirmation {
				t.Fatalf("read capability %s confirmation=%v side=%s", spec.ID, spec.RequiresConfirmation, spec.SideEffect)
			}
		}
	}
	for _, spec := range reg.Tools() {
		switch spec.ID {
		case libraryseattool.ReservationsCreateToolID, libraryseattool.ReservationsCancelToolID:
			if spec.SideEffect != registry.SideEffectWrite || !spec.RequiresConfirmation {
				t.Fatalf("write tool %s confirmation=%v side=%s", spec.ID, spec.RequiresConfirmation, spec.SideEffect)
			}
		default:
			if spec.SideEffect != registry.SideEffectRead || spec.RequiresConfirmation {
				t.Fatalf("read tool %s confirmation=%v side=%s", spec.ID, spec.RequiresConfirmation, spec.SideEffect)
			}
		}
	}
	if err := reg.ValidateCapabilityInput(SlotsSearchCapabilityID, []byte(`{"space_id":"space-a","date":"2026-09-01"}`)); err != nil {
		t.Fatalf("valid search rejected: %v", err)
	}
	if err := reg.ValidateCapabilityInput(SlotsSearchCapabilityID, []byte(`{"space_id":"space-a","date":"2026-09-01","unexpected":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown field err=%v", err)
	}
}
