package library

import (
	"testing"
	"time"
)

func TestParseAcademicDateRejectsNonCanonical(t *testing.T) {
	for _, value := range []string{"2026-9-2", "2026/09/02", "2026-09-02T00:00:00Z", "", "2026-13-01"} {
		if _, err := ParseAcademicDate(value); err == nil {
			t.Fatalf("ParseAcademicDate(%q) should fail", value)
		}
	}
	parsed, err := ParseAcademicDate("2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Format(AcademicDateLayout) != "2026-09-02" {
		t.Fatalf("parsed=%s", parsed)
	}
}

func TestSlotBoundsConvertsToUTC(t *testing.T) {
	slot := Slot{StartMinute: 8 * 60, EndMinute: 12 * 60}
	start, end, err := SlotBounds("2026-09-02", slot)
	if err != nil {
		t.Fatal(err)
	}
	// 上海 08:00 = UTC 同日 00:00（东八区领先）。
	if start.Format("2006-01-02T15:04:05Z") != "2026-09-02T00:00:00Z" {
		t.Fatalf("start=%s", start)
	}
	if end.Format("2006-01-02T15:04:05Z") != "2026-09-02T04:00:00Z" {
		t.Fatalf("end=%s", end)
	}
}

func TestSlotBoundsRejectsInvalidMinutes(t *testing.T) {
	for _, slot := range []Slot{{StartMinute: -1, EndMinute: 60}, {StartMinute: 60, EndMinute: 1441}, {StartMinute: 60, EndMinute: 60}} {
		if _, _, err := SlotBounds("2026-09-02", slot); err == nil {
			t.Fatalf("slot %+v should fail", slot)
		}
	}
}

func TestEffectiveStatusDerivesExpired(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reservation := Reservation{
		Status: ReservationConfirmed,
		EndsAt: start.Add(2 * time.Hour).Format(time.RFC3339),
	}
	if reservation.EffectiveStatus(start.Add(time.Hour)) != ReservationConfirmed {
		t.Fatal("within window should stay confirmed")
	}
	if reservation.EffectiveStatus(start.Add(3*time.Hour)) != ReservationExpired {
		t.Fatal("past window should derive expired")
	}
	cancelled := Reservation{Status: ReservationCancelled, EndsAt: start.Add(2 * time.Hour).Format(time.RFC3339)}
	if cancelled.EffectiveStatus(start.Add(3*time.Hour)) != ReservationCancelled {
		t.Fatal("cancelled should stay cancelled")
	}
}

func TestRequestValidationBoundaries(t *testing.T) {
	if err := (&SpacesListRequest{Limit: 51}).NormalizeAndValidate(); err == nil {
		t.Fatal("spaces limit 51 should fail")
	}
	if err := (&SpacesListRequest{}).NormalizeAndValidate(); err != nil {
		t.Fatalf("default spaces limit should pass: %v", err)
	}
	if err := (&SlotSearchRequest{SpaceID: " ", Date: "2026-09-02"}).NormalizeAndValidate(); err == nil {
		t.Fatal("blank space_id should fail")
	}
	if err := (&SlotSearchRequest{SpaceID: "s", Date: "2026-09-02", Limit: 201}).NormalizeAndValidate(); err == nil {
		t.Fatal("seat limit 201 should fail")
	}
	if err := (&ReservationCreateRequest{SpaceID: "s", SeatID: "", SlotID: "x", Date: "2026-09-02"}).NormalizeAndValidate(); err == nil {
		t.Fatal("missing seat_id should fail")
	}
	if err := (&ReservationCreateRequest{SpaceID: "s", SeatID: "a", SlotID: "x", Date: "2026-9-2"}).NormalizeAndValidate(); err == nil {
		t.Fatal("non-canonical date should fail")
	}
	if err := (&ReservationCancelRequest{}).NormalizeAndValidate(); err == nil {
		t.Fatal("missing reservation_id should fail")
	}
	if err := (&ReservationListRequest{Limit: 0}).NormalizeAndValidate(); err != nil {
		t.Fatalf("default reservations limit should pass: %v", err)
	}
	if err := (&ReservationListRequest{Limit: 51}).NormalizeAndValidate(); err == nil {
		t.Fatal("reservations limit 51 should fail")
	}
}
