package sqlite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/appconfig"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestAppConfigEnsureIsAppScopedAndKeepsImmutableRevisions(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "app-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed := validAppConfig("app-one")
	created, inserted, err := store.Ensure(t.Context(), seed)
	if err != nil || !inserted || created.Generation != 1 {
		t.Fatalf("created=%#v inserted=%t err=%v", created, inserted, err)
	}
	changedSeed := seed
	changedSeed.ExecutorID = "executor.other"
	existing, inserted, err := store.Ensure(t.Context(), changedSeed)
	if err != nil || inserted || existing.Revision != created.Revision {
		t.Fatalf("existing=%#v inserted=%t err=%v", existing, inserted, err)
	}
	if _, err := store.Current(t.Context(), "app-two"); !errors.Is(err, appconfig.ErrNotFound) {
		t.Fatalf("cross-app current error=%v", err)
	}

	replacement := existing
	replacement.ExecutorConfig = json.RawMessage(`{"strategy":"changed"}`)
	updated, err := store.CompareAndSwap(t.Context(), existing.Generation, replacement)
	if err != nil || updated.Generation != 2 || updated.Revision == existing.Revision {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	old, err := store.Revision(t.Context(), "app-one", existing.Revision)
	if err != nil || old.ExecutorID != seed.ExecutorID || string(old.ExecutorConfig) != string(seed.ExecutorConfig) {
		t.Fatalf("old=%#v err=%v", old, err)
	}
	if _, err := store.Revision(t.Context(), "app-two", existing.Revision); !errors.Is(err, appconfig.ErrNotFound) {
		t.Fatalf("cross-app revision error=%v", err)
	}
	current, err := store.Current(t.Context(), "app-one")
	if err != nil || current.Revision != updated.Revision || string(current.ExecutorConfig) != string(replacement.ExecutorConfig) {
		t.Fatalf("current=%#v err=%v", current, err)
	}
	unchanged, err := store.CompareAndSwap(t.Context(), updated.Generation, replacement)
	if err != nil || unchanged.Generation != updated.Generation {
		t.Fatalf("unchanged=%#v err=%v", unchanged, err)
	}
}

func TestAppConfigCompareAndSwapAllowsOnlyOneConcurrentWriter(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "app-config-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	current, _, err := store.Ensure(t.Context(), validAppConfig("app"))
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for _, executorID := range []string{"executor.a", "executor.b"} {
		workers.Add(1)
		go func(value string) {
			defer workers.Done()
			replacement := current
			replacement.ExecutorID = value
			_, updateErr := store.CompareAndSwap(context.Background(), current.Generation, replacement)
			results <- updateErr
		}(executorID)
	}
	workers.Wait()
	close(results)
	var succeeded, conflicted int
	for result := range results {
		switch {
		case result == nil:
			succeeded++
		case errors.Is(result, appconfig.ErrConflict):
			conflicted++
		default:
			t.Fatalf("unexpected update error=%v", result)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	current, err = store.Current(t.Context(), "app")
	if err != nil || current.Generation != 2 {
		t.Fatalf("current=%#v err=%v", current, err)
	}
}

func TestAppConfigReadRejectsRevisionContentTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-config-tamper.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Ensure(t.Context(), validAppConfig("app"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE app_config_revisions SET executor_config='{"tampered":true}' WHERE app_id=? AND revision=?`, current.AppID, current.Revision); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.Current(t.Context(), "app"); !errors.Is(err, appconfig.ErrInvalid) {
		t.Fatalf("tampered revision error=%v", err)
	}
}

func TestAppConfigRejectsMalformedBoundariesBeforePersistence(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "app-config-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	config := validAppConfig("app")
	config.PermissionScope = []string{"private.read", "private.read"}
	if _, _, err := store.Ensure(t.Context(), config); !errors.Is(err, appconfig.ErrInvalid) {
		t.Fatalf("invalid config error=%v", err)
	}
	if _, err := store.Current(t.Context(), "../app"); !errors.Is(err, appconfig.ErrInvalid) {
		t.Fatalf("invalid app id error=%v", err)
	}
	if _, err := store.Revision(t.Context(), "app", "not-a-revision"); !errors.Is(err, appconfig.ErrInvalid) {
		t.Fatalf("invalid revision error=%v", err)
	}
}

func validAppConfig(appID string) appconfig.Config {
	return appconfig.Config{
		AppID: appID, Enabled: true, ExecutorID: "executor.test",
		ExecutorConfig: json.RawMessage(`{"strategy":"test"}`), MaxSteps: 8, MaxCapabilityCalls: 8,
		MaxExecutionUnits: 40960, MaxOutputBytes: 65536, MaxCostMicrousd: 0,
		ExecutionTimeout: 30 * time.Second, EnabledCapabilities: []string{"campus.bus.routes.list"},
		PermissionScope: []string{"bus.read"},
	}
}
