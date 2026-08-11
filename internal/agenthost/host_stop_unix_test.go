//go:build unix

package agenthost_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/agenthost"
)

func TestManagedAgentHostStopsWithinDeadline(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap 'exit 0' INT TERM\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := agenthost.Start(context.Background(), script, "127.0.0.1:50051", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stopContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := host.Stop(stopContext); err != nil {
		t.Fatalf("stop managed Agent: %v", err)
	}
	if err := host.Stop(stopContext); err != nil {
		t.Fatalf("repeat stop managed Agent: %v", err)
	}
	select {
	case <-host.Done():
	case <-time.After(time.Second):
		t.Fatal("managed Agent did not publish completion")
	}
}
