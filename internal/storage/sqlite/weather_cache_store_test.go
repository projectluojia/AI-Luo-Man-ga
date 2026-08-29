package sqlite_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/tools/weather"
)

func TestWeatherCacheIsAppIsolatedAndExpires(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "weather.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC)
	entry := weather.CacheEntry{
		Key: "openmeteo.forecast.abc", Provider: weather.ProviderOpenMeteo,
		Payload: json.RawMessage(`{"ok":true}`), SourceRevision: "rev-1",
		FetchedAt: now, ValidUntil: now.Add(10 * time.Minute),
	}
	if err := store.PutWeather(t.Context(), "campus-services", entry); err != nil {
		t.Fatal(err)
	}
	got, hit, err := store.GetWeather(t.Context(), "campus-services", entry.Key, now.Add(time.Minute))
	if err != nil || !hit || string(got.Payload) != `{"ok":true}` {
		t.Fatalf("got=%#v hit=%v err=%v", got, hit, err)
	}
	if _, hit, err := store.GetWeather(t.Context(), "other-app", entry.Key, now.Add(time.Minute)); err != nil || hit {
		t.Fatalf("cross-app hit=%v err=%v", hit, err)
	}
	if _, hit, err := store.GetWeather(t.Context(), "campus-services", entry.Key, now.Add(11*time.Minute)); err != nil || hit {
		t.Fatalf("expired hit=%v err=%v", hit, err)
	}
}
