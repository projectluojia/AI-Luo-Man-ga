package confirmation

import (
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
)

func TestValidateEffectTarget(t *testing.T) {
	for _, value := range []string{EffectState, EffectExternal} {
		if err := ValidateEffectTarget(value); err != nil {
			t.Fatalf("effect %q: %v", value, err)
		}
	}
	if err := ValidateEffectTarget("none"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("none error=%v", err)
	}
}

func TestValidateRequestRequiresGovernedEffectAndIdempotency(t *testing.T) {
	valid := runtime.ConfirmationRequest{
		AppID: "app", EchoID: "echo", RunID: "run", ConfirmationID: "confirmation",
		CapabilityID: "library.book.borrow", EffectTarget: EffectExternal, IdempotencyKey: "operation-1",
	}
	if err := ValidateRequest(valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*runtime.ConfirmationRequest){
		func(request *runtime.ConfirmationRequest) { request.EffectTarget = "none" },
		func(request *runtime.ConfirmationRequest) { request.IdempotencyKey = "bad key" },
	} {
		candidate := valid
		mutate(&candidate)
		if err := ValidateRequest(candidate); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid request error=%v", err)
		}
	}
}

func TestValidateConfirmationBindsDigestAndState(t *testing.T) {
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	digest, err := Digest([]byte(`{"book_id":"book-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	record := Confirmation{
		AppID: "app", ConfirmationID: "confirmation", EchoID: "echo", RunID: "run", CallID: "call",
		CapabilityID: "library.book.borrow", EffectTarget: EffectExternal, IdempotencyKey: "operation-1",
		ArgumentDigest: digest, Status: StatusWaiting, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := ValidateConfirmation(record); err != nil {
		t.Fatal(err)
	}
	record.EffectTarget = EffectState
	if err := ValidateConfirmation(record); err != nil {
		t.Fatal(err)
	}
	record.EffectTarget = EffectExternal
	record.Status = StatusApproved
	if err := ValidateConfirmation(record); err == nil {
		t.Fatal("approved confirmation without decision time accepted")
	}
}
