package timetable

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
)

func TestRegisterExposesAllTimetableCapabilitiesWithStrictSchemas(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "timetable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	reg := registry.New()
	if err := NewService(store).Register(reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Services()) != 1 || len(reg.Tools()) != 12 || len(reg.Capabilities()) != 12 {
		t.Fatalf("services=%d tools=%d capabilities=%d", len(reg.Services()), len(reg.Tools()), len(reg.Capabilities()))
	}
	if err := reg.ValidateCapabilityInput(ImportCapabilityID, []byte(`{"format":"csv","content":"x"}`)); err != nil {
		t.Fatalf("valid import input rejected: %v", err)
	}
	if err := reg.ValidateCapabilityInput(ImportCapabilityID, []byte(`{"format":"csv","content":"x","unexpected":true}`)); !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("unknown import field err=%v", err)
	}
}
