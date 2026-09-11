package loader_test

import (
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/contracts"
)

// hostedTestRequest 构造携带能力标识的治理上下文（lease.Invoke 用）。
func hostedTestRequest(capabilityID string) contracts.RequestContext {
	return contracts.RequestContext{
		AppID: "app.test", EchoID: "echo-1", RequestID: "request-1", UserID: "user-1",
		Deadline: time.Now().Add(time.Minute), CapabilityID: capabilityID,
	}
}
