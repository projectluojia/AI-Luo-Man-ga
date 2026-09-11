// success 是 hosted Loader 成功路径的最小测试固件：校验并回显调用载荷。
// 不知道包名与能力名：能力标识属于宿主侧治理，guest 只消费信封里的 payload，
// 避免测试固件退化成业务 Package。只用于 internal/kernel/loader 测试。
package main

import (
	"encoding/json"
	"io"
	"os"
)

// requestEnvelope 与宿主约定的 stdin 调用信封；Capability 标识由宿主治理，固件不消费。
type requestEnvelope struct {
	Payload json.RawMessage `json:"payload"`
}

// resultEnvelope 与宿主约定的 stdout 结果信封。
type resultEnvelope struct {
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

func main() {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeEnvelope(resultEnvelope{Code: "internal"})
		return
	}
	var request requestEnvelope
	if err := json.Unmarshal(input, &request); err != nil || !json.Valid(request.Payload) {
		writeEnvelope(resultEnvelope{Code: "invalid_argument"})
		return
	}
	writeEnvelope(resultEnvelope{OK: true, Result: request.Payload})
}

func writeEnvelope(envelope resultEnvelope) {
	_ = json.NewEncoder(os.Stdout).Encode(envelope)
}
