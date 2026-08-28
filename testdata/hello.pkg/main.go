//go:build wasip1

// hello 说你好。
package main

import (
	"encoding/json"
	"io"
	"os"
)

// hello 是 hello.pkg 的唯一 capability。
func hello(args HelloArgs) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"message": "hello, " + args.Name})
}

// HelloArgs 是 hello 的输入。
type HelloArgs struct {
	Name string `json:"name"`
}

type hostedRequest struct {
	ToolID  string          `json:"tool_id"`
	Payload json.RawMessage `json:"payload"`
}

type hostedEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

func main() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeEnvelope(hostedEnvelope{Code: "internal", Message: "read stdin failed"})
		return
	}
	var request hostedRequest
	if err := json.Unmarshal(input, &request); err != nil {
		writeEnvelope(hostedEnvelope{Code: "invalid_argument", Message: "request envelope is malformed"})
		return
	}
	if request.ToolID != "hello.pkg.hello" {
		writeEnvelope(hostedEnvelope{Code: "invalid_argument", Message: "unknown tool"})
		return
	}
	var args HelloArgs
	if err := json.Unmarshal(request.Payload, &args); err != nil {
		writeEnvelope(hostedEnvelope{Code: "invalid_argument", Message: "payload is malformed"})
		return
	}
	result, err := hello(args)
	if err != nil {
		writeEnvelope(hostedEnvelope{Code: "internal", Message: "capability failed"})
		return
	}
	writeEnvelope(hostedEnvelope{OK: true, Result: result})
}

func writeEnvelope(envelope hostedEnvelope) {
	if err := json.NewEncoder(os.Stdout).Encode(envelope); err != nil {
		os.Exit(1)
	}
}
