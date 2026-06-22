package cypher_test

// scalar_fn_entity_test.go — correctness coverage for #1659 (audit H1).
//
// #1659 makes id/elementId/type/startNode/endNode/labels/keys over a bare bound
// node/relationship variable materialise only the field each function reads
// (skipping the entity's property map and every unrelated bound variable),
// instead of forcing full per-row materialisation. These tests pin that the
// observable result is unchanged. id()/elementId() are NOT covered by the
// openCypher TCK, so this is their primary regression guard; the partial-path
// behaviour for the others is additionally exercised by the TCK
// (labels x9, type x6, keys x6).
//
// The critical safety invariant — a partial entity value must never escape into
// a result row, only the extracted scalar/list — is exercised by the mixed and
// downstream-access cases (field extractor combined with a whole-entity use, and
// labels()/property access on a node returned by startNode(r)).

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedFieldExtractorGraph builds a tiny directed graph:
//
//	(a:Person {name:'A', age:30}) -[:KNOWS {since:2020, weight:5}]-> (b:Person {name:'B', age:40})
//
// with stable IDs captured so the field-extractor results can be checked exactly.
func seedFieldExtractorGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	run := func(q string) {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() { //nolint:revive // drain
		}
		if err := res.Err(); err != nil {
			t.Fatalf("seed drain %q: %v", q, err)
		}
		_ = res.Close()
	}
	run("CREATE (a:Person {name:'A', age:30})")
	run("CREATE (b:Person {name:'B', age:40})")
	run("MATCH (a:Person {name:'A'}),(b:Person {name:'B'}) CREATE (a)-[:KNOWS {since:2020, weight:5}]->(b)")
	return eng
}

// firstFieldRecord runs q and returns the first result record, failing if none.
func firstFieldRecord(t *testing.T, eng *cypher.Engine, q string) map[string]interface{} {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	defer res.Close() //nolint:errcheck // read-only query
	if !res.Next() {
		if err := res.Err(); err != nil {
			t.Fatalf("run %q: %v", q, err)
		}
		t.Fatalf("run %q: expected at least one row", q)
	}
	rec := res.Record()
	cp := make(map[string]interface{}, len(rec))
	for k, v := range rec {
		cp[k] = v
	}
	return cp
}

func TestScalarFnEntity_FieldExtractors(t *testing.T) {
	eng := seedFieldExtractorGraph(t)

	// Resolve the actual node/relationship ids via the whole-entity path so the
	// field-extractor results can be compared against ground truth.
	whole := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN a AS a, b AS b, r AS r")
	an, ok := whole["a"].(expr.NodeValue)
	if !ok {
		t.Fatalf("expected NodeValue for a, got %T", whole["a"])
	}
	bn, ok := whole["b"].(expr.NodeValue)
	if !ok {
		t.Fatalf("expected NodeValue for b, got %T", whole["b"])
	}
	rv, ok := whole["r"].(expr.RelationshipValue)
	if !ok {
		t.Fatalf("expected RelationshipValue for r, got %T", whole["r"])
	}

	t.Run("id(r) over relationship", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN id(r)")
		got, ok := rec["id(r)"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("id(r): expected IntegerValue, got %T (%v)", rec["id(r)"], rec["id(r)"])
		}
		if uint64(int64(got)) != rv.ID {
			t.Errorf("id(r) = %d, want %d", int64(got), rv.ID)
		}
	})

	t.Run("id(a) over node", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN id(a)")
		got, ok := rec["id(a)"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("id(a): expected IntegerValue, got %T", rec["id(a)"])
		}
		if uint64(int64(got)) != an.ID {
			t.Errorf("id(a) = %d, want %d", int64(got), an.ID)
		}
	})

	t.Run("type(r)", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN type(r)")
		got, ok := rec["type(r)"].(expr.StringValue)
		if !ok {
			t.Fatalf("type(r): expected StringValue, got %T", rec["type(r)"])
		}
		if string(got) != "KNOWS" {
			t.Errorf("type(r) = %q, want %q", string(got), "KNOWS")
		}
	})

	t.Run("labels(a)", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN labels(a)")
		got := stringList(t, rec["labels(a)"])
		if len(got) != 1 || got[0] != "Person" {
			t.Errorf("labels(a) = %v, want [Person]", got)
		}
	})

	t.Run("keys(r)", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN keys(r)")
		got := stringList(t, rec["keys(r)"])
		sort.Strings(got)
		if len(got) != 2 || got[0] != "since" || got[1] != "weight" {
			t.Errorf("keys(r) = %v, want [since weight]", got)
		}
	})

	t.Run("keys(a) over node", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN keys(a)")
		got := stringList(t, rec["keys(a)"])
		sort.Strings(got)
		if len(got) != 2 || got[0] != "age" || got[1] != "name" {
			t.Errorf("keys(a) = %v, want [age name]", got)
		}
	})

	t.Run("startNode(r) identity", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN startNode(r)")
		got, ok := rec["startNode(r)"].(expr.NodeValue)
		if !ok {
			t.Fatalf("startNode(r): expected NodeValue, got %T", rec["startNode(r)"])
		}
		if got.ID != an.ID {
			t.Errorf("startNode(r).ID = %d, want %d (a)", got.ID, an.ID)
		}
	})

	t.Run("endNode(r) identity", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN endNode(r)")
		got, ok := rec["endNode(r)"].(expr.NodeValue)
		if !ok {
			t.Fatalf("endNode(r): expected NodeValue, got %T", rec["endNode(r)"])
		}
		if got.ID != bn.ID {
			t.Errorf("endNode(r).ID = %d, want %d (b)", got.ID, bn.ID)
		}
	})
}

