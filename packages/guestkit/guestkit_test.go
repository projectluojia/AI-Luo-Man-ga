package guestkit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunDispatchesByCapabilityAndWritesEnvelope(t *testing.T) {
	calls := make([]string, 0, 2)
	handlers := map[string]func(json.RawMessage) (any, error){
		"demo.echo": func(payload json.RawMessage) (any, error) {
			calls = append(calls, "demo.echo")
			if string(payload) != `{"x":1}` {
				t.Fatalf("payload=%s", payload)
			}
			return payload, nil
		},
	}
	var output strings.Builder
	Run(strings.NewReader(`{"capability_id":"demo.echo","payload":{"x":1}}`), &output, handlers)
	if len(calls) != 1 || calls[0] != "demo.echo" {
		t.Fatalf("calls=%v", calls)
	}
	var decoded ResultEnvelope
	if err := json.Unmarshal([]byte(output.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.OK || string(decoded.Result) != `{"x":1}` {
		t.Fatalf("envelope=%+v", decoded)
	}
}

func TestRunRejectsMalformedEnvelope(t *testing.T) {
	var output strings.Builder
	Run(strings.NewReader(`not-json`), &output, map[string]func(json.RawMessage) (any, error){
		"demo.echo": func(json.RawMessage) (any, error) {
			t.Fatal("dispatch must not run for malformed envelope")
			return nil, nil
		},
	})
	var decoded ResultEnvelope
	if err := json.Unmarshal([]byte(output.String()), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OK || decoded.Code != CodeInvalidArgument {
		t.Fatalf("envelope=%+v", decoded)
	}
}

func TestDecodePayloadIsStrict(t *testing.T) {
	var input struct {
		Name string `json:"name"`
	}
	if err := DecodePayload([]byte(`{"name":"a"}`), &input); err != nil || input.Name != "a" {
		t.Fatalf("decode=%+v err=%v", input, err)
	}
	if err := DecodePayload(nil, &input); err != nil {
		t.Fatalf("empty payload err=%v", err)
	}
	if err := DecodePayload([]byte(`{"name":"a","extra":1}`), &input); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := DecodePayload([]byte(`{"name":"a"} trailing`), &input); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestGovernValidatesSnapshotMeta(t *testing.T) {
	now := time.Now()
	importedAt := now.Add(-time.Hour)
	base := SnapshotMeta{
		SourceRevision: "rev-1", Source: "test", Authoritative: true,
		Complete: true, ImportedAt: importedAt, ValidUntil: now.Add(time.Hour),
	}
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = time.Now })

	if _, err := Govern(base); err != nil {
		t.Fatalf("valid meta err=%v", err)
	}
	missingRevision := base
	missingRevision.SourceRevision = ""
	if _, err := Govern(missingRevision); err == nil || err.Error() != CodeDataIncomplete {
		t.Fatalf("missing revision err=%v", err)
	}
	untrusted := base
	untrusted.Authoritative = false
	if _, err := Govern(untrusted); err == nil || err.Error() != CodeDataUntrusted {
		t.Fatalf("untrusted err=%v", err)
	}
	expired := base
	expired.ValidUntil = now.Add(-time.Minute)
	if _, err := Govern(expired); err == nil || err.Error() != CodeDataExpired {
		t.Fatalf("expired err=%v", err)
	}
	zeroValidity := base
	zeroValidity.ValidUntil = time.Time{}
	if _, err := Govern(zeroValidity); err == nil || err.Error() != CodeDataIncomplete {
		t.Fatalf("zero validity err=%v", err)
	}
}
