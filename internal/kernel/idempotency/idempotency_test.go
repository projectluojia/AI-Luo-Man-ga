package idempotency_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/idempotency"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/storage/sqlite"
)

func TestManagerMergesConcurrentDuplicatesAndReplaysResult(t *testing.T) {
	t.Parallel()

	store := openStore(t)
	manager := idempotency.NewManager(store)
	operation := testOperation("app", "same-key", "payload")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	type outcome struct {
		result   []byte
		replayed bool
		err      error
	}
	firstResult := make(chan outcome, 1)
	go func() {
		result, replayed, err := manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
			calls.Add(1)
			close(started)
			<-release
			return []byte(`{"ok":true}`), nil
		})
		firstResult <- outcome{result: result, replayed: replayed, err: err}
	}()
	<-started
	secondResult := make(chan outcome, 1)
	go func() {
		result, replayed, err := manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"wrong":true}`), nil
		})
		secondResult <- outcome{result: result, replayed: replayed, err: err}
	}()
	close(release)

	first := <-firstResult
	second := <-secondResult
	if first.err != nil || second.err != nil {
		t.Fatalf("first error=%v second error=%v", first.err, second.err)
	}
	if calls.Load() != 1 {
		t.Fatalf("execution count=%d, want 1", calls.Load())
	}
	if !bytes.Equal(first.result, []byte(`{"ok":true}`)) || !bytes.Equal(second.result, first.result) {
		t.Fatalf("results differ: first=%s second=%s", first.result, second.result)
	}
	if first.replayed == second.replayed {
		t.Fatalf("exactly one call must be a replay: first=%t second=%t", first.replayed, second.replayed)
	}

	replayed, wasReplay, err := manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("must not run")
	})
	if err != nil || !wasReplay || !bytes.Equal(replayed, first.result) || calls.Load() != 1 {
		t.Fatalf("durable replay result=%s replayed=%t calls=%d err=%v", replayed, wasReplay, calls.Load(), err)
	}
}

func TestManagerRejectsConflictsAndDoesNotRepeatFailedOrUncertainEffects(t *testing.T) {
	t.Parallel()

	store := openStore(t)
	manager := idempotency.NewManager(store)
	operation := testOperation("app", "terminal-key", "payload")
	var calls atomic.Int32
	handlerFailure := errors.New("handler failed after a possible side effect")
	if _, _, err := manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, handlerFailure
	}); !errors.Is(err, handlerFailure) {
		t.Fatalf("first failure=%v, want handler failure", err)
	}
	if _, replayed, err := manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	}); !replayed || !errors.Is(err, idempotency.ErrPreviousFailure) {
		t.Fatalf("failed replay replayed=%t err=%v", replayed, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("failed operation executed %d times", calls.Load())
	}

	conflict := operation
	conflict.Fingerprint = idempotency.Fingerprint([]byte("different"))
	if _, _, err := manager.Execute(context.Background(), conflict, func(context.Context) ([]byte, error) {
		return nil, nil
	}); !errors.Is(err, idempotency.ErrKeyConflict) {
		t.Fatalf("conflicting key error=%v, want ErrKeyConflict", err)
	}

	unknown := testOperation("app", "unknown-key", "payload")
	now := time.Now().UTC()
	if _, claimed, err := store.BeginIdempotent(context.Background(), idempotency.Claim{
		Operation:      unknown,
		LeaseToken:     "abandoned-lease",
		LeaseExpiresAt: now.Add(time.Millisecond),
	}, now); err != nil || !claimed {
		t.Fatalf("seed abandoned operation claimed=%t err=%v", claimed, err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, replayed, err := manager.Execute(context.Background(), unknown, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return nil, nil
	}); !replayed || !errors.Is(err, idempotency.ErrOutcomeUnknown) {
		t.Fatalf("unknown outcome replayed=%t err=%v", replayed, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("uncertain operation was executed again, calls=%d", calls.Load())
	}
}

func TestManagerScopesKeysByAppAndRejectsOversizedResults(t *testing.T) {
	t.Parallel()

	store := openStore(t)
	manager := idempotency.NewManager(store)
	var calls atomic.Int32
	for _, appID := range []string{"app-a", "app-b"} {
		operation := testOperation(appID, "shared-key", "payload")
		if _, replayed, err := manager.Execute(context.Background(), operation, func(context.Context) ([]byte, error) {
			calls.Add(1)
			return []byte(appID), nil
		}); err != nil || replayed {
			t.Fatalf("App %s execution replayed=%t err=%v", appID, replayed, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("cross-App key collision, calls=%d", calls.Load())
	}

	oversized := testOperation("app-a", "oversized-key", "payload")
	if _, _, err := manager.Execute(context.Background(), oversized, func(context.Context) ([]byte, error) {
		return bytes.Repeat([]byte{'x'}, 300<<10), nil
	}); !errors.Is(err, idempotency.ErrResultTooLarge) {
		t.Fatalf("oversized result error=%v, want ErrResultTooLarge", err)
	}
	if _, replayed, err := manager.Execute(context.Background(), oversized, func(context.Context) ([]byte, error) {
		return []byte("must-not-run"), nil
	}); !replayed || !errors.Is(err, idempotency.ErrPreviousFailure) {
		t.Fatalf("oversized replay replayed=%t err=%v", replayed, err)
	}
}

func testOperation(appID, key, payload string) idempotency.Operation {
	return idempotency.Operation{
		AppID:       appID,
		Scope:       "test.operation/v1",
		Key:         key,
		Fingerprint: idempotency.Fingerprint([]byte(payload)),
		OwnerID:     "test-owner",
	}
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}
