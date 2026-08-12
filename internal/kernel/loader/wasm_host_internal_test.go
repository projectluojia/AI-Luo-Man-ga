package loader

import (
	"bytes"
	"strings"
	"testing"
)

func TestLimitedBufferStopsAtLimit(t *testing.T) {
	var buffer limitedBuffer
	large := bytes.Repeat([]byte("x"), hostedStdoutLimit+1024)
	written, err := buffer.Write(large)
	if !buffer.overflowed {
		t.Fatal("overflowed = false, want true when output exceeds limit")
	}
	if err == nil {
		t.Fatal("Write error = nil, want overflow error")
	}
	if written != hostedStdoutLimit {
		t.Fatalf("written = %d, want %d", written, hostedStdoutLimit)
	}
	if len(buffer.Buffer()) != hostedStdoutLimit {
		t.Fatalf("buffered = %d, want %d", len(buffer.Buffer()), hostedStdoutLimit)
	}
	// 溢出后继续写入不再增长。
	if _, err := buffer.Write([]byte("more")); err == nil {
		t.Fatal("Write after overflow error = nil, want overflow error")
	}
	if len(buffer.Buffer()) != hostedStdoutLimit {
		t.Fatalf("buffered after overflow = %d, want %d", len(buffer.Buffer()), hostedStdoutLimit)
	}
}

func TestLimitedBufferAcceptsWithinLimit(t *testing.T) {
	var buffer limitedBuffer
	payload := strings.Repeat("hello ", 100)
	written, err := buffer.Write([]byte(payload))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written != len(payload) || buffer.overflowed {
		t.Fatalf("written = %d overflowed = %v, want full write without overflow", written, buffer.overflowed)
	}
	if len(buffer.Buffer()) != len(payload) {
		t.Fatalf("buffered = %d, want %d", len(buffer.Buffer()), len(payload))
	}
}
