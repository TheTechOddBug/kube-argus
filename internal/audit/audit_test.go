package audit

import (
	"testing"
	"time"
)

// Dedup must preserve all unique entries from both slices, deduplicating
// by (time, actor, action, resource). This is what guarantees consistency when
// multiple pod replicas write to the same ConfigMap.
func TestAuditDedup_DedupesAcrossSources(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)

	local := []Entry{
		{Time: now, Actor: "alice", Action: "login", Resource: ""},
		{Time: now.Add(1 * time.Second), Actor: "bob", Action: "pod.delete", Resource: "Pod ns/p1"},
	}
	remote := []Entry{
		{Time: now, Actor: "alice", Action: "login", Resource: ""},
		{Time: now.Add(2 * time.Second), Actor: "carol", Action: "workload.restart", Resource: "Deployment ns/api"},
	}

	merged := Dedup(local, remote)

	if len(merged) != 3 {
		t.Fatalf("expected 3 deduplicated entries, got %d: %+v", len(merged), merged)
	}

	seenActors := map[string]int{}
	for _, e := range merged {
		seenActors[e.Actor]++
	}
	if seenActors["alice"] != 1 || seenActors["bob"] != 1 || seenActors["carol"] != 1 {
		t.Fatalf("expected each unique actor exactly once, got %v", seenActors)
	}
}

func TestAuditDedup_DifferentResourcesNotMerged(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)

	a := []Entry{
		{Time: now, Actor: "alice", Action: "pod.delete", Resource: "Pod ns/p1"},
	}
	b := []Entry{
		{Time: now, Actor: "alice", Action: "pod.delete", Resource: "Pod ns/p2"},
	}

	merged := Dedup(a, b)

	if len(merged) != 2 {
		t.Fatalf("expected 2 entries (different resources), got %d", len(merged))
	}
}

func TestAuditDedup_EmptyInputs(t *testing.T) {
	if got := Dedup(nil, nil); len(got) != 0 {
		t.Fatalf("expected 0 entries from nil inputs, got %d", len(got))
	}
}

func TestAuditTrimExpired_DropsOldEntries(t *testing.T) {
	savedRetention := Retention
	defer func() { Retention = savedRetention }()
	Retention = 1 * time.Hour

	now := time.Now()
	entries := []Entry{
		{Time: now.Add(-2 * time.Hour), Actor: "old1"},
		{Time: now.Add(-30 * time.Minute), Actor: "recent1"},
		{Time: now.Add(-90 * time.Minute), Actor: "old2"},
		{Time: now, Actor: "recent2"},
	}

	kept := TrimExpired(entries)

	if len(kept) != 2 {
		t.Fatalf("expected 2 retained entries, got %d", len(kept))
	}
	for _, e := range kept {
		if e.Actor == "old1" || e.Actor == "old2" {
			t.Fatalf("expired entry leaked: %s", e.Actor)
		}
	}
}

func TestAuditTrimExpired_KeepsEntriesAtBoundary(t *testing.T) {
	savedRetention := Retention
	defer func() { Retention = savedRetention }()
	Retention = 1 * time.Hour

	now := time.Now()
	entries := []Entry{{Time: now.Add(-30 * time.Minute), Actor: "fresh"}}

	kept := TrimExpired(entries)
	if len(kept) != 1 {
		t.Fatalf("expected entry within retention to be kept, got %d", len(kept))
	}
}

// TrimExpired must be a no-op when Retention is unset (zero) — otherwise
// cutoff would equal time.Now() and every prior entry would be silently
// dropped. Regression guard for the case where Record() fires before Init.
func TestAuditTrimExpired_ZeroRetentionPreservesEntries(t *testing.T) {
	savedRetention := Retention
	defer func() { Retention = savedRetention }()
	Retention = 0

	now := time.Now()
	entries := []Entry{
		{Time: now.Add(-24 * time.Hour), Actor: "day-old"},
		{Time: now.Add(-1 * time.Hour), Actor: "hour-old"},
		{Time: now, Actor: "fresh"},
	}

	kept := TrimExpired(entries)
	if len(kept) != 3 {
		t.Fatalf("with Retention=0, all entries must be preserved; got %d/%d", len(kept), len(entries))
	}
}