// TestScalarFnEntity_DownstreamAccess proves the no-escape invariant: a node
// returned by startNode(r)/endNode(r) still resolves its labels and properties,
// so the partial materialisation never truncates an entity that a downstream
// accessor reads.
func TestScalarFnEntity_DownstreamAccess(t *testing.T) {
	eng := seedFieldExtractorGraph(t)

	t.Run("startNode(r).name", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN startNode(r).name AS sn")
		got, ok := rec["sn"].(expr.StringValue)
		if !ok {
			t.Fatalf("startNode(r).name: expected StringValue, got %T (%v)", rec["sn"], rec["sn"])
		}
		if string(got) != "A" {
			t.Errorf("startNode(r).name = %q, want %q", string(got), "A")
		}
	})

	t.Run("endNode(r).name", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN endNode(r).name AS en")
		got, ok := rec["en"].(expr.StringValue)
		if !ok {
			t.Fatalf("endNode(r).name: expected StringValue, got %T (%v)", rec["en"], rec["en"])
		}
		if string(got) != "B" {
			t.Errorf("endNode(r).name = %q, want %q", string(got), "B")
		}
	})

	t.Run("labels(endNode(r))", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN labels(endNode(r)) AS le")
		got := stringList(t, rec["le"])
		if len(got) != 1 || got[0] != "Person" {
			t.Errorf("labels(endNode(r)) = %v, want [Person]", got)
		}
	})
}

// TestScalarFnEntity_MixedUse proves a variable used BOTH as a field-extractor
// argument and as a whole entity (or a scalar property) is still materialised
// correctly: the whole-entity use forces full materialisation, and the
// field-extractor still returns the right scalar.
func TestScalarFnEntity_MixedUse(t *testing.T) {
	eng := seedFieldExtractorGraph(t)

	t.Run("id(r) and whole r", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN id(r) AS rid, r AS r")
		rid, ok := rec["rid"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("rid: expected IntegerValue, got %T", rec["rid"])
		}
		rv, ok := rec["r"].(expr.RelationshipValue)
		if !ok {
			t.Fatalf("r: expected RelationshipValue, got %T", rec["r"])
		}
		if uint64(int64(rid)) != rv.ID {
			t.Errorf("id(r)=%d but r.ID=%d", int64(rid), rv.ID)
		}
		// The whole-entity use must carry the full property map.
		if rv.Properties == nil || len(rv.Properties) != 2 {
			t.Errorf("whole r must carry its 2 properties, got %v", rv.Properties)
		}
	})

	t.Run("labels(a) and a.name", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN labels(a) AS labs, a.name AS nm")
		labs := stringList(t, rec["labs"])
		if len(labs) != 1 || labs[0] != "Person" {
			t.Errorf("labels(a) = %v, want [Person]", labs)
		}
		nm, ok := rec["nm"].(expr.StringValue)
		if !ok {
			t.Fatalf("a.name: expected StringValue, got %T", rec["nm"])
		}
		if string(nm) != "A" {
			t.Errorf("a.name = %q, want %q", string(nm), "A")
		}
	})

	t.Run("keys(a) and a.age", func(t *testing.T) {
		rec := firstFieldRecord(t, eng, "MATCH (a)-[r]->(b) RETURN keys(a) AS ks, a.age AS ag")
		ks := stringList(t, rec["ks"])
		sort.Strings(ks)
		if len(ks) != 2 || ks[0] != "age" || ks[1] != "name" {
			t.Errorf("keys(a) = %v, want [age name]", ks)
		}
		ag, ok := rec["ag"].(expr.IntegerValue)
		if !ok {
			t.Fatalf("a.age: expected IntegerValue, got %T", rec["ag"])
		}
		if int64(ag) != 30 {
			t.Errorf("a.age = %d, want 30", int64(ag))
		}
	})
}

// TestScalarFnEntity_NullInput pins the null short-circuit of every extractor,
// reached via OPTIONAL MATCH producing a null relationship/node.
func TestScalarFnEntity_NullInput(t *testing.T) {
	eng := seedFieldExtractorGraph(t)
	// No :Missing node exists, so OPTIONAL MATCH binds r and m to null.
	const q = "MATCH (a:Person {name:'A'}) OPTIONAL MATCH (a)-[r:NOPE]->(m) " +
		"RETURN id(r) AS idr, type(r) AS tr, startNode(r) AS sn, keys(r) AS kr, labels(m) AS lm"
	rec := firstFieldRecord(t, eng, q)
	for _, col := range []string{"idr", "tr", "sn", "kr", "lm"} {
		v := rec[col]
		if v == nil {
			continue // absent / untyped nil counts as null
		}
		ev, ok := v.(expr.Value)
		if !ok {
			t.Errorf("%s: result value is not an expr.Value, got %T", col, v)
			continue
		}
		if !expr.IsNull(ev) {
			t.Errorf("%s over null input = %v (%T), want null", col, ev, ev)
		}
	}
}

// stringList coerces a result value to a []string, failing the test otherwise.
func stringList(t *testing.T, v interface{}) []string {
	t.Helper()
	lv, ok := v.(expr.ListValue)
	if !ok {
		t.Fatalf("expected ListValue, got %T (%v)", v, v)
	}
	out := make([]string, len(lv))
	for i, e := range lv {
		sv, ok := e.(expr.StringValue)
		if !ok {
			t.Fatalf("expected StringValue element, got %T", e)
		}
		out[i] = string(sv)
	}
	return out
}
