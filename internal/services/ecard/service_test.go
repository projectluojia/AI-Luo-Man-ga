package ecard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime/runtimetest"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/services/campus"
	ecardservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/ecard"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	ecard "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/ecard"
)

var testKey = bytes.Repeat([]byte{0x5a}, ecard.AES256KeySize)

func TestECardDualEntriesAndDemoSessionPlan(t *testing.T) {
	harness := newHarness(t, ecard.Config{DemoMode: true, Key: testKey, Now: frozenNow})
	listed, err := harness.invoke(readRequest("user-1"), ecardservice.EntriesListCapabilityID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	entries := listed["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries=%v", listed)
	}
	ids := map[string]map[string]any{}
	for _, item := range entries {
		entry := item.(map[string]any)
		ids[entry["id"].(string)] = entry
		if entry["required_user_agent_purpose"] != ecard.UserAgentPurposeSmartCampus {
			t.Fatalf("ua purpose=%v", entry)
		}
		headers := entry["required_header_names"].([]any)
		if len(headers) != 2 {
			t.Fatalf("headers=%v", headers)
		}
		if entry["data_status"].(map[string]any)["authoritative"] != false {
			t.Fatalf("demo list must be non-authoritative: %v", entry["data_status"])
		}
	}
	if ids[ecard.EntryIDLuoJiaECard]["title"] != "珞珈E卡" || ids[ecard.EntryIDPayCode]["title"] != "付款码" {
		t.Fatalf("titles=%v", ids)
	}

	expires := frozenNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	put, err := harness.invoke(writeRequest("user-1", "put-1"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"demo_handle","credential_material":"demo:fixture-handle","expires_at":"`+expires+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if put["credential_handle"] != ecard.KindDemoHandle || put["has_credential"] != true {
		t.Fatalf("put=%v", put)
	}
	plan, err := harness.invoke(readRequest("user-1"), ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"luojia_ecard","credential_handle":"demo_handle"}`)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(plan)
	if bytes.Contains(encoded, []byte("demo:fixture-handle")) || bytes.Contains(encoded, []byte("CASTGC=")) {
		t.Fatalf("prepare leaked secret: %s", encoded)
	}
	if plan["entry_url"] != ecard.DemoECardEntryURL || plan["user_agent"] != ecard.DemoUserAgent {
		t.Fatalf("plan=%v", plan)
	}
	cookieNames := plan["cookie_names"].([]any)
	if len(cookieNames) == 0 {
		t.Fatal("cookie names missing")
	}
}

func TestECardProductionFailClosedWithoutCredential(t *testing.T) {
	harness := newHarness(t, ecard.Config{Production: true, Key: testKey, Now: frozenNow})
	listed, err := harness.invoke(readRequest("user-1"), ecardservice.EntriesListCapabilityID, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if listed["entries"].([]any)[0].(map[string]any)["data_status"].(map[string]any)["authoritative"] != false {
		t.Fatalf("production catalog must not claim authority: %v", listed)
	}
	_, err = harness.invoke(readRequest("user-1"), ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"ecard_paycode","credential_handle":"cas_cookie"}`)
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("prepare without credential err=%v", err)
	}
}

func TestECardMissingUserIsDenied(t *testing.T) {
	harness := newHarness(t, ecard.Config{Key: testKey, Now: frozenNow})
	expires := frozenNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	_, err := harness.invoke(writeRequest("", "put-missing"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"delegated-value","expires_at":"`+expires+`"}`)
	if !errors.Is(err, registry.ErrPermissionDenied) || !errors.Is(err, ecard.ErrUserRequired) {
		t.Fatalf("put missing user err=%v", err)
	}
	_, err = harness.invoke(readRequest(""), ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"luojia_ecard","credential_handle":"cas_cookie"}`)
	if !errors.Is(err, registry.ErrPermissionDenied) {
		t.Fatalf("prepare missing user err=%v", err)
	}
	_, err = harness.invoke(writeRequest("", "revoke-missing"), ecardservice.CredentialsRevokeCapabilityID, `{"kind":"cas_cookie"}`)
	if !errors.Is(err, registry.ErrPermissionDenied) {
		t.Fatalf("revoke missing user err=%v", err)
	}
}

