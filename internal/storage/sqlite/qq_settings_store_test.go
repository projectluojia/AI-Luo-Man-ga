package sqlite_test

import (
	"errors"
	"path/filepath"
	"testing"

	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestQQSettingsPersistAndUseGenerationCAS(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "qq-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := qqsettings.Settings{AppID: "campus-services"}
	current, created, err := store.EnsureQQSettings(t.Context(), seed)
	if err != nil || !created || current.Generation != 1 || current.Enabled {
		t.Fatalf("current=%#v created=%t err=%v", current, created, err)
	}
	replacement := current
	replacement.Enabled = true
	replacement.WSURL = "ws://127.0.0.1:3001"
	replacement.BotQQID = "2647414417"
	replacement.AllowedGroupIDs = []string{"12345"}
	updated, err := store.CompareAndSwapQQSettings(t.Context(), current.Generation, replacement)
	if err != nil || updated.Generation != 2 || !updated.Enabled {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if _, err := store.CompareAndSwapQQSettings(t.Context(), current.Generation, replacement); !errors.Is(err, qqsettings.ErrConflict) {
		t.Fatalf("stale update error=%v", err)
	}
	reloaded, err := store.CurrentQQSettings(t.Context(), "campus-services")
	if err != nil || len(reloaded.AllowedGroupIDs) != 1 || reloaded.AllowedGroupIDs[0] != "12345" {
		t.Fatalf("reloaded=%#v err=%v", reloaded, err)
	}
}
