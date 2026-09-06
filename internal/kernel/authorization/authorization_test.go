package authorization

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectluojia/AI-Luo-Man-ga/contracts/pkg/capability"
)

func authorizationSpec(pointer string) capability.CapabilitySpec {
	return capability.CapabilitySpec{
		ID: "library.book.get", Version: "1.0.0", Name: "查看图书",
		InputSchemaJSON: `{"type":"object"}`,
		Authorization: capability.AuthorizationSpec{
			ResourceType: "library.book", ResourceIDFrom: pointer,
		},
		Execution: capability.ExecutionSpec{
			EffectTarget: capability.EffectNone, Replay: capability.ReplaySafe,
			ConfirmationFloor: capability.ConfirmationPolicy,
		},
	}
}

func authorizationGrant() capability.Grant {
	return capability.Grant{
		ID: "grant-1", AppID: "campus-services", Principal: "user-alice",
		CapabilityID: "library.book.get",
		Resource:     capability.ResourceScope{Type: "library.book", IDs: []string{"book-1"}},
		ExpiresAt:    time.Now().Add(time.Hour), MaxCalls: 2, Audience: "run-1",
		PolicyRevision: "revision-1",
	}
}

func TestAuthorizeBindsResourceFromPayload(t *testing.T) {
	decision, err := Authorize(context.Background(), authorizationSpec("/book_id"), Request{
		AppID: "campus-services", Principal: "user-alice", RunID: "run-1",
		CapabilityID: "library.book.get", Payload: []byte(`{"book_id":"book-1"}`),
		Now: time.Now(),
	}, []capability.Grant{authorizationGrant()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Grant.ID != "grant-1" || decision.RequireIdempotency || decision.RequireConfirmation {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestAuthorizeRejectsResourceOutsideGrant(t *testing.T) {
	_, err := Authorize(context.Background(), authorizationSpec("/book_id"), Request{
		AppID: "campus-services", Principal: "user-alice", RunID: "run-1",
		CapabilityID: "library.book.get", Payload: []byte(`{"book_id":"book-2"}`),
	}, []capability.Grant{authorizationGrant()}, nil)
	if err == nil {
		t.Fatal("resource outside grant unexpectedly authorized")
	}
}

func TestAuthorizeReturnsExecutionObligations(t *testing.T) {
	spec := authorizationSpec("/book_id")
	spec.Execution = capability.ExecutionSpec{
		EffectTarget: capability.EffectExternal, Replay: capability.ReplayIdempotencyKey,
		ConfirmationFloor: capability.ConfirmationRequired,
	}
	decision, err := Authorize(context.Background(), spec, Request{
		AppID: "campus-services", Principal: "user-alice", RunID: "run-1",
		CapabilityID: spec.ID, Payload: []byte(`{"book_id":"book-1"}`),
	}, []capability.Grant{authorizationGrant()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.RequireIdempotency || !decision.RequireConfirmation {
		t.Fatalf("obligations not returned: %+v", decision)
	}
}

func TestAuthorizeRejectsAnonymousCurrentUserCapability(t *testing.T) {
	spec := authorizationSpec("/book_id")
	spec.Authorization.Principal = capability.PrincipalCurrentUser
	_, err := Authorize(context.Background(), spec, Request{
		AppID: "campus-services", Principal: "public", RunID: "run-1",
		CapabilityID: spec.ID, Payload: []byte(`{"book_id":"book-1"}`),
	}, []capability.Grant{authorizationGrant()}, nil)
	if !errors.Is(err, ErrPrincipalRequired) {
		t.Fatalf("anonymous authorization error=%v, want ErrPrincipalRequired", err)
	}
}

func TestGrantSubsetAllowsNarrowingWildcardResources(t *testing.T) {
	parent := authorizationGrant()
	parent.Resource.IDs = nil
	child := parent
	child.ID = "grant-child"
	child.Audience = parent.Audience
	child.Resource.IDs = []string{"book-1"}
	if !capability.GrantSubset(child, parent) {
		t.Fatal("specific child resource is not a subset of wildcard parent")
	}
}

func TestGrantSubsetRejectsChangingAudience(t *testing.T) {
	parent := authorizationGrant()
	child := parent
	child.ID = "grant-child"
	child.Audience = "another-run"
	if capability.GrantSubset(child, parent) {
		t.Fatal("child changed a parent-bound audience")
	}
}
