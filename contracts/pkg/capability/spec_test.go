package capability

import (
	"testing"
	"time"
)

func testGrant() Grant {
	return Grant{
		ID: "grant-1", AppID: "campus-services", Principal: "user-alice",
		CapabilityID: "library.book.borrow",
		Resource:     ResourceScope{Type: "library.book", IDs: []string{"book-1", "book-2"}},
		ExpiresAt:    time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), MaxCalls: 2,
		MaxCostMicrousd: 100, Audience: "run-1", Delegable: true, MaxDelegationDepth: 1,
		PolicyRevision: "revision-1",
	}
}

func TestNarrowGrantRejectsResourceWidening(t *testing.T) {
	parent, err := NormalizeGrant(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	requested := parent
	requested.ID = "grant-2"
	requested.Resource.IDs = []string{"book-1", "book-3"}
	if _, err := NarrowGrant(parent, requested); err == nil {
		t.Fatal("resource widening unexpectedly succeeded")
	}
}

func TestNarrowGrantAcceptsStrictlySmallerGrant(t *testing.T) {
	parent, err := NormalizeGrant(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	requested := parent
	requested.ID = "grant-2"
	requested.Resource.IDs = []string{"book-2"}
	requested.MaxCalls = 1
	requested.MaxCostMicrousd = 50
	requested.ExpiresAt = requested.ExpiresAt.Add(-time.Hour)
	requested.MaxDelegationDepth = 0
	child, err := NarrowGrant(parent, requested)
	if err != nil {
		t.Fatal(err)
	}
	if !GrantSubset(child, parent) {
		t.Fatal("narrowed grant is not a subset")
	}
}

func TestNarrowGrantRejectsEmptyResourceScopeAgainstBoundedParent(t *testing.T) {
	parent, err := NormalizeGrant(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	requested := parent
	requested.ID = "grant-2"
	requested.Resource.IDs = nil
	if _, err := NarrowGrant(parent, requested); err == nil {
		t.Fatal("unbounded resource request unexpectedly succeeded")
	}
}

// TestGrantSubsetAllowsAudienceUnderUnboundParent 验证 App 级 Grant（Audience
// 为空）可以收窄到绑定具体 Run 的 child Grant；绑定后仍要求 Audience 一致。
func TestGrantSubsetAllowsAudienceUnderUnboundParent(t *testing.T) {
	unbound := testGrant()
	unbound.Audience = ""
	parent, err := NormalizeGrant(unbound)
	if err != nil {
		t.Fatal(err)
	}
	child := parent
	child.ID = "grant-2"
	if !GrantSubset(child, parent) {
		t.Fatal("unbound parent rejected same-audience child")
	}
	child.Audience = "run-9"
	if !GrantSubset(child, parent) {
		t.Fatal("unbound parent rejected bound child audience")
	}
	bound := parent
	bound.Audience = "run-1"
	if _, err := NormalizeGrant(bound); err != nil {
		t.Fatal(err)
	}
	mismatch := child
	mismatch.Audience = "run-9"
	if GrantSubset(mismatch, bound) {
		t.Fatal("bound parent accepted mismatched audience")
	}
}
