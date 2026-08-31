package ecard

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestParseAndRoundTripAES256GCM(t *testing.T) {
	t.Parallel()
	raw := bytes.Repeat([]byte{0x3c}, AES256KeySize)
	key, err := ParseAES256Key(raw)
	if err != nil {
		t.Fatal(err)
	}
	hexKey, err := ParseAES256Key([]byte(hex.EncodeToString(raw)))
	if err != nil || !bytes.Equal(key, hexKey) {
		t.Fatalf("hex key mismatch err=%v", err)
	}
	nonce, ciphertext, err := encryptMaterial(key, "campus-services", "user-1", KindCASCookie, []byte("delegated-material"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != GCMNonceSize || bytes.Contains(ciphertext, []byte("delegated-material")) {
		t.Fatalf("ciphertext leaked plaintext")
	}
	plain, err := decryptMaterial(key, nonce, ciphertext, "campus-services", "user-1", KindCASCookie)
	if err != nil || string(plain) != "delegated-material" {
		t.Fatalf("roundtrip=%q err=%v", plain, err)
	}
	if _, err := decryptMaterial(key, nonce, ciphertext, "other-app", "user-1", KindCASCookie); err == nil {
		t.Fatal("cross-app associated data must not decrypt")
	}
	if _, err := ParseAES256Key([]byte("too-short")); err != ErrKeyInvalid {
		t.Fatalf("short key err=%v", err)
	}
	spaced := append([]byte{' '}, bytes.Repeat([]byte{0x3c}, AES256KeySize-1)...)
	parsed, err := ParseAES256Key(spaced)
	if err != nil || !bytes.Equal(parsed, spaced) {
		t.Fatalf("raw key with leading space was altered err=%v", err)
	}
}

func TestEncryptRejectsOversizedMaterial(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x11}, AES256KeySize)
	if _, _, err := encryptMaterial(key, "campus-services", "user-1", KindCASCookie, make([]byte, MaxMaterialBytes+1)); err != ErrInvalid {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}
