package libraryseat

import (
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

func TestGovernRejectsIncompleteExpiredAndUntrusted(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	base := SnapshotMetadata{
		Revision: "rev-1", Source: "official", Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}
	if status, err := base.Govern(now); err != nil || status.State != DataStateAuthoritativeFresh {
		t.Fatalf("authoritative got %#v err=%v", status, err)
	}
	incomplete := base
	incomplete.Revision = ""
	if _, err := incomplete.Govern(now); !errors.Is(err, contracts.ErrDataIncomplete) {
		t.Fatalf("incomplete err=%v", err)
	}
	expired := base
	expired.ValidUntil = now
	if _, err := expired.Govern(now); !errors.Is(err, contracts.ErrDataExpired) {
		t.Fatalf("expired err=%v", err)
	}
	untrusted := base
	untrusted.Authoritative = false
	untrusted.Source = "scraped-copy"
	if _, err := untrusted.Govern(now); !errors.Is(err, contracts.ErrDataUntrusted) {
		t.Fatalf("untrusted err=%v", err)
	}
	demo := base
	demo.Authoritative = false
	demo.Source = DemoSource
	if status, err := demo.Govern(now); err != nil || status.State != DataStateDemoNonAuthoritative || status.Authoritative {
		t.Fatalf("demo got %#v err=%v", status, err)
	}
}

func TestValidateCatalogAcceptsHyphenatedIDs(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	err := ValidateCatalog(CatalogSnapshot{
		AppID: "campus-services", Revision: "rev-campus-services", Source: DemoSource,
		Authoritative: false, Complete: true, ImportedAt: now, ValidUntil: now.Add(time.Hour),
		Spaces: []Space{{ID: "space-a", Name: "演示阅览室", SourceRevision: "rev-campus-services"}},
		Seats:  []Seat{{ID: "seat-1", SpaceID: "space-a", Label: "A01", SourceRevision: "rev-campus-services"}},
		Slots:  []Slot{{ID: "slot-morning", Name: "上午", StartMinute: 480, EndMinute: 720, SourceRevision: "rev-campus-services"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseLibraryDateUsesShanghaiCalendar(t *testing.T) {
	day, err := ParseLibraryDate("2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	location, err := TimeLocation()
	if err != nil {
		t.Fatal(err)
	}
	if day.Location().String() != location.String() || day.Hour() != 0 || day.Format("2006-01-02") != "2026-09-01" {
		t.Fatalf("day=%s loc=%s", day, day.Location())
	}
	if _, err := ParseLibraryDate("2026-02-31"); err == nil {
		t.Fatal("invalid calendar date must fail")
	}
	start, end, err := SlotBounds("2026-09-01", 8*60, 12*60)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(time.Date(2026, 9, 1, 8, 0, 0, 0, location).UTC()) || !end.After(start) {
		t.Fatalf("bounds start=%s end=%s", start, end)
	}
}
