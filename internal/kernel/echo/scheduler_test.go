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

type retryShutdownRunner struct {
	calls atomic.Int32
}

func (r *retryShutdownRunner) Recoverable(context.Context) ([]RunWork, error)         { return nil, nil }
func (r *retryShutdownRunner) Runnable(context.Context, int) ([]RunWork, error)       { return nil, nil }
func (r *retryShutdownRunner) RunQueued(context.Context, RunWork, EventEmitter) error { return nil }
func (r *retryShutdownRunner) Cancel(context.Context, string) (bool, error)           { return false, nil }
func (r *retryShutdownRunner) CancelQueuedRuns(context.Context) error {
	if r.calls.Add(1) == 1 {
		return errors.New("first shutdown attempt failed")
	}
	return nil
}

func TestSchedulerShutdownRetriesAfterFailure(t *testing.T) {
	runner := &retryShutdownRunner{}
	scheduler := NewScheduler(context.Background(), runner, shutdownOrderReader{}, shutdownOrderEvents{}, "app")
	if err := scheduler.Shutdown(context.Background()); err == nil {
		t.Fatal("first Shutdown unexpectedly succeeded")
	}
	if err := scheduler.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if got := runner.calls.Load(); got != 2 {
		t.Fatalf("CancelQueuedRuns calls=%d, want 2", got)
	}
}

type cancelledEchoReader struct{}

func (cancelledEchoReader) GetEcho(context.Context, string, string) (Record, []Event, error) {
	return Record{Status: StatusCancelled}, nil, nil
}

type finishTrackingEvents struct{ finished atomic.Bool }

func (e *finishTrackingEvents) Publish(Event)         {}
func (e *finishTrackingEvents) Finish(string, string) { e.finished.Store(true) }

type queuedCancelRunner struct{}

func (queuedCancelRunner) Recoverable(context.Context) ([]RunWork, error)         { return nil, nil }
func (queuedCancelRunner) Runnable(context.Context, int) ([]RunWork, error)       { return nil, nil }
func (queuedCancelRunner) RunQueued(context.Context, RunWork, EventEmitter) error { return nil }
func (queuedCancelRunner) Cancel(context.Context, string) (bool, error)           { return true, nil }
func (queuedCancelRunner) CancelQueuedRuns(context.Context) error                 { return nil }

func TestSchedulerCancelFinishesQueuedEcho(t *testing.T) {
	events := &finishTrackingEvents{}
	scheduler := NewScheduler(context.Background(), queuedCancelRunner{}, cancelledEchoReader{}, events, "app")
	if cancelled, err := scheduler.Cancel(context.Background(), "echo"); err != nil || !cancelled {
		t.Fatalf("Cancel result=%t err=%v", cancelled, err)
	}
	if !events.finished.Load() {
		t.Fatal("queued cancellation did not finish Echo events")
	}
}
