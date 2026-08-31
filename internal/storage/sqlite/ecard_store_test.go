package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite/sqlitetest"
	ecard "github.com/projectluojia/AI-Luo-Man-ga/internal/tools/ecard"
)

func TestECardMigration28CreatesCiphertextSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ecard.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version, tables, indexes int
	if err := db.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 28 {
		t.Fatalf("schema 版本=%d，期望至少 28", version)
	}
	if err := db.QueryRowContext(t.Context(), `
SELECT
  (SELECT count(*) FROM sqlite_master WHERE type='table' AND name='ecard_credentials'),
  (SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN
    ('ecard_credentials_active_idx','ecard_credentials_lookup_idx'))`).Scan(&tables, &indexes); err != nil {
		t.Fatal(err)
	}
	if tables != 1 || indexes != 2 {
		t.Fatalf("ecard 表=%d 索引=%d，期望 1 表 2 索引", tables, indexes)
	}
	var applied int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM schema_migrations WHERE version=28`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migration 28 applied=%d err=%v", applied, err)
	}
}

func TestECardStorePersistsCiphertextOnlyAndIsolatesApps(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "ecard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	ctx := context.Background()
	if _, err := store.CreateUser(ctx, identity.User{UserID: "user-1", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	plain := []byte("CASTGC=secret-cookie-value")
	record, err := store.PutECardCredential(ctx, ecard.CredentialRecord{
		AppID: "campus-services", UserID: "user-1", Kind: ecard.KindCASCookie,
		Nonce:      bytes.Repeat([]byte{0x01}, ecard.GCMNonceSize),
		Ciphertext: append(bytes.Repeat([]byte{0x02}, 32), []byte("not-plain")...),
		CreatedAt:  now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetActiveECardCredential(ctx, "campus-services", "user-1", ecard.KindCASCookie)
	if err != nil || !bytes.Equal(got.Ciphertext, record.Ciphertext) {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	if bytes.Contains(got.Ciphertext, plain) || bytes.Contains(got.Nonce, plain) {
		t.Fatal("plaintext cookie written to sqlite")
	}
	if _, err := store.GetActiveECardCredential(ctx, "other-app", "user-1", ecard.KindCASCookie); !errors.Is(err, ecard.ErrNotFound) {
		t.Fatalf("cross-app err=%v", err)
	}
	if err := store.RevokeECardCredential(ctx, "campus-services", "user-1", ecard.KindCASCookie, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeECardCredential(ctx, "campus-services", "user-1", ecard.KindCASCookie, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetActiveECardCredential(ctx, "campus-services", "user-1", ecard.KindCASCookie); !errors.Is(err, ecard.ErrNotFound) {
		t.Fatalf("revoked err=%v", err)
	}
	meta, err := store.GetECardCredentialMeta(ctx, "campus-services", "user-1", ecard.KindCASCookie)
	if err != nil || !meta.Present || !meta.Revoked {
		t.Fatalf("meta=%#v err=%v", meta, err)
	}
}

func TestECardStorePutIsIdempotentForActiveRow(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "ecard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	ctx := context.Background()
	if _, err := store.CreateUser(ctx, identity.User{UserID: "user-1", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	first := bytes.Repeat([]byte{0x11}, 32)
	if _, err := store.PutECardCredential(ctx, ecard.CredentialRecord{
		AppID: "campus-services", UserID: "user-1", Kind: ecard.KindCASCookie,
		Nonce: bytes.Repeat([]byte{0x01}, ecard.GCMNonceSize), Ciphertext: first,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	second := bytes.Repeat([]byte{0x22}, 32)
	if _, err := store.PutECardCredential(ctx, ecard.CredentialRecord{
		AppID: "campus-services", UserID: "user-1", Kind: ecard.KindCASCookie,
		Nonce: bytes.Repeat([]byte{0x03}, ecard.GCMNonceSize), Ciphertext: second,
		CreatedAt: now.Add(time.Minute), ExpiresAt: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetActiveECardCredential(ctx, "campus-services", "user-1", ecard.KindCASCookie)
	if err != nil || !bytes.Equal(got.Ciphertext, second) {
		t.Fatalf("idempotent replace got=%#v err=%v", got, err)
	}
}

func TestECardStoreRejectsMissingUser(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "ecard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	_, err = store.PutECardCredential(context.Background(), ecard.CredentialRecord{
		AppID: "campus-services", UserID: "user-missing", Kind: ecard.KindCASCookie,
		Nonce: bytes.Repeat([]byte{0x01}, ecard.GCMNonceSize), Ciphertext: bytes.Repeat([]byte{0x02}, 32),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if !errors.Is(err, ecard.ErrUserRequired) {
		t.Fatalf("missing user err=%v", err)
	}
}

func TestECardStoreHonorsContextCancel(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "ecard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlitetest.CloseAndWait(t, store, dir)
	if _, err := store.CreateUser(context.Background(), identity.User{UserID: "user-1", Status: identity.UserStatusActive}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	_, err = store.PutECardCredential(ctx, ecard.CredentialRecord{
		AppID: "campus-services", UserID: "user-1", Kind: ecard.KindCASCookie,
		Nonce: bytes.Repeat([]byte{0x01}, ecard.GCMNonceSize), Ciphertext: bytes.Repeat([]byte{0x02}, 32),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context err=%v", err)
	}
}
