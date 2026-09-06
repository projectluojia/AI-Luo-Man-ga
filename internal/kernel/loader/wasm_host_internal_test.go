package loader

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestParseHostedEnvelopeReturnsGenericInvocationErrors(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{code: "data_unavailable", want: "data_unavailable"},
		{code: "data_incomplete", want: "data_incomplete"},
		{code: "data_untrusted", want: "data_non_authoritative"},
		{code: "data_expired", want: "data_expired"},
		{code: "invalid_argument", want: "invalid_arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"ok": false, "code": tc.code, "message": "package detail",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseHostedEnvelope("test.runtime", payload)
			if !errors.Is(err, ErrHostedCallRejected) {
				t.Fatalf("error = %v, want ErrHostedCallRejected", err)
			}
			var invocation InvocationError
			if !errors.As(err, &invocation) || invocation.Code != tc.want {
				t.Fatalf("invocation error = %#v, want code %q", invocation, tc.want)
			}
		})
	}
	unknown, err := json.Marshal(map[string]any{
		"ok": false, "code": "data_non_authoritative", "message": "package detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseHostedEnvelope("test.runtime", unknown); !errors.Is(err, ErrRuntimeProtocol) {
		t.Fatalf("unknown hosted error code = %v, want ErrRuntimeProtocol", err)
	}
}

func TestParseHostedEnvelopeRejectsNonCanonicalOrIncompleteResults(t *testing.T) {
	for _, payload := range []string{
		`{"ok":true,"result":{},"extra":true}`,
		`{"OK":true,"Result":{}}`,
		`{"ok":true,"result":{},"result":{}}`,
		`{"ok":true,"result":{}} trailing`,
		`{"ok":true}`,
		`{"code":"internal"}`,
		`{"ok":null,"code":"internal"}`,
		`{"ok":true,"result":{},"code":""}`,
		`{"ok":true,"result":{},"message":""}`,
		`{"ok":true,"result":{},"code":"internal"}`,
		`{"ok":false,"result":{},"code":"internal"}`,
		`{"ok":false,"result":null,"code":"internal"}`,
		`{"ok":false,"code":""}`,
		`{"ok":false,"code":null}`,
		`{"ok":false,"code":"internal","message":null}`,
		`{"ok":false}`,
	} {
		if _, err := parseHostedEnvelope("test.runtime", []byte(payload)); !errors.Is(err, ErrRuntimeProtocol) {
			t.Errorf("payload %q error=%v, want ErrRuntimeProtocol", payload, err)
		}
	}
}
