package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/identity"
	promptservice "github.com/projectluojia/AI-Luo-Man-ga/internal/services/prompt"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestPromptSettingsStoreRoundTripAndAppIsolation(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "prompt-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identityService := identity.NewService(store)
	if _, err := identityService.CreateUser(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePromptSettings(ctx, "campus-services", promptservice.Settings{
		UserID:     "user-1",
		BasicStyle: "专业可靠",
		ExtraTraitLevels: map[string]string{
			"表情符号":  "减弱",
			"标题和列表": "增强",
		},
	}); err != nil {
		t.Fatal(err)
	}
	settings, err := store.GetPromptSettings(ctx, "campus-services", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if settings.UserID != "user-1" || settings.BasicStyle != "professional" ||
		settings.ExtraTraitLevels["emoji"] != "reduced" ||
		settings.ExtraTraitLevels["headings_lists"] != "enhanced" {
		t.Fatalf("settings=%#v", settings)
	}
	if _, err := store.GetPromptSettings(ctx, "other-app", "user-1"); !errors.Is(err, promptservice.ErrNotFound) {
		t.Fatalf("cross-app error=%v", err)
	}
	if err := store.DeletePromptSettings(ctx, "campus-services", "user-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPromptSettings(ctx, "campus-services", "user-1"); !errors.Is(err, promptservice.ErrNotFound) {
		t.Fatalf("after delete error=%v", err)
	}
	if err := store.DeletePromptSettings(ctx, "campus-services", "user-1"); !errors.Is(err, promptservice.ErrNotFound) {
		t.Fatalf("second delete error=%v", err)
	}
}

func TestPromptSettingsStoreRejectsUnknownUser(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "prompt-settings-user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.SavePromptSettings(t.Context(), "campus-services", promptservice.Settings{
		UserID: "ghost-user", BasicStyle: "default",
	})
	if err == nil {
		t.Fatal("saving settings for unknown user succeeded")
	}
}
