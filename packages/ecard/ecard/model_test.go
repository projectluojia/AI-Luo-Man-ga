package ecard

import (
	"testing"
	"time"
)

func TestRequestValidationBoundaries(t *testing.T) {
	if err := (&SessionPrepareRequest{EntryID: "unknown", CredentialID: "c1"}).NormalizeAndValidate(); err == nil {
		t.Fatal("unknown entry should fail")
	}
	if err := (&SessionPrepareRequest{EntryID: EntryIDEcard}).NormalizeAndValidate(); err == nil {
		t.Fatal("missing credential_id should fail")
	}
	if err := (&SessionPrepareRequest{EntryID: EntryIDPay, CredentialID: "c1"}).NormalizeAndValidate(); err != nil {
		t.Fatalf("valid prepare should pass: %v", err)
	}
	if err := (&CredentialsPutRequest{Kind: "cas_cookie", Material: "demo:x"}).NormalizeAndValidate(); err == nil {
		t.Fatal("real credential kind should fail")
	}
	if err := (&CredentialsPutRequest{Kind: KindDemoHandle, Material: "CASTGC-abc"}).NormalizeAndValidate(); err == nil {
		t.Fatal("material without demo: prefix should fail")
	}
	if err := (&CredentialsPutRequest{Kind: KindDemoHandle, Material: "demo:castgc"}).NormalizeAndValidate(); err == nil {
		t.Fatal("material that looks like real CAS credential should fail")
	}
	if err := (&CredentialsPutRequest{Kind: KindDemoHandle, Material: "demo:cas.whu.edu.cn"}).NormalizeAndValidate(); err == nil {
		t.Fatal("material containing CAS host should fail")
	}
	if err := (&CredentialsPutRequest{Kind: KindDemoHandle, Material: "demo:ST-12345"}).NormalizeAndValidate(); err == nil {
		t.Fatal("material containing ST- ticket should fail")
	}
	if err := (&CredentialsPutRequest{Kind: KindDemoHandle, Material: "demo:handle-1"}).NormalizeAndValidate(); err != nil {
		t.Fatalf("valid demo material should pass: %v", err)
	}
	// TTL 边界：缺省 24h，超出 7 天拒绝。
	request := &CredentialsPutRequest{Kind: KindDemoHandle, Material: "demo:handle-1"}
	if err := request.NormalizeAndValidate(); err != nil || request.TTLHours != DefaultTTLHours {
		t.Fatalf("default ttl = %d err=%v", request.TTLHours, err)
	}
	if err := (&CredentialsPutRequest{Kind: KindDemoHandle, Material: "demo:h", TTLHours: MaxTTLHours + 1}).NormalizeAndValidate(); err == nil {
		t.Fatal("over-max ttl should fail")
	}
	if err := (&CredentialsRevokeRequest{}).NormalizeAndValidate(); err == nil {
		t.Fatal("missing credential_id should fail")
	}
	if err := (&CredentialsStatusRequest{CredentialID: " "}).NormalizeAndValidate(); err == nil {
		t.Fatal("blank credential_id should fail")
	}
}

func TestEffectiveStatusDerivesExpired(t *testing.T) {
	now := time.Now().UTC()
	expired := Credential{Status: StatusActive, ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)}
	if expired.EffectiveStatus(now) != StatusExpired {
		t.Fatal("past expiry should derive expired")
	}
	active := Credential{Status: StatusActive, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	if active.EffectiveStatus(now) != StatusActive {
		t.Fatal("future expiry should stay active")
	}
	revoked := Credential{Status: StatusRevoked, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	if revoked.EffectiveStatus(now) != StatusRevoked {
		t.Fatal("revoked should stay revoked")
	}
}

func TestDemoEntriesAreExplicitlyNonAuthoritative(t *testing.T) {
	entries, status := DemoEntries(time.Now())
	if len(entries) != 2 {
		t.Fatalf("entries=%d", len(entries))
	}
	if status.Authoritative || status.State != "non_authoritative" {
		t.Fatalf("status=%+v", status)
	}
	for _, entry := range entries {
		if entry.EntryURL == "" || !entry.RequiresDelegatedAuth {
			t.Fatalf("entry=%+v", entry)
		}
	}
}
