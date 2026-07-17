package snapshot

// registry_capture_wiring_test.go — end-to-end regression gate for the #1880
// self-heal RETRY-LOOP WIRING inside [WriteLabels] / [WriteProperties] (task
// #1890). The sibling registry_capture_selfheal_test.go pins the collector +
// snapshotRegistry building blocks in isolation; these tests drive the whole
// writer body (header → bounded retry loop → name-table emission → record
// emission → flush) and pin the four behaviours the block-level tests leave
// unconstrained: (1) the loop breaks only when BOTH collectors agree,
// (2) it retries on a collector error, (3) it returns the error and writes
// NOTHING when the bounded budget is exhausted, and (4) WriteProperties uses a
// FRESH value arena per attempt.
//
// The test seam is the per-call function parameter that the exported writers
// delegate to: writeLabels(w, g, snapReg) and writeProperties(w, g, snapKeys,
// newArena). Production passes the real snapshotRegistry / snapshotPropertyKeys
// / newPropValueArena; these tests pass locally-scoped closures whose only state
// is a stack-local counter. There is NO package-level mutable variable, so the
// seam honours the module's "no hidden global state" mandate and the writers
// stay exactly as re-entrant as before. The closures reproduce the concurrent-
// commit race deterministically: on the chosen attempts they intern a brand-new,
// entity-attached name AFTER taking the (now stale) snapshot, so the collector's
// live-graph walk observes a name absent from the capture — precisely the
// condition the retry loop self-heals.

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// WriteLabels — retry-loop wiring
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteLabels_SelfHealRetriesAndSucceeds forces the label collector to fail
// on the first forcedRetries attempts (a fresh node label interned after each
// snapshot) and asserts the writer re-captures until it converges, taking exactly
// one snapshot per attempt and round-tripping every label — including the ones
// interned mid-race.
//
// Kills the "break unconditionally on the first iteration" mutant (it would emit
// the failed attempt's nil records — ReadLabels would show zero node labels) and
// the "return on first collector error" mutant (it would surface the error).
func TestWriteLabels_SelfHealRetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	const forcedRetries = 3
	remaining := forcedRetries
	calls := 0
	snap := func(reg *lpg.LabelRegistry) []string {
		calls++
		// Capture BEFORE the race mutation, so the returned table is stale.
		out := snapshotRegistry(reg)
		if remaining > 0 {
			remaining--
			node := fmt.Sprintf("racer-%d", remaining)
			label := fmt.Sprintf("Fresh-%d", remaining)
			if err := g.SetNodeLabel(node, label); err != nil {
				t.Fatalf("SetNodeLabel(race): %v", err)
			}
		}
		return out
	}

	var buf bytes.Buffer
	size, crc, err := writeLabels(&buf, g, snap)
	if err != nil {
		t.Fatalf("writeLabels must self-heal and succeed, got %v", err)
	}
	if size <= 0 || crc == 0 {
		t.Fatalf("size=%d crc=%d, want a non-empty written payload", size, crc)
	}
	if calls != forcedRetries+1 {
		t.Fatalf("snapshot taken %d times, want %d (one per attempt: %d retries + the converging attempt) — the loop did not retry",
			calls, forcedRetries+1, forcedRetries)
	}

	rb, err := ReadLabels(&buf)
	if err != nil {
		t.Fatalf("ReadLabels: %v", err)
	}
	got := nodeLabelNameSet(t, rb)
	want := []string{"Person", "Fresh-0", "Fresh-1", "Fresh-2"}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("node label %q missing from readback %v — the writer emitted a stale/partial capture", w, got)
		}
	}
	if len(rb.NodeLabels) != len(want) {
		t.Fatalf("NodeLabels = %d, want %d", len(rb.NodeLabels), len(want))
	}
}

