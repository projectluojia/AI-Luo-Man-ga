package agenthost_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/agenthost"
)

func TestAgentHostRejectsUnauthenticatedNonLoopbackBoundary(t *testing.T) {
	t.Parallel()

	if _, err := agenthost.Start(context.Background(), "python", "0.0.0.0:50051", io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "非回环") {
		t.Fatalf("non-loopback Start error=%v", err)
	}
	if connection, _, err := agenthost.Dial(context.Background(), "192.0.2.10:50051"); err == nil || connection != nil ||
		!strings.Contains(err.Error(), "非回环") {
		t.Fatalf("non-loopback Dial connection=%v error=%v", connection, err)
	}
}
