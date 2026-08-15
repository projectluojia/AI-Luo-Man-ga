package qq

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	qqsettings "github.com/projectluojia/AI-Luo-Man-ga/internal/access/qq/settings"
)

type managerTestRunner struct{ stopped chan struct{} }

func (r *managerTestRunner) Run(ctx context.Context) error {
	<-ctx.Done()
	close(r.stopped)
	return nil
}

type managerTestStore struct {
	value qqsettings.Settings
	count atomic.Int32
}

func (s *managerTestStore) EnsureQQSettings(_ context.Context, seed qqsettings.Settings) (qqsettings.Settings, bool, error) {
	if s.value.AppID == "" {
		normalized, err := qqsettings.Normalize(seed)
		if err != nil {
			return qqsettings.Settings{}, false, err
		}
		normalized.Generation = 1
		s.value = normalized
		return normalized, true, nil
	}
	return s.value, false, nil
}
func (s *managerTestStore) CurrentQQSettings(context.Context, string) (qqsettings.Settings, error) {
	return s.value, nil
}
func (s *managerTestStore) CompareAndSwapQQSettings(_ context.Context, generation uint64, replacement qqsettings.Settings) (qqsettings.Settings, error) {
	if generation != s.value.Generation {
		return qqsettings.Settings{}, qqsettings.ErrConflict
	}
	replacement.Generation++
	s.value = replacement
	return replacement, nil
}

func TestManagerAppliesUpdatesWithoutRestartingGoProcess(t *testing.T) {
	store := &managerTestStore{}
	var created atomic.Int32
	runners := make(chan *managerTestRunner, 2)
	manager, err := NewManager(store, func(settings qqsettings.Settings, _ func(bool)) (Runner, error) {
		created.Add(1)
		runner := &managerTestRunner{stopped: make(chan struct{})}
		runners <- runner
		return runner, nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Start(ctx, qqsettings.Settings{AppID: "campus-services"}); err != nil {
		t.Fatal(err)
	}
	settings, status, err := manager.Snapshot(ctx)
	if err != nil || status.Running || settings.Generation != 1 {
		t.Fatalf("settings=%#v status=%#v err=%v", settings, status, err)
	}
	replacement := settings
	replacement.Enabled = true
	replacement.WSURL = "ws://127.0.0.1:3001"
	replacement.BotQQID = "2647414417"
	replacement.AllowedGroupIDs = []string{"12345"}
	_, _, err = manager.Update(ctx, settings.Generation, replacement)
	if err != nil {
		t.Fatal(err)
	}
	first := <-runners
	current, _, _ := manager.Snapshot(ctx)
	secondReplacement := current
	secondReplacement.AllowedGroupIDs = []string{"54321"}
	if _, _, err := manager.Update(ctx, current.Generation, secondReplacement); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.stopped:
	case <-time.After(time.Second):
		t.Fatal("旧适配器没有停止")
	}
	if created.Load() != 2 {
		t.Fatalf("created=%d", created.Load())
	}
	if err := manager.Shutdown(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
