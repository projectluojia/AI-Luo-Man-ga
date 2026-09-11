package sports

import (
	"testing"
	"time"
)

func TestRequestValidationBoundaries(t *testing.T) {
	if err := (&VenuesListRequest{Limit: 51}).NormalizeAndValidate(); err == nil {
		t.Fatal("venues limit 51 should fail")
	}
	if err := (&VenuesListRequest{}).NormalizeAndValidate(); err != nil {
		t.Fatalf("default venues limit should pass: %v", err)
	}
	if err := (&ProjectsListRequest{VenueID: " "}).NormalizeAndValidate(); err == nil {
		t.Fatal("blank venue_id should fail")
	}
	if err := (&SlotSearchRequest{VenueID: "v", ProjectID: "p", Date: "2026-9-2"}).NormalizeAndValidate(); err == nil {
		t.Fatal("non-canonical date should fail")
	}
	if err := (&SlotSearchRequest{VenueID: "v", ProjectID: "p", Date: "2026-09-02", Limit: 101}).NormalizeAndValidate(); err == nil {
		t.Fatal("slots limit 101 should fail")
	}
	if err := (&ReservationCreateRequest{VenueID: "v", ProjectID: "p", SlotID: "s"}).NormalizeAndValidate(); err != nil {
		t.Fatalf("default count=1 should pass: %v", err)
	}
	if err := (&ReservationCreateRequest{VenueID: "v", ProjectID: "p", SlotID: "s", Count: MaxCount + 1}).NormalizeAndValidate(); err == nil {
		t.Fatal("count over max should fail")
	}
	if err := (&ReservationCancelRequest{}).NormalizeAndValidate(); err == nil {
		t.Fatal("missing reservation_id should fail")
	}
	if err := (&ReservationListRequest{Limit: 51}).NormalizeAndValidate(); err == nil {
		t.Fatal("reservations limit 51 should fail")
	}
	if err := (&ScheduleAddRequest{ReservationID: " "}).NormalizeAndValidate(); err == nil {
		t.Fatal("blank schedule reservation_id should fail")
	}
}

func TestNormalizeWebViewDescriptor(t *testing.T) {
	valid := WebViewDescriptor{
		EntryURL: "https://orders.example.com/sports", RequiredUserAgent: "LuoJia/1.0",
		RequiredHeaders: []RequiredHeader{{Name: "X-App-Id", Purpose: "应用标识"}},
	}
	normalized, err := NormalizeWebViewDescriptor(valid)
	if err != nil {
		t.Fatalf("valid descriptor should pass: %v", err)
	}
	if len(normalized.RequiredHeaders) != 1 {
		t.Fatalf("headers=%+v", normalized.RequiredHeaders)
	}
	// 非 https 入口拒绝。
	bad := valid
	bad.EntryURL = "http://orders.example.com/sports"
	if _, err := NormalizeWebViewDescriptor(bad); err == nil {
		t.Fatal("http entry should fail")
	}
	// 凭据头拒绝。
	for _, name := range []string{"Cookie", "Authorization", "X-Access-Token"} {
		bad := valid
		bad.RequiredHeaders = []RequiredHeader{{Name: name, Purpose: "p"}}
		if _, err := NormalizeWebViewDescriptor(bad); err == nil {
			t.Fatalf("header %s should fail", name)
		}
	}
	// 重复头拒绝。
	bad = valid
	bad.RequiredHeaders = []RequiredHeader{{Name: "X-A", Purpose: "p1"}, {Name: "x-a", Purpose: "p2"}}
	if _, err := NormalizeWebViewDescriptor(bad); err == nil {
		t.Fatal("duplicate header should fail")
	}
	// 超过头数量上限拒绝。
	headers := make([]RequiredHeader, MaxHeaders+1)
	for index := range headers {
		headers[index] = RequiredHeader{Name: "X-H", Purpose: "p"}
	}
	bad = valid
	bad.RequiredHeaders = headers
	if _, err := NormalizeWebViewDescriptor(bad); err == nil {
		t.Fatal("over-max headers should fail")
	}
}

func TestEffectiveStatusDerivesExpired(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reservation := Reservation{
		Status: StatusConfirmed,
		EndsAt: start.Add(2 * time.Hour).Format(time.RFC3339),
	}
	if reservation.EffectiveStatus(start.Add(time.Hour)) != StatusConfirmed {
		t.Fatal("within window should stay confirmed")
	}
	if reservation.EffectiveStatus(start.Add(3*time.Hour)) != StatusExpired {
		t.Fatal("past window should derive expired")
	}
	cancelled := Reservation{Status: StatusCancelled, EndsAt: start.Add(2 * time.Hour).Format(time.RFC3339)}
	if cancelled.EffectiveStatus(start.Add(3*time.Hour)) != StatusCancelled {
		t.Fatal("cancelled should stay cancelled")
	}
}