func TestECardAppIsolationAndRevoke(t *testing.T) {
	harness := newHarness(t, ecard.Config{Key: testKey, Now: frozenNow})
	expires := frozenNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	secret := "CASTGC=TGT-secret-ticket-value"
	_, err := harness.invoke(writeRequest("user-1", "put-iso"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"`+secret+`","expires_at":"`+expires+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	other := readRequest("user-1")
	other.AppID = "other-app"
	_, err = harness.invoke(other, ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"luojia_ecard","credential_handle":"cas_cookie"}`)
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("cross-app prepare err=%v", err)
	}
	status, err := harness.invoke(readRequest("user-1"), ecardservice.CredentialsStatusCapabilityID, `{"kind":"cas_cookie"}`)
	if err != nil || status["has_credential"] != true {
		t.Fatalf("status=%v err=%v", status, err)
	}
	if _, err := harness.invoke(writeRequest("user-1", "revoke-1"), ecardservice.CredentialsRevokeCapabilityID, `{"kind":"cas_cookie"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.invoke(writeRequest("user-1", "revoke-2"), ecardservice.CredentialsRevokeCapabilityID, `{"kind":"cas_cookie"}`); err != nil {
		t.Fatal(err)
	}
	status, err = harness.invoke(readRequest("user-1"), ecardservice.CredentialsStatusCapabilityID, `{"kind":"cas_cookie"}`)
	if err != nil || status["has_credential"] != false {
		t.Fatalf("revoked status=%v err=%v", status, err)
	}
	_, err = harness.invoke(readRequest("user-1"), ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"luojia_ecard","credential_handle":"cas_cookie"}`)
	if !errors.Is(err, contracts.ErrDataUnavailable) {
		t.Fatalf("prepare after revoke err=%v", err)
	}
	stored, err := harness.store.GetECardCredentialMeta(t.Context(), campus.AppID, "user-1", ecard.KindCASCookie)
	if err != nil || !stored.Revoked {
		t.Fatalf("stored meta=%#v err=%v", stored, err)
	}
}

func TestECardExpiredCredentialFailsPrepare(t *testing.T) {
	now := frozenNow()
	clock := now
	harness := newHarness(t, ecard.Config{Key: testKey, Now: func() time.Time { return clock }})
	expires := now.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := harness.invoke(writeRequest("user-1", "put-exp"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"delegated-value","expires_at":"`+expires+`"}`); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Hour)
	_, err := harness.invoke(readRequest("user-1"), ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"ecard_paycode","credential_handle":"cas_cookie"}`)
	if !errors.Is(err, contracts.ErrDataExpired) {
		t.Fatalf("expired prepare err=%v", err)
	}
}

func TestECardPrepareNeverReturnsPlaintextAndDemoRejectsRealCookies(t *testing.T) {
	demo := newHarness(t, ecard.Config{DemoMode: true, Key: testKey, Now: frozenNow})
	expires := frozenNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	_, err := demo.invoke(writeRequest("user-1", "put-real"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"CASTGC=TGT-real","expires_at":"`+expires+`"}`)
	if !errors.Is(err, contracts.ErrDataUntrusted) {
		t.Fatalf("demo accepted real cookie err=%v", err)
	}
	prod := newHarness(t, ecard.Config{Production: true, Key: testKey, Now: frozenNow})
	secret := "CASTGC=TGT-unique-secret-xyz"
	if _, err := prod.invoke(writeRequest("user-1", "put-prod"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"`+secret+`","expires_at":"`+expires+`"}`); err != nil {
		t.Fatal(err)
	}
	active, err := prod.store.GetActiveECardCredential(t.Context(), campus.AppID, "user-1", ecard.KindCASCookie)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(active.Ciphertext, []byte(secret)) {
		t.Fatal("plaintext cookie persisted")
	}
	plan, err := prod.invoke(readRequest("user-1"), ecardservice.SessionPrepareCapabilityID,
		`{"entry_id":"ecard_paycode","credential_handle":"cas_cookie"}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("TGT-unique-secret-xyz")) {
		t.Fatalf("prepare JSON leaked cookie: %s", raw)
	}
	if plan["entry_url"] != ecard.PublicCASEntryURL {
		t.Fatalf("production entry_url=%v", plan["entry_url"])
	}
	_, err = prod.invoke(writeRequest("user-1", "put-demo"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"demo_handle","credential_material":"demo:x","expires_at":"`+expires+`"}`)
	if !errors.Is(err, contracts.ErrDataUntrusted) {
		t.Fatalf("production accepted demo handle err=%v", err)
	}
}

func TestECardProductionWithoutKeyFailsClosed(t *testing.T) {
	harness := newHarness(t, ecard.Config{Production: true, Now: frozenNow})
	expires := frozenNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	_, err := harness.invoke(writeRequest("user-1", "put-nokey"), ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"delegated-value","expires_at":"`+expires+`"}`)
	if !errors.Is(err, contracts.ErrDataUnavailable) && !errors.Is(err, ecard.ErrKeyUnavailable) {
		t.Fatalf("put without key err=%v", err)
	}
}

