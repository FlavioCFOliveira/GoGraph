package server_test

// rel_accessor_agreement_wire_test.go — rmp #2815, over the Bolt wire.
//
// The in-process gate is cypher/rel_accessor_agreement_test.go. This file is its
// wire-level counterpart, and it exists because the report came from a Bolt
// client, not from the embedded engine API: a divergence introduced by the
// server's own result encoding — the PackStream Relationship structure carries
// its properties in a field the scalar and map projections never touch — would
// be invisible to every in-process test in the module.
//
// Nothing pinned that. The four reads reach the client by three different
// encodings:
//
//   - `e.stamp`      → a bare PackStream scalar
//   - `properties(e)`→ a PackStream map
//   - `keys(e)`      → a PackStream list
//   - `RETURN e`      → the Bolt Relationship structure (tag 0x52), whose
//     properties field is built separately from all of the above, and whose
//     shape differs between Bolt 4.4 (5 fields) and 5.x (8 fields)
//
// #2815 alleged exactly a split between the first three and the fourth, so the
// agreement is asserted here across the encodings, through the official
// neo4j-go-driver, which is what the reporting client uses.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// relWireMatch is the reporter's read pattern.
const relWireMatch = `MATCH (:T {key:'s'})-[e:R]->(:T {key:'t'}) `

// relWireAccessors reads the four accessors over the wire and returns the rows
// of each, in the order (scalar, properties, keys, whole).
func relWireAccessors(ctx context.Context, t *testing.T, sess neo4j.SessionWithContext) (scalar, props, keys, whole []map[string]any) {
	t.Helper()
	return runRead(ctx, t, sess, relWireMatch+`RETURN e.stamp AS v`, nil),
		runRead(ctx, t, sess, relWireMatch+`RETURN properties(e) AS v`, nil),
		runRead(ctx, t, sess, relWireMatch+`RETURN keys(e) AS v`, nil),
		runRead(ctx, t, sess, relWireMatch+`RETURN e AS v`, nil)
}

// TestRelAccessorsOverBolt_AgreeOnCleanGraph is the #2815 acceptance gate over
// the wire: on a graph holding exactly one matching relationship, each accessor
// returns exactly ONE row and all four describe the same property set.
//
// The single-row assertion is load-bearing. It is what distinguishes a genuine
// accessor split — which is what #2815 alleged — from the two-row result a
// duplicate relationship produces, where a reader looking only at the first row
// of each query sees the same symptom with nothing wrong.
func TestRelAccessorsOverBolt_AgreeOnCleanGraph(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)

	runWrite(ctx, t, sess, `CREATE (a:T {key:'s'})-[e:R]->(b:T {key:'t'}) SET e = {stamp:'M'}`, nil)

	scalar, props, keys, whole := relWireAccessors(ctx, t, sess)
	for name, rows := range map[string][]map[string]any{
		"e.stamp": scalar, "properties(e)": props, "keys(e)": keys, "RETURN e": whole,
	} {
		if len(rows) != 1 {
			t.Fatalf("%s returned %d rows over Bolt, want 1 — the fixture is not the single-edge shape this gate needs", name, len(rows))
		}
	}

	sv := scalar[0]["v"]
	pv, _ := props[0]["v"].(map[string]any)
	kv, _ := keys[0]["v"].([]any)
	rel, ok := whole[0]["v"].(neo4j.Relationship)
	if !ok {
		t.Fatalf("RETURN e materialised as %T, not a neo4j.Relationship — the server is not sending the Bolt Relationship structure", whole[0]["v"])
	}

	if sv != "M" {
		t.Errorf("e.stamp over Bolt = %#v, want \"M\"", sv)
	}
	if got, ok := pv["stamp"]; !ok || got != "M" {
		t.Errorf("properties(e) over Bolt = %#v, want {stamp: M}", pv)
	}
	if len(kv) != 1 || kv[0] != "stamp" {
		t.Errorf("keys(e) over Bolt = %#v, want [stamp]", kv)
	}
	if got, ok := rel.Props["stamp"]; !ok || got != "M" {
		t.Errorf("RETURN e carried props %#v over the wire, want {stamp: M}", rel.Props)
	}

	// The claim #2815 made, stated directly against the wire encodings: the
	// scalar projection and the map projection cannot disagree about one
	// stored property, and neither may disagree with the Relationship
	// structure's own properties field.
	if (sv == nil) != (len(pv) == 0) {
		t.Errorf("e.stamp = %#v but properties(e) = %#v — the scalar and map encodings disagree", sv, pv)
	}
	if (len(pv) == 0) != (len(rel.Props) == 0) {
		t.Errorf("properties(e) = %#v but the Relationship structure carried %#v — the map and structure encodings disagree", pv, rel.Props)
	}
	if (len(kv) == 0) != (len(pv) == 0) {
		t.Errorf("keys(e) = %#v but properties(e) = %#v — the list and map encodings disagree", kv, pv)
	}
}

// TestRelAccessorsOverBolt_DuplicateEdgeReturnsTwoRowsEverywhere records, as
// evidence rather than inference, what a SECOND matching relationship does over
// the wire: EVERY one of the four reads returns two rows, and the accessors
// agree row by row.
//
// This matters for reading the #2815 report. CREATE always creates, so running
// its script against a database that already held such an edge leaves two, and
// the reads then return two rows — one of which correctly has no stamp. That
// produces the reported symptom with nothing wrong, but ONLY for a reader
// looking at a single row per query: the wire itself always carries both rows,
// for all four accessors alike. A client that renders every row would show two.
func TestRelAccessorsOverBolt_DuplicateEdgeReturnsTwoRowsEverywhere(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)
	sess := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer sess.Close(ctx)

	runWrite(ctx, t, sess, `CREATE (a:T {key:'s'})-[e:R]->(b:T {key:'t'})`, nil)
	runWrite(ctx, t, sess, `CREATE (a:T {key:'s'})-[e:R]->(b:T {key:'t'}) SET e = {stamp:'M'}`, nil)

	scalar, props, keys, whole := relWireAccessors(ctx, t, sess)
	for name, rows := range map[string][]map[string]any{
		"e.stamp": scalar, "properties(e)": props, "keys(e)": keys, "RETURN e": whole,
	} {
		if len(rows) != 2 {
			t.Errorf("%s returned %d rows over Bolt, want 2 — every accessor must carry BOTH relationships, so a two-row result can never be mistaken for an accessor split", name, len(rows))
		}
	}
	if t.Failed() {
		return
	}

	// Exactly one of the two carries the stamp, and the accessors agree about
	// which — whatever order the rows arrive in (the order is not specified and
	// was measured to vary run to run).
	stampedScalar, stampedProps := 0, 0
	for i := range scalar {
		if scalar[i]["v"] != nil {
			stampedScalar++
		}
		if pv, _ := props[i]["v"].(map[string]any); len(pv) != 0 {
			stampedProps++
		}
	}
	if stampedScalar != 1 || stampedProps != 1 {
		t.Errorf("stamped rows: e.stamp reported %d, properties(e) reported %d, want exactly 1 each", stampedScalar, stampedProps)
	}
}
