package publicerror_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/loader"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/publicerror"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/registry"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/kernel/runtime"
)

func TestCapabilityErrorDoesNotDiscloseInternalCause(t *testing.T) {
	internal := errors.New("open /srv/private.db: SQL secret-value")
	public := publicerror.Capability(internal)
	if public.Code != "capability_failed" || public.Message != "Capability 调用失败" {
		t.Fatalf("unexpected public error: %#v", public)
	}
	if strings.Contains(public.Message, "private.db") || strings.Contains(public.Message, "secret-value") {
		t.Fatalf("public error disclosed internal cause: %#v", public)
	}
}

func TestCapabilityRuntimeErrorsAreStableAndSafe(t *testing.T) {
	public := publicerror.Capability(errors.Join(loader.ErrLoadFailed, errors.New("host /srv/private.sock token-secret")))
	if public.Code != "runtime_unavailable" || public.Message != "Capability 运行时暂时不可用" || !public.Retryable {
		t.Fatalf("public=%#v", public)
	}
	if strings.Contains(public.Message, "/srv/") || strings.Contains(public.Message, "secret") {
		t.Fatalf("unsafe public runtime error=%#v", public)
	}
}

func TestCapabilityRuntimeInvocationRejectsHostMessageAndUntrustedCode(t *testing.T) {
	known := publicerror.Capability(loader.InvocationError{Code: "permission_denied", Retryable: true})
	if known.Code != "permission_denied" || known.Retryable || strings.Contains(known.Message, "host") {
		t.Fatalf("known invocation error=%#v", known)
	}
	unknown := publicerror.Capability(loader.InvocationError{Code: "private_secret", Retryable: true})
	if unknown.Code != "capability_failed" || unknown.Retryable || unknown.Message != "Capability 调用失败" {
		t.Fatalf("unknown invocation error=%#v", unknown)
	}
}

func TestCapabilityRuntimeProtocolErrorIsStableAndNotRetryable(t *testing.T) {
	public := publicerror.Capability(errors.Join(loader.ErrRuntimeProtocol, errors.New("host secret")))
	if public.Code != "runtime_protocol_error" || public.Retryable ||
		public.Message != "Capability 运行时协议响应无效" {
		t.Fatalf("public=%#v", public)
	}
}

func TestCapabilityTrustBoundaryErrorsHaveStableSafeCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		code string
	}{
		{err: errors.Join(registry.ErrSchemaValidation, errors.New(`payload contains "private-value"`)), code: "invalid_arguments"},
		{err: errors.Join(registry.ErrPermissionDenied, errors.New("permission=secret.admin")), code: "permission_denied"},
		{err: errors.Join(runtime.ErrIdempotencyKeyRequired, errors.New("target=private-write")), code: "idempotency_key_required"},
		{err: errors.Join(runtime.ErrConfirmationRequired, errors.New("confirmation=private-token")), code: "confirmation_required"},
	}
	for _, test := range tests {
		public := publicerror.Capability(test.err)
		if public.Code != test.code {
			t.Fatalf("error %v got code %q, want %q", test.err, public.Code, test.code)
		}
		if strings.Contains(public.Message, "private") || strings.Contains(public.Message, "secret") {
			t.Fatalf("public error disclosed internal detail: %#v", public)
		}
		if normalized := publicerror.NormalizeCapability(public); normalized != public {
			t.Fatalf("normalization changed trusted error: got %#v, want %#v", normalized, public)
		}
	}
}

func TestCapabilityErrorKeepsStableGovernanceCode(t *testing.T) {
	public := publicerror.Capability(errors.Join(runtime.ErrCapabilityDisabled, errors.New("app=secret-app")))
	if public.Code != "capability_disabled" || strings.Contains(public.Message, "secret-app") {
		t.Fatalf("unexpected public error: %#v", public)
	}
	unavailable := publicerror.Capability(errors.Join(runtime.ErrAppPolicyUnavailable, errors.New("SQL /srv/private.db secret")))
	if unavailable.Code != "app_policy_unavailable" || !unavailable.Retryable ||
		strings.Contains(unavailable.Message, "private") || strings.Contains(unavailable.Message, "secret") {
		t.Fatalf("unexpected App policy error: %#v", unavailable)
	}
	for _, code := range []string{"app_policy_unavailable", "app_disabled"} {
		value := publicerror.Echo(code)
		if value.Code != code || strings.Contains(value.Message, "private") || strings.Contains(value.Message, "secret") {
			t.Fatalf("unexpected Echo App error: %#v", value)
		}
	}
}

func TestCapabilityDataGovernanceErrorsAreStableAndSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err       error
		code      string
		retryable bool
	}{
		{err: loader.InvocationError{Code: "data_unavailable"}, code: "data_unavailable", retryable: true},
		{err: loader.InvocationError{Code: "data_incomplete"}, code: "data_incomplete"},
		{err: loader.InvocationError{Code: "data_non_authoritative"}, code: "data_non_authoritative"},
		{err: loader.InvocationError{Code: "data_expired"}, code: "data_expired", retryable: true},
	}
	for _, test := range tests {
		value := publicerror.Capability(errors.Join(test.err, errors.New("source=private-copy")))
		if value.Code != test.code || value.Retryable != test.retryable ||
			strings.Contains(value.Message, "private-copy") {
			t.Fatalf("data error %q normalized to %#v", test.code, value)
		}
		if normalized := publicerror.NormalizeCapability(value); normalized != value {
			t.Fatalf("normalization changed %#v to %#v", value, normalized)
		}
	}
}

func TestExecutorErrorRejectsUntrustedCodeAndMessageShape(t *testing.T) {
	public := publicerror.Executor("untrusted_body_/srv/private", true)
	if public.Code != "executor_failed" || public.Message != "执行者 Run 执行失败" || !public.Retryable {
		t.Fatalf("unexpected public error: %#v", public)
	}
}

func TestExecutorFailuresKeepStableRetrySemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code      string
		retryable bool
	}{
		{code: "execution_timeout", retryable: true},
		{code: "execution_rate_limited", retryable: true},
		{code: "execution_unavailable", retryable: true},
		{code: "execution_rejected"},
		{code: "execution_failed"},
		{code: "execution_protocol_error"},
		{code: "budget_exceeded"},
	}
	for _, test := range tests {
		executorFailure := publicerror.Executor(test.code, test.retryable)
		expectedCode := test.code
		if test.code == "execution_unavailable" {
			expectedCode = "executor_unavailable"
		}
		if executorFailure.Code != expectedCode || executorFailure.Retryable != test.retryable {
			t.Fatalf("executor error %q normalized to %#v", test.code, executorFailure)
		}
		echoFailure := publicerror.Echo(executorFailure.Code)
		if echoFailure.Code != expectedCode || echoFailure.Retryable != test.retryable {
			t.Fatalf("Echo error %q normalized to %#v", test.code, echoFailure)
		}
		if strings.Contains(echoFailure.Message, "private") || strings.Contains(echoFailure.Message, "secret") {
			t.Fatalf("provider error disclosed details: %#v", echoFailure)
		}
	}
}
