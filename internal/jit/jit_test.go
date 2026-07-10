package jit

import (
	"strings"
	"testing"
	"time"
)

func resetJITStore() func() {
	saved := jitStore.requests
	jitStore.requests = nil
	return func() { jitStore.requests = saved }
}

func TestJitApprove_Success(t *testing.T) {
	defer resetJITStore()()

	req := jitRequest{
		ID:        "test-1",
		Email:     "alice@example.com",
		Namespace: "ns-a",
		Pod:       "pod-1",
		Duration:  "1h",
		Status:    "pending",
		CreatedAt: time.Now(),
	}
	jitStore.requests = []jitRequest{req}

	if err := jitApprove("test-1", "admin@example.com", "127.0.0.1"); err != nil {
		t.Fatalf("approve failed: %v", err)
	}

	got := jitStore.requests[0]
	if got.Status != "active" {
		t.Fatalf("expected status=active, got %s", got.Status)
	}
	if got.ApprovedBy != "admin@example.com" {
		t.Fatalf("expected ApprovedBy=admin@example.com, got %s", got.ApprovedBy)
	}
	if got.ExpiresAt == nil {
		t.Fatal("expected ExpiresAt to be set after approve")
	}
}

func TestJitApprove_RejectsNonPending(t *testing.T) {
	defer resetJITStore()()

	jitStore.requests = []jitRequest{
		{ID: "denied-1", Email: "alice@example.com", Duration: "1h", Status: "denied", CreatedAt: time.Now()},
	}

	err := jitApprove("denied-1", "admin@example.com", "127.0.0.1")
	if err == nil {
		t.Fatal("expected error approving a non-pending request")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Fatalf("expected error to mention 'pending', got: %v", err)
	}
}

func TestJitApprove_NotFound(t *testing.T) {
	defer resetJITStore()()

	err := jitApprove("does-not-exist", "admin@example.com", "127.0.0.1")
	if err != errJITNotFound {
		t.Fatalf("expected errJITNotFound, got: %v", err)
	}
}

func TestJitDeny_Success(t *testing.T) {
	defer resetJITStore()()

	jitStore.requests = []jitRequest{
		{ID: "to-deny", Email: "alice@example.com", Duration: "1h", Status: "pending", CreatedAt: time.Now()},
	}

	if err := jitDeny("to-deny", "admin@example.com", "127.0.0.1"); err != nil {
		t.Fatalf("deny failed: %v", err)
	}
	if jitStore.requests[0].Status != "denied" {
		t.Fatalf("expected status=denied, got %s", jitStore.requests[0].Status)
	}
}

// hasActiveJIT must respect both ownership (email match) and namespace scope.
// A viewer with an active grant for ns-a/Deployment/api MUST NOT be able to
// reuse it for a different workload.
func TestHasActiveJIT_ScopedToWorkload(t *testing.T) {
	defer resetJITStore()()

	expiresIn := time.Now().Add(30 * time.Minute)
	jitStore.requests = []jitRequest{{
		ID: "g1", Email: "alice@example.com",
		Namespace: "payments", OwnerKind: "Deployment", OwnerName: "api",
		Status: "active", ExpiresAt: &expiresIn,
	}}

	// Exact match → allowed
	if !hasActiveJIT("alice@example.com", "payments", "Deployment", "api") {
		t.Fatal("expected matching grant to be active")
	}
	// Different workload in same namespace → must NOT match
	if hasActiveJIT("alice@example.com", "payments", "Deployment", "worker") {
		t.Fatal("grant must not leak to a different workload")
	}
	// Different user → must NOT match
	if hasActiveJIT("bob@example.com", "payments", "Deployment", "api") {
		t.Fatal("grant must not leak to a different user")
	}
}

func TestHasActiveJIT_RespectsExpiration(t *testing.T) {
	defer resetJITStore()()

	expiredAt := time.Now().Add(-1 * time.Minute)
	jitStore.requests = []jitRequest{{
		ID: "g1", Email: "alice@example.com",
		Namespace: "payments", OwnerKind: "Deployment", OwnerName: "api",
		Status: "active", ExpiresAt: &expiredAt,
	}}

	if hasActiveJIT("alice@example.com", "payments", "Deployment", "api") {
		t.Fatal("expired grant must not be considered active")
	}
}