// TestWriteLabels_BreakRequiresBothCollectors forces a race that fails ONLY the
// edge collector: a fresh label is attached to an existing edge after the
// snapshot, while no new node label appears. A loop that broke on node-collector
// success alone would emit an edge table missing the raced label; the real loop
// breaks only when both collectors agree, so it retries once and captures the
// edge label too.
//
// Kills the "break as soon as the node collector succeeds" mutant.
func TestWriteLabels_BreakRequiresBothCollectors(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("a", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	g.SetEdgeLabel("a", "b", "KNOWS")

	fired := false
	calls := 0
	snap := func(reg *lpg.LabelRegistry) []string {
		calls++
		out := snapshotRegistry(reg)
		if !fired {
			fired = true
			// Fresh label on the EXISTING edge: visible to the edge walk,
			// absent from the just-captured snapshot; node labels unchanged.
			g.SetEdgeLabel("a", "b", "FRESH_EDGE")
		}
		return out
	}

	var buf bytes.Buffer
	if _, _, err := writeLabels(&buf, g, snap); err != nil {
		t.Fatalf("writeLabels must self-heal on an edge-only race, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("snapshot taken %d times, want 2 (the edge-only race forces one retry) — the loop broke before the edge collector agreed", calls)
	}

	rb, err := ReadLabels(&buf)
	if err != nil {
		t.Fatalf("ReadLabels: %v", err)
	}
	edges := edgeLabelNameSet(t, rb)
	for _, w := range []string{"KNOWS", "FRESH_EDGE"} {
		if !edges[w] {
			t.Fatalf("edge label %q missing from readback %v — the loop broke before the edge collector succeeded", w, edges)
		}
	}
}

// TestWriteLabels_ExhaustionReturnsErrorWritesNothing interns a fresh, node-
// attached label on EVERY attempt so no snapshot ever converges. It asserts the
// writer exhausts the bounded budget, returns the collector error, reports 0/0,
// and leaves the underlying writer untouched (the buffered magic header is never
// flushed on the error path).
//
// Kills the "unbounded retry" mutant (it would spin forever and time out) and
// any mutant that flushes partial output before returning the exhaustion error.
func TestWriteLabels_ExhaustionReturnsErrorWritesNothing(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}

	calls := 0
	snap := func(reg *lpg.LabelRegistry) []string {
		out := snapshotRegistry(reg)
		node := fmt.Sprintf("racer-%d", calls)
		label := fmt.Sprintf("Fresh-%d", calls)
		calls++
		if err := g.SetNodeLabel(node, label); err != nil {
			t.Fatalf("SetNodeLabel(race): %v", err)
		}
		return out
	}

	var buf bytes.Buffer
	size, crc, err := writeLabels(&buf, g, snap)
	if err == nil {
		t.Fatal("writeLabels must return the collector error when the retry budget is exhausted")
	}
	if size != 0 || crc != 0 {
		t.Fatalf("on exhaustion size=%d crc=%d, want 0/0", size, crc)
	}
	if buf.Len() != 0 {
		t.Fatalf("on exhaustion %d bytes reached the writer, want 0 (nothing must be written)", buf.Len())
	}
	if calls != maxRegistryCaptureRetries+1 {
		t.Fatalf("snapshot taken %d times, want %d (initial attempt + maxRegistryCaptureRetries=%d retries)",
			calls, maxRegistryCaptureRetries+1, maxRegistryCaptureRetries)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WriteProperties — retry-loop wiring + fresh-arena-per-attempt
// ─────────────────────────────────────────────────────────────────────────────

// TestWriteProperties_SelfHealRetriesAndSucceeds is the property-key counterpart
// of TestWriteLabels_SelfHealRetriesAndSucceeds, and additionally pins the
// fresh-arena-per-attempt invariant: the injected arena factory records every
// arena it hands out, and the test asserts one distinct arena per attempt.
//
// Kills the "break on first iteration" / "no retry" mutants (as for labels) and
// the "reuse one arena across attempts" mutant — hoisting the arena allocation
// out of the loop makes the factory fire once instead of once per attempt.
func TestWriteProperties_SelfHealRetriesAndSucceeds(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("a")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	const forcedRetries = 3
	remaining := forcedRetries
	snapCalls := 0
	snap := func(reg *lpg.PropertyKeyRegistry) []string {
		snapCalls++
		out := snapshotPropertyKeys(reg)
		if remaining > 0 {
			remaining--
			node := fmt.Sprintf("racer-%d", remaining)
			key := fmt.Sprintf("fresh_%d", remaining)
			if err := g.SetNodeProperty(node, key, lpg.StringValue("v")); err != nil {
				t.Fatalf("SetNodeProperty(race): %v", err)
			}
		}
		return out
	}
	var arenas []*propValueArena
	newArena := func() *propValueArena {
		a := &propValueArena{}
		arenas = append(arenas, a)
		return a
	}

	var buf bytes.Buffer
	if _, _, err := writeProperties(&buf, g, snap, newArena); err != nil {
		t.Fatalf("writeProperties must self-heal and succeed, got %v", err)
	}
	if snapCalls != forcedRetries+1 {
		t.Fatalf("key snapshot taken %d times, want %d — the loop did not retry", snapCalls, forcedRetries+1)
	}
	if len(arenas) != forcedRetries+1 {
		t.Fatalf("value arena allocated %d times, want %d (one FRESH arena per attempt) — an attempt reused a prior arena",
			len(arenas), forcedRetries+1)
	}
	seen := make(map[*propValueArena]struct{}, len(arenas))
	for _, a := range arenas {
		if _, dup := seen[a]; dup {
			t.Fatal("value arena reused across attempts — each attempt must receive a distinct fresh arena")
		}
		seen[a] = struct{}{}
	}

	rb, err := ReadProperties(&buf)
	if err != nil {
		t.Fatalf("ReadProperties: %v", err)
	}
	got := nodePropertyKeyNameSet(t, rb)
	for _, w := range []string{"name", "fresh_0", "fresh_1", "fresh_2"} {
		if !got[w] {
			t.Fatalf("node property key %q missing from readback %v — the writer emitted a stale/partial capture", w, got)
		}
	}
}

// TestWriteProperties_BreakRequiresBothCollectors forces a race that fails ONLY
// the edge-property collector (a fresh key on an existing edge), the property-key
// dual of TestWriteLabels_BreakRequiresBothCollectors. It uses the production
// arena factory to confirm the real wiring works through the seam.
func TestWriteProperties_BreakRequiresBothCollectors(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeProperty("a", "name", lpg.StringValue("x")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "since", lpg.StringValue("2020")); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}

	fired := false
	snapCalls := 0
	snap := func(reg *lpg.PropertyKeyRegistry) []string {
		snapCalls++
		out := snapshotPropertyKeys(reg)
		if !fired {
			fired = true
			if err := g.SetEdgeProperty("a", "b", "fresh_edge_key", lpg.StringValue("v")); err != nil {
				t.Fatalf("SetEdgeProperty(race): %v", err)
			}
		}
		return out
	}

	var buf bytes.Buffer
	if _, _, err := writeProperties(&buf, g, snap, newPropValueArena); err != nil {
		t.Fatalf("writeProperties must self-heal on an edge-only race, got %v", err)
	}
	if snapCalls != 2 {
		t.Fatalf("key snapshot taken %d times, want 2 (the edge-only race forces one retry) — the loop broke before the edge collector agreed", snapCalls)
	}

	rb, err := ReadProperties(&buf)
	if err != nil {
		t.Fatalf("ReadProperties: %v", err)
	}
	edges := edgePropertyKeyNameSet(t, rb)
	for _, w := range []string{"since", "fresh_edge_key"} {
		if !edges[w] {
			t.Fatalf("edge property key %q missing from readback %v — the loop broke before the edge collector succeeded", w, edges)
		}
	}
}

// TestWriteProperties_ExhaustionReturnsErrorWritesNothing is the property-key
// counterpart of TestWriteLabels_ExhaustionReturnsErrorWritesNothing. It also
// asserts a fresh arena is allocated on every attempt of the failing path, not
// only the succeeding one.
func TestWriteProperties_ExhaustionReturnsErrorWritesNothing(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.SetNodeProperty("alice", "name", lpg.StringValue("a")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}

	snapCalls := 0
	snap := func(reg *lpg.PropertyKeyRegistry) []string {
		out := snapshotPropertyKeys(reg)
		node := fmt.Sprintf("racer-%d", snapCalls)
		key := fmt.Sprintf("fresh_%d", snapCalls)
		snapCalls++
		if err := g.SetNodeProperty(node, key, lpg.StringValue("v")); err != nil {
			t.Fatalf("SetNodeProperty(race): %v", err)
		}
		return out
	}
	arenaCalls := 0
	newArena := func() *propValueArena {
		arenaCalls++
		return &propValueArena{}
	}

	var buf bytes.Buffer
	size, crc, err := writeProperties(&buf, g, snap, newArena)
	if err == nil {
		t.Fatal("writeProperties must return the collector error when the retry budget is exhausted")
	}
	if size != 0 || crc != 0 {
		t.Fatalf("on exhaustion size=%d crc=%d, want 0/0", size, crc)
	}
	if buf.Len() != 0 {
		t.Fatalf("on exhaustion %d bytes reached the writer, want 0 (nothing must be written)", buf.Len())
	}
	if snapCalls != maxRegistryCaptureRetries+1 {
		t.Fatalf("key snapshot taken %d times, want %d (initial attempt + maxRegistryCaptureRetries=%d retries)",
			snapCalls, maxRegistryCaptureRetries+1, maxRegistryCaptureRetries)
	}
	if arenaCalls != maxRegistryCaptureRetries+1 {
		t.Fatalf("value arena allocated %d times, want %d (a fresh arena per attempt, even on the failing path)",
			arenaCalls, maxRegistryCaptureRetries+1)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Readback assertion helpers
// ─────────────────────────────────────────────────────────────────────────────

// nodeLabelNameSet resolves each node-label record's StringIdx against the
// embedded string table and returns the set of distinct node-label names.
func nodeLabelNameSet(t *testing.T, rb LabelsReadback) map[string]bool {
	t.Helper()
	m := make(map[string]bool, len(rb.NodeLabels))
	for _, e := range rb.NodeLabels {
		if int(e.StringIdx) >= len(rb.Strings) {
			t.Fatalf("node label StringIdx %d out of range (table has %d entries)", e.StringIdx, len(rb.Strings))
		}
		m[rb.Strings[e.StringIdx]] = true
	}
	return m
}

// edgeLabelNameSet is the edge-label counterpart of nodeLabelNameSet.
func edgeLabelNameSet(t *testing.T, rb LabelsReadback) map[string]bool {
	t.Helper()
	m := make(map[string]bool, len(rb.EdgeLabels))
	for _, e := range rb.EdgeLabels {
		if int(e.StringIdx) >= len(rb.Strings) {
			t.Fatalf("edge label StringIdx %d out of range (table has %d entries)", e.StringIdx, len(rb.Strings))
		}
		m[rb.Strings[e.StringIdx]] = true
	}
	return m
}

// nodePropertyKeyNameSet resolves each node-property record's KeyIdx against the
// embedded key table and returns the set of distinct node-property key names.
func nodePropertyKeyNameSet(t *testing.T, rb PropertiesReadback) map[string]bool {
	t.Helper()
	m := make(map[string]bool, len(rb.NodeProperties))
	for _, e := range rb.NodeProperties {
		if int(e.KeyIdx) >= len(rb.Keys) {
			t.Fatalf("node property KeyIdx %d out of range (table has %d entries)", e.KeyIdx, len(rb.Keys))
		}
		m[rb.Keys[e.KeyIdx]] = true
	}
	return m
}

// edgePropertyKeyNameSet is the edge-property counterpart of nodePropertyKeyNameSet.
func edgePropertyKeyNameSet(t *testing.T, rb PropertiesReadback) map[string]bool {
	t.Helper()
	m := make(map[string]bool, len(rb.EdgeProperties))
	for _, e := range rb.EdgeProperties {
		if int(e.KeyIdx) >= len(rb.Keys) {
			t.Fatalf("edge property KeyIdx %d out of range (table has %d entries)", e.KeyIdx, len(rb.Keys))
		}
		m[rb.Keys[e.KeyIdx]] = true
	}
	return m
}