func TestECardConfirmationRequiredOnWrite(t *testing.T) {
	harness := newHarness(t, ecard.Config{Key: testKey, Now: frozenNow})
	expires := frozenNow().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	request := writeRequest("user-1", "put-unconfirmed")
	request.ConfirmationID = ""
	_, err := harness.invoke(request, ecardservice.CredentialsPutCapabilityID,
		`{"kind":"cas_cookie","credential_material":"delegated-value","expires_at":"`+expires+`"}`)
	if !errors.Is(err, runtime.ErrConfirmationRequired) {
		t.Fatalf("unconfirmed put err=%v", err)
	}
}

type harness struct {
	t          *testing.T
	store      *sqlite.Store
	dispatcher *runtime.Dispatcher
}

func newHarness(t *testing.T, cfg ecard.Config) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "ecard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlitetest.CloseAndWait(t, store, dir) })
	if _, err := store.CreateUser(t.Context(), identity.User{UserID: "user-1", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	cfg.Store = store
	reg := registry.New()
	policy := runtimetest.NewStaticAppPolicy()
	for _, id := range ecardservice.CapabilityIDs() {
		policy.Enable(campus.AppID, id)
		policy.Enable("other-app", id)
	}
	dispatcher := runtime.NewDispatcher(reg, policy, runtime.DispatcherConfig{
		IdempotencyStore: store,
		ConfirmationVerifier: verifierFunc(func(context.Context, runtime.ConfirmationRequest) error {
			return nil
		}),
	})
	if err := ecardservice.Register(reg, ecardservice.NewService(cfg)); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, store: store, dispatcher: dispatcher}
}

func (h *harness) invoke(request contracts.RequestContext, capabilityID, payload string) (map[string]any, error) {
	h.t.Helper()
	raw, err := h.dispatcher.InvokeCapability(h.t.Context(), request, capabilityID, json.RawMessage(payload))
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		h.t.Fatal(err)
	}
	if strings.Contains(string(raw), "CASTGC=") {
		h.t.Fatalf("secret leaked in %s result: %s", capabilityID, raw)
	}
	return decoded, nil
}

func frozenNow() time.Time {
	return time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
}

func readRequest(userID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: campus.AppID, EchoID: "echo", RequestID: "req-read", UserID: userID,
		Deadline: time.Now().Add(time.Minute),
	}
}

func writeRequest(userID, key string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: campus.AppID, EchoID: "echo", RequestID: "req-" + key, UserID: userID,
		IdempotencyKey: key, ConfirmationID: "confirm-" + key,
		Deadline: time.Now().Add(time.Minute),
	}
}

type verifierFunc func(context.Context, runtime.ConfirmationRequest) error

func (f verifierFunc) VerifyConfirmation(ctx context.Context, request runtime.ConfirmationRequest) error {
	return f(ctx, request)
}
