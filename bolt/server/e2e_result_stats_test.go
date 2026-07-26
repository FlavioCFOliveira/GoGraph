package server_test

// e2e_result_stats_test.go — end-to-end gate for the Bolt result statistics (rmp #2190).
//
// The audit's finding was that after a successful CREATE the official driver reported
// NodesCreated=0 and ContainsUpdates=false, because the server never sent the `stats`
// metadata field. These tests assert the numbers the DRIVER reports, through its own
// ResultSummary.Counters(), which is the only assertion that proves the wire contract
// end to end: the key names, the integer-versus-boolean value types, and the placement on
// the terminal SUCCESS all have to be right for the driver to surface them.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// summaryFor runs a write through the official driver and returns its ResultSummary.
func summaryFor(ctx context.Context, t *testing.T, session neo4j.SessionWithContext, query string) neo4j.ResultSummary {
	t.Helper()
	res, err := session.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	summary, err := res.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume(%q): %v", query, err)
	}
	return summary
}

// TestE2E_ResultStats_CreateCounters is the audit's headline case: a successful CREATE
// must report its effects, not zeros.
func TestE2E_ResultStats_CreateCounters(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	summary := summaryFor(ctx, t, session, `CREATE (:Person {name: 'x', age: 42})`)
	c := summary.Counters()

	if !c.ContainsUpdates() {
		t.Error("ContainsUpdates() = false after a successful CREATE")
	}
	if got := c.NodesCreated(); got != 1 {
		t.Errorf("NodesCreated() = %d, want 1", got)
	}
	if got := c.LabelsAdded(); got != 1 {
		t.Errorf("LabelsAdded() = %d, want 1", got)
	}
	if got := c.PropertiesSet(); got != 2 {
		t.Errorf("PropertiesSet() = %d, want 2", got)
	}
	// Nothing was deleted, and an unsent counter must read as zero, not as garbage.
	if got := c.NodesDeleted(); got != 0 {
		t.Errorf("NodesDeleted() = %d, want 0", got)
	}
}

// TestE2E_ResultStats_RelationshipAndDelete covers the relationship and deletion
// counters, so every counter family is exercised over the wire and not just the node one.
func TestE2E_ResultStats_RelationshipAndDelete(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	created := summaryFor(ctx, t, session, `CREATE (:A)-[:KNOWS {since: 2020}]->(:B)`).Counters()
	if got := created.NodesCreated(); got != 2 {
		t.Errorf("NodesCreated() = %d, want 2", got)
	}
	if got := created.RelationshipsCreated(); got != 1 {
		t.Errorf("RelationshipsCreated() = %d, want 1", got)
	}
	if got := created.PropertiesSet(); got != 1 {
		t.Errorf("PropertiesSet() = %d, want 1", got)
	}

	deleted := summaryFor(ctx, t, session, `MATCH (a:A)-[r:KNOWS]->(b:B) DELETE r`).Counters()
	if got := deleted.RelationshipsDeleted(); got != 1 {
		t.Errorf("RelationshipsDeleted() = %d, want 1", got)
	}
	if !deleted.ContainsUpdates() {
		t.Error("ContainsUpdates() = false after a successful DELETE")
	}

	// DETACH DELETE reports -nodes; openCypher declares no -labels/-properties for it,
	// and the server must not invent them from its internal teardown.
	detached := summaryFor(ctx, t, session, `MATCH (a:A) DETACH DELETE a`).Counters()
	if got := detached.NodesDeleted(); got != 1 {
		t.Errorf("NodesDeleted() = %d, want 1", got)
	}
	if got := detached.LabelsRemoved(); got != 0 {
		t.Errorf("LabelsRemoved() = %d after DETACH DELETE, want 0: a deleted node's "+
			"labels vanish WITH the node and are not a separate side effect", got)
	}
}

// TestE2E_ResultStats_MergeCreateVersusMatch is the distinction the audit called out by
// name, asserted where it matters — at the driver.
func TestE2E_ResultStats_MergeCreateVersusMatch(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	first := summaryFor(ctx, t, session, `MERGE (n:M {k: 1})`).Counters()
	if !first.ContainsUpdates() || first.NodesCreated() != 1 {
		t.Errorf("MERGE that creates: ContainsUpdates=%v NodesCreated=%d, want true and 1",
			first.ContainsUpdates(), first.NodesCreated())
	}

	second := summaryFor(ctx, t, session, `MERGE (n:M {k: 1})`).Counters()
	if second.ContainsUpdates() || second.NodesCreated() != 0 {
		t.Errorf("MERGE that matches: ContainsUpdates=%v NodesCreated=%d, want false and 0",
			second.ContainsUpdates(), second.NodesCreated())
	}
}

// TestE2E_ResultStats_ReadOnlyReportsNoUpdates pins that a read-only query's SUCCESS is
// unchanged: no stats map is sent, so the driver reports no updates.
func TestE2E_ResultStats_ReadOnlyReportsNoUpdates(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	summaryFor(ctx, t, session, `CREATE (:R {p: 1})`)

	c := summaryFor(ctx, t, session, `MATCH (n:R) RETURN n.p`).Counters()
	if c.ContainsUpdates() {
		t.Error("a read-only MATCH reported ContainsUpdates() = true")
	}
	if got := c.NodesCreated(); got != 0 {
		t.Errorf("a read-only MATCH reported NodesCreated() = %d, want 0", got)
	}
}

// TestE2E_ResultStats_SetAndRemoveBothCountAsPropertiesSet pins the one lossy step in the
// mapping: openCypher counts a property removal as its own -properties effect, but Bolt
// has only properties-set, so both are reported there.
func TestE2E_ResultStats_SetAndRemoveBothCountAsPropertiesSet(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	summaryFor(ctx, t, session, `CREATE (:S {a: 1, b: 2})`)

	set := summaryFor(ctx, t, session, `MATCH (n:S) SET n.c = 3`).Counters()
	if got := set.PropertiesSet(); got != 1 {
		t.Errorf("SET one property: PropertiesSet() = %d, want 1", got)
	}

	removed := summaryFor(ctx, t, session, `MATCH (n:S) REMOVE n.a`).Counters()
	if got := removed.PropertiesSet(); got != 1 {
		t.Errorf("REMOVE one property: PropertiesSet() = %d, want 1 — Bolt has no "+
			"properties-removed counter, so openCypher's -properties maps here", got)
	}
	if !removed.ContainsUpdates() {
		t.Error("ContainsUpdates() = false after a successful REMOVE")
	}
}
