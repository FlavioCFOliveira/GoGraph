package cypher_test

// set_entity_property_value_test.go — regression gate for rmp #2816: SET must
// REFUSE a node-, relationship- or path-valued property, never accept the
// statement and store nothing.
//
// openCypher 9 §3.2 restricts a property value to a primitive or a homogeneous
// list of primitives. A graph entity is therefore InvalidPropertyType. The
// engine already knew this for a MAP right-hand side — `SET a.m = {x: 1}`
// raised — but every entity-valued right-hand side took a different route: the
// storage conversion reported "cannot convert", each write path read that as
// "no value produced", and the statement returned ok having written nothing.
//
// Two consequences made this a Consistency defect rather than a cosmetic one:
//
//  1. the statement reported success for an effect it did not apply, so a caller
//     had no way to learn the property was never stored; and
//  2. on the REPLACE forms (`SET n = {…}`) the dropped entry still cleared every
//     key the entity already carried, so `SET a = {other: b}` reported ok and
//     DESTROYED a's existing properties.
//
// The table below drives every write path that can carry a property value —
// plain SET, SET +=, SET =, and MERGE's ON CREATE SET / ON MATCH SET, on nodes
// and on relationships — with a node, a relationship, a path, and a list
// containing a node. Each must raise InvalidPropertyType and leave the graph
// untouched.
//
// The controls at the end are what keeps the gate honest: `SET a = b` (the
// entity-COPY form openCypher explicitly allows) and an ordinary scalar write
// must still succeed, and the pre-existing map refusal must still raise its own
// message. A fix that refused too much would fail those.
//
// Layer: short.

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// entityPropSeed is the fixture every case below starts from: three :X nodes
// keyed a, b and c, with a single relationship a→c so a relationship and a path
// are both bindable.
const entityPropSeed = `CREATE (a:X {key:'a'})-[:R]->(c:X {key:'c'}), (b:X {key:'b'})`

// newEntityPropEngine returns an engine over a fresh multigraph seeded with
// [entityPropSeed].
func newEntityPropEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	if _, err := runEntityProp(eng, entityPropSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return eng
}

// runEntityProp executes query and returns the first error it produces, whether
// raised at plan time or during iteration.
func runEntityProp(eng *cypher.Engine, query string) ([]map[string]any, error) {
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	for res.Next() {
		rec := res.Record()
		row := make(map[string]any, len(rec))
		for k, v := range rec {
			row[k] = v
		}
		rows = append(rows, row)
	}
	iterErr := res.Err()
	closeErr := res.Close()
	if iterErr != nil {
		return rows, iterErr
	}
	return rows, closeErr
}

// entityPropKeys returns "key=[k1 k2 …]" for every :X node, ordered by key, so a
// case can assert the graph is byte-for-byte what the seed left behind.
func entityPropKeys(t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	rows, err := runEntityProp(eng, `MATCH (n:X) RETURN n.key AS k, keys(n) AS ks ORDER BY k`)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	var sb strings.Builder
	for _, r := range rows {
		sb.WriteString(strings.TrimSpace(fmtAny(r["k"])))
		sb.WriteString("=")
		sb.WriteString(fmtAny(r["ks"]))
		sb.WriteString(" ")
	}
	return strings.TrimSpace(sb.String())
}

