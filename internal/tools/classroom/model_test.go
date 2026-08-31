package classroom

import (
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
)

func TestSnapshotMetadataGovernFailClosed(t *testing.T) {
	now := time.Date(2026, time.August, 31, 8, 0, 0, 0, time.UTC)
	fresh := SnapshotMetadata{
		Revision: "rev-1", Source: "zhihui-luojia-authorized", Authoritative: true, Complete: true,
		ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
	}
	status, err := fresh.Govern(now)
	if err != nil || status.State != DataStateAuthoritativeFresh {
		t.Fatalf("fresh=%#v err=%v", status, err)
	}
	cases := []struct {
		name     string
		metadata SnapshotMetadata
		want     error
	}{
		{
			name: "demo untrusted",
			metadata: SnapshotMetadata{
				Revision: "demo", Source: DemoSource, Complete: true,
				ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
			},
			want: contracts.ErrDataUntrusted,
		},
		{
			name: "expired",
			metadata: SnapshotMetadata{
				Revision: "exp", Source: "zhihui-luojia-authorized", Authoritative: true, Complete: true,
				ImportedAt: now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Minute),
			},
			want: contracts.ErrDataExpired,
		},
		{
			name: "incomplete",
			metadata: SnapshotMetadata{
				Revision: "inc", Source: "zhihui-luojia-authorized", Authoritative: true,
				ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
			},
			want: contracts.ErrDataIncomplete,
		},
		{
			name: "missing source",
			metadata: SnapshotMetadata{
				Revision: "rev", Authoritative: true, Complete: true,
				ImportedAt: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour),
			},
			want: contracts.ErrDataIncomplete,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.metadata.Govern(now); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
		})
	}
}

func TestParseAcademicDateUsesShanghaiCalendar(t *testing.T) {
	parsed, err := ParseAcademicDate("2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	location, err := time.LoadLocation(AcademicTimezone)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Location().String() != location.String() || parsed.Hour() != 0 || parsed.Format(AcademicDateLayout) != "2026-08-31" {
		t.Fatalf("parsed=%v", parsed)
	}
	if _, err := ParseAcademicDate("2026-02-31"); !errors.Is(err, ErrInvalidDate) {
		t.Fatalf("rolled date err=%v", err)
	}
	if _, err := ParseAcademicDate("26-08-31"); !errors.Is(err, ErrInvalidDate) {
		t.Fatalf("short date err=%v", err)
	}
}

func TestSearchRequestRejectsUnknownCampusAndPeriod(t *testing.T) {
	request := SearchRequest{Date: "2026-08-31", CampusID: "campus-wenli", Period: 14}
	if err := request.NormalizeAndValidate(); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("period err=%v", err)
	}
	request = SearchRequest{Date: "2026-08-31", Period: 1}
	if err := request.NormalizeAndValidate(); !errors.Is(err, ErrCampusRequired) {
		t.Fatalf("campus err=%v", err)
	}
}

func TestRequireUserDoesNotInventAnonymous(t *testing.T) {
	if err := RequireUser(contracts.RequestContext{}); !errors.Is(err, ErrUserRequired) || !errors.Is(err, registry.ErrSchemaValidation) {
		t.Fatalf("err=%v", err)
	}
}
