package echo

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type shutdownOrderRunner struct {
	started  chan struct{}
	finished chan struct{}
	returned atomic.Bool
}

func (r *shutdownOrderRunner) Recoverable(context.Context) ([]RunWork, error) {
	return nil, nil
}

func (r *shutdownOrderRunner) Runnable(context.Context, int) ([]RunWork, error) {
	if !r.returned.CompareAndSwap(false, true) {
		return nil, nil
	}
	return []RunWork{{Run: RunRecord{ID: "run", EchoID: "echo"}}}, nil
}

func (r *shutdownOrderRunner) RunQueued(ctx context.Context, _ RunWork, _ EventEmitter) error {
	close(r.started)
	<-ctx.Done()
	close(r.finished)
	return ctx.Err()
}

func (r *shutdownOrderRunner) Cancel(context.Context, string) (bool, error) {
	return false, nil
}

func (r *shutdownOrderRunner) CancelQueuedRuns(context.Context) error {
	select {
	case <-r.finished:
		return nil
	default:
		return errors.New("queued cancellation started before active Run stopped")
	}
}

type shutdownOrderReader struct{}

func (shutdownOrderReader) GetEcho(context.Context, string, string) (Record, []Event, error) {
	return Record{Status: StatusCancelled}, nil, nil
}

type shutdownOrderEvents struct{}

func (shutdownOrderEvents) Publish(Event)         {}
func (shutdownOrderEvents) Finish(string, string) {}

type pollingShutdownRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *pollingShutdownRunner) Recoverable(context.Context) ([]RunWork, error) {
	return nil, nil
}

func (r *pollingShutdownRunner) Runnable(ctx context.Context, _ int) ([]RunWork, error) {
	close(r.started)
	select {
	case <-r.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *pollingShutdownRunner) RunQueued(context.Context, RunWork, EventEmitter) error {
	return nil
}

func (r *pollingShutdownRunner) Cancel(context.Context, string) (bool, error) {
	return false, nil
}

func (r *pollingShutdownRunner) CancelQueuedRuns(context.Context) error {
	return nil
}

func TestSchedulerShutdownStopsWorkersBeforeQueuedCancellation(t *testing.T) {
	runner := &shutdownOrderRunner{started: make(chan struct{}), finished: make(chan struct{})}
	scheduler := NewScheduler(context.Background(), runner, shutdownOrderReader{}, shutdownOrderEvents{}, "app")
	if _, err := scheduler.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	scheduler.Enqueue(context.Background(), "echo")
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("调度器没有开始执行 Run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scheduler.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestSchedulerShutdownDoesNotCancelPollingQuery(t *testing.T) {
	runner := &pollingShutdownRunner{started: make(chan struct{}), release: make(chan struct{})}
	scheduler := NewScheduler(
		context.Background(), runner, shutdownOrderReader{}, shutdownOrderEvents{}, "app",
		WithScheduler(1, time.Millisecond, 1),
	)
	if _, err := scheduler.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("调度器没有开始读取持久队列")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- scheduler.Shutdown(ctx) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("轮询未结束就完成 Shutdown: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runner.release)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("轮询结束后 Shutdown 未完成")
	}
}