// TestSetEntityValuedProperty_RaisesInvalidPropertyType is the #2816 gate: every
// SET form that receives an entity-valued property must raise
// InvalidPropertyType and leave the graph unchanged.
//
// It fails on the pre-fix engine because each of these statements returned a nil
// error (verified by reverting the guards: all eleven subtests reported
// "want InvalidPropertyType, got <nil>", and `replaceNodeValue` additionally
// reported the destroyed `a=[]`).
func TestSetEntityValuedProperty_RaisesInvalidPropertyType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
	}{
		// ── plain SET n.k = <entity> ──────────────────────────────────────────
		{"plainNodeValue", `MATCH (a:X {key:'a'}), (b:X {key:'b'}) SET a.other = b`},
		{"plainRelValue", `MATCH (a:X {key:'a'})-[r:R]->(), (b:X {key:'b'}) SET b.other = r`},
		{"plainPathValue", `MATCH p = (a:X {key:'a'})-[:R]->() MATCH (b:X {key:'b'}) SET b.other = p`},
		{"plainListOfNode", `MATCH (a:X {key:'a'}), (b:X {key:'b'}) SET a.other = [b]`},
		{"relTargetNodeValue", `MATCH (a:X {key:'a'})-[r:R]->(), (b:X {key:'b'}) SET r.other = b`},

		// ── SET n += {…} and SET n = {…} ─────────────────────────────────────
		{"mutateNodeValue", `MATCH (a:X {key:'a'}), (b:X {key:'b'}) SET a += {other: b}`},
		{"replaceNodeValue", `MATCH (a:X {key:'a'}), (b:X {key:'b'}) SET a = {other: b}`},
		{"relReplaceNodeValue", `MATCH (a:X {key:'a'})-[r:R]->(), (b:X {key:'b'}) SET r = {other: b}`},

		// ── MERGE ON CREATE SET / ON MATCH SET ───────────────────────────────
		{"mergeOnCreateScalar", `MATCH (b:X {key:'b'}) MERGE (m:Zed {k:1}) ON CREATE SET m.other = b`},
		{"mergeOnMatchScalar", `MATCH (b:X {key:'b'}) MERGE (a:X {key:'a'}) ON MATCH SET a.other = b`},
		{"mergeOnCreateReplace", `MATCH (b:X {key:'b'}) MERGE (m:Zed {k:1}) ON CREATE SET m = {other: b}`},
		{"mergeOnMatchMutate", `MATCH (b:X {key:'b'}) MERGE (a:X {key:'a'}) ON MATCH SET a += {other: b}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := newEntityPropEngine(t)
			before := entityPropKeys(t, eng)

			_, err := runEntityProp(eng, tc.query)
			if err == nil {
				t.Fatalf("SET with an entity-valued property reported success and wrote nothing;\n  query: %s\n  want InvalidPropertyType, got <nil>", tc.query)
			}
			if !strings.Contains(err.Error(), "InvalidPropertyType") {
				t.Fatalf("error does not name InvalidPropertyType;\n  query: %s\n  got:   %v", tc.query, err)
			}

			// The refusal must also be atomic: nothing written, and — the
			// REPLACE forms' specific trap — nothing CLEARED either.
			if after := entityPropKeys(t, eng); after != before {
				t.Fatalf("refused SET still changed the graph;\n  query:  %s\n  before: %s\n  after:  %s", tc.query, before, after)
			}
		})
	}
}

// TestSetEntityValuedProperty_Controls pins the behaviour the #2816 guard must
// NOT disturb. Without these, a guard that refused every entity-shaped
// right-hand side would look correct: `SET a = b` is the openCypher entity-COPY
// form and is legal, and an ordinary scalar write must be unaffected.
func TestSetEntityValuedProperty_Controls(t *testing.T) {
	t.Parallel()
	t.Run("copyEntityIsLegal", func(t *testing.T) {
		t.Parallel()
		eng := newEntityPropEngine(t)
		// `SET a = b` replaces a's properties with b's — a bare entity as the
		// whole right-hand side, not as a property VALUE. openCypher allows it.
		if _, err := runEntityProp(eng, `MATCH (a:X {key:'a'}), (b:X {key:'b'}) SET a = b`); err != nil {
			t.Fatalf("SET a = b (entity copy) must succeed: %v", err)
		}
		rows, err := runEntityProp(eng, `MATCH (n:X {key:'b'}) RETURN count(n) AS c`)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		// a now carries b's key, so two nodes answer key='b'.
		if got := fmtAny(rows[0]["c"]); got != "2" {
			t.Fatalf("after SET a = b, count of key='b' = %s, want 2 (a took b's properties)", got)
		}
	})

	t.Run("scalarWriteUnaffected", func(t *testing.T) {
		t.Parallel()
		eng := newEntityPropEngine(t)
		if _, err := runEntityProp(eng, `MATCH (a:X {key:'a'}) SET a.n = 42`); err != nil {
			t.Fatalf("ordinary scalar SET must succeed: %v", err)
		}
		rows, err := runEntityProp(eng, `MATCH (a:X {key:'a'}) RETURN a.n AS n`)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		if got := fmtAny(rows[0]["n"]); got != "42" {
			t.Fatalf("a.n = %s, want 42", got)
		}
	})

	t.Run("mapRefusalUnchanged", func(t *testing.T) {
		t.Parallel()
		eng := newEntityPropEngine(t)
		_, err := runEntityProp(eng, `MATCH (a:X {key:'a'}) SET a.m = {x: 1}`)
		if err == nil {
			t.Fatal("SET a.m = {x: 1} must still raise InvalidPropertyType")
		}
		// The map case keeps its own diagnostic; the entity guard must not have
		// swallowed or reworded it.
		if !strings.Contains(err.Error(), "maps cannot be stored") {
			t.Fatalf("map refusal lost its own message: %v", err)
		}
	})
}
