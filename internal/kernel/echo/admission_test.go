package echo_test

import (
	"context"
	"testing"

	kernelecho "github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/echo"
)

type admissionCreator struct{}

func (*admissionCreator) CreateIdempotent(context.Context, kernelecho.RunRequest) (string, bool, error) {
	return "echo-1", true, nil
}

type admissionQueue struct {
	ids []string
}

func (q *admissionQueue) Enqueue(_ context.Context, echoID string) {
	q.ids = append(q.ids, echoID)
}

func TestAdmissionCreatesAndQueuesOnce(t *testing.T) {
	queue := &admissionQueue{}
	admission := kernelecho.NewAdmission(&admissionCreator{}, queue)
	if echoID, created, err := admission.Create(context.Background(), kernelecho.RunRequest{}); err != nil || !created || echoID != "echo-1" {
		t.Fatalf("create = %q/%t, error=%v", echoID, created, err)
	}
	if len(queue.ids) != 1 || queue.ids[0] != "echo-1" {
		t.Fatalf("queued ids = %v", queue.ids)
	}
}
