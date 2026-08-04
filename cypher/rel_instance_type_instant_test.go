package cypher_test

// rel_instance_type_instant_test.go — rmp #2295: per-instance relationship-type
// attribution survives the CROSS-INSTANT comparison in buildRelationshipValueFromRow.
//
// The guard there compares totalCreates (from the UNVERSIONED EdgeCreateCount, which
// answers from the present) against parallelCount (from the forward CSR, which since
// rmp #2293 answers at the reader's instant). The call site traces why that is safe;
// this is the test that makes the trace checkable rather than merely argued.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestRelInstanceType_HoldsAcrossConcurrentCreateAndDelete drives both directions of
// the skew and asserts that every returned relationship reports a type that BELONGS
// to the pair, never one invented by comparing two instants.
//
// # Why this shape
//
// Three parallel relationships of DISTINCT types on one pair, so a mis-attribution
// is observable at all: with one type, or with types shared across instances, the
// wrong answer and the right answer coincide and the test would prove nothing.
//
//   - skew UP is a CREATE after the read transaction's snapshot, which makes
//     totalCreates exceed the snapshot's parallelCount, so the guard DECLINES to the
//     per-pair union. The union is a SUPERSET of the pair's types, so what must hold
//     is that every reported type is one of the pair's — a decline is a loss of
//     precision and must not become a wrong answer.
//   - skew DOWN is a DELETE after the snapshot, which decrements the counter
//     (DecEdgeCreateCount) so the guard ADMITS. What it admits is a snapshot-derived
//     index against the versioned per-instance store, so the reported types must be
//     exactly the three the snapshot can see — the deleted one included, because the
//     read transaction began before the delete.
//
// Both arms run inside an EXPLICIT read transaction, which is what pins the
// snapshot across statements (rmp #2307); without one each statement would open its
// own instant and the skew this test exists for could not be constructed.
func TestRelInstanceType_HoldsAcrossConcurrentCreateAndDelete(t *testing.T) {
	for _, arm := range []struct {
		name string
		// after is the statement run AFTER the snapshot is taken, producing the skew.
		after string
		// wantTypes is the set of types the snapshot's reader must report.
		wantTypes []string
	}{
		{
			name:      "skew up: a CREATE after the snapshot",
			after:     "MATCH (a:A), (b:B) CREATE (a)-[:R4]->(b)",
			wantTypes: []string{"R1", "R2", "R3"},
		},
		{
			name:      "skew down: a DELETE after the snapshot",
			after:     "MATCH (:A)-[r:R3]->(:B) DELETE r",
			wantTypes: []string{"R1", "R2", "R3"},
		},
	} {
		t.Run(arm.name, func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			eng := cypher.NewEngine(g)
			defer func() {
				if err := eng.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			}()

			seed := "CREATE (a:A), (b:B), (a)-[:R1]->(b), (a)-[:R2]->(b), (a)-[:R3]->(b)"
			if _, err := eng.RunInTx(context.Background(), seed, nil); err != nil {
				t.Fatalf("seed: %v", err)
			}

			// The snapshot. Its first statement forces the instant to be taken before
			// the skew below.
			tx, err := eng.BeginReadTx(context.Background())
			if err != nil {
				t.Fatalf("BeginReadTx: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			if got := typesFromTx(t, tx); !sameTypeSet(got, arm.wantTypes) {
				t.Fatalf("before the skew the snapshot reports %v, want %v", got, arm.wantTypes)
			}

			// THE SKEW, committed by another transaction while the snapshot is held.
			if _, err := eng.RunInTx(context.Background(), arm.after, nil); err != nil {
				t.Fatalf("skew statement %q: %v", arm.after, err)
			}

			// The snapshot must still report exactly its own three types. A
			// mis-attribution shows up as a type outside the set; a lost row as a
			// missing one.
			if got := typesFromTx(t, tx); !sameTypeSet(got, arm.wantTypes) {
				t.Errorf("after the skew the held snapshot reports %v, want %v — the "+
					"cross-instant comparison in buildRelationshipValueFromRow changed the "+
					"answer", got, arm.wantTypes)
			}
		})
	}
}

// typesFromTx returns type(r) for every relationship on the seeded pair, as the
// transaction's snapshot sees it.
func typesFromTx(t *testing.T, tx *cypher.ExplicitTx) []string {
	t.Helper()
	res, err := tx.Exec("MATCH (:A)-[r]->(:B) RETURN type(r) AS t", nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	defer res.Close()
	var out []string
	for res.Next() {
		rec := res.Record()
		v, ok := rec["t"]
		if !ok {
			t.Fatalf("the projection has no column t; record is %v", rec)
		}
		sv, ok := v.(expr.StringValue)
		if !ok {
			t.Fatalf("type(r) is %T (%v), want a string", v, v)
		}
		out = append(out, string(sv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
	return out
}

// sameTypeSet compares two type multisets irrespective of order.
func sameTypeSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, s := range got {
		seen[s]++
	}
	for _, s := range want {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}
