package packagecontract

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

func testLock() Lock {
	return Lock{
		SchemaVersion: SchemaVersion, PackageID: "pkg.test", PackageVersion: "1.0.0",
		ManifestSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Artifacts: []LockedArtifact{
			{ComponentID: "b", Path: "/install/pkg.test/b", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{ComponentID: "a", Path: "/install/pkg.test/a", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
	}
}

func TestSignAndVerifyRoundTrips(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lock := testLock()
	signature, err := Sign(lock, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	lock.Signature = &signature
	trusted := map[string]struct{}{hex.EncodeToString(publicKey): {}}
	if err := Verify(lock, trusted); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// 载荷对工件顺序稳定：乱序工件不破坏签名。
	flipped := lock
	flipped.Artifacts = []LockedArtifact{lock.Artifacts[1], lock.Artifacts[0]}
	if err := Verify(flipped, trusted); err != nil {
		t.Fatalf("verify reordered artifacts: %v", err)
	}
}

func TestVerifyRejectsUntrustedSigner(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lock := testLock()
	signature, err := Sign(lock, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	lock.Signature = &signature
	trusted := map[string]struct{}{hex.EncodeToString(otherPublic): {}}
	if err := Verify(lock, trusted); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("untrusted signer error=%v, want ErrInvalidSignature", err)
	}
	if err := Verify(lock, nil); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("empty trust error=%v, want ErrInvalidSignature", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lock := testLock()
	signature, err := Sign(lock, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	lock.Signature = &signature
	trusted := map[string]struct{}{hex.EncodeToString(publicKey): {}}
	lock.ManifestSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := Verify(lock, trusted); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered manifest error=%v, want ErrInvalidSignature", err)
	}
	lock = testLock()
	lock.Signature = &signature
	lock.Artifacts[0].SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := Verify(lock, trusted); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered artifact error=%v, want ErrInvalidSignature", err)
	}
}

func TestVerifyRejectsUnsignedLock(t *testing.T) {
	if err := Verify(testLock(), map[string]struct{}{"aa": {}}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("unsigned error=%v, want ErrInvalidSignature", err)
	}
}

func TestSignaturePayloadSortsArtifacts(t *testing.T) {
	lock := testLock()
	payload, err := lock.SignaturePayload()
	if err != nil {
		t.Fatal(err)
	}
	if payload.Artifacts[0].ComponentID != "a" || payload.Artifacts[1].ComponentID != "b" {
		t.Fatalf("payload order = %v", payload.Artifacts)
	}
}
