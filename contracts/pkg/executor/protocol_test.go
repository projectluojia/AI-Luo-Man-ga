package executor_test

import (
	"errors"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executor"
	executorv1 "github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/executorv1"
)

func TestProtocolValidationIsOwnedByContracts(t *testing.T) {
	if err := executor.ValidateStartFrame(nil); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("nil start frame error=%v", err)
	}
	if err := executor.ValidateHealthRequest(&executorv1.HealthRequest{
		AcceptedProtocolVersions: []string{executor.Version},
	}); err != nil {
		t.Fatalf("valid health request: %v", err)
	}
	if err := executor.ValidateHealthResponse(&executorv1.HealthResponse{
		Ready: true, SupportedProtocolVersions: []string{executor.Version},
	}); err != nil {
		t.Fatalf("valid health response: %v", err)
	}
	invalid := &executorv1.HealthResponse{StatusCode: "unsafe status"}
	if err := executor.ValidateHealthResponse(invalid); !errors.Is(err, executor.ErrInvalidFrame) {
		t.Fatalf("invalid health response error=%v", err)
	}
}
