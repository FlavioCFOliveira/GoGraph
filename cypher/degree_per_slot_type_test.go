package cypher

// degree_per_slot_type_test.go — the regression gate for rmp #2258: a
// relationship's TYPE is a property of the relationship INSTANCE, so every
// spelling of "how many :K edges leave a" must resolve the type PER SLOT and
// therefore return the same number.
//
// Why the pre-existing suites missed it. cypher/degree_parallel_edges_test.go
// (the #2241 gate) builds every parallel edge through Cypher CREATE, so every
// slot carries a stable per-edge handle and the type is resolved from the
// handle-keyed store; the adjacency label column is never the deciding source
// there. graph/lpg's differentials build through the Go API but never place two
// parallel edges of DIFFERENT typing on one pair. The defect lived exactly in the
// gap: a pair with several HANDLE-LESS slots, where the label column was written
// per-PAIR (first matching slot only) and read per-SLOT.
//
// Every case below carries a HAND-COMPUTED ABSOLUTE count. A differential between
// two spellings is not enough on its own: in this repository a differential has
// twice gone green over a real defect because both arms shared the broken code —
// here, for instance, the typed rewrite and the enumeration agreed on 2 for a
// pair holding one typed and one untyped edge, and both were wrong.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// perSlotFixture returns an empty directed multigraph holding three :P nodes with
// Go-API keys "a", "b", "c" and id properties 0, 1, 2. "b" also carries :Q, so a
// far-node label predicate has something to discriminate on: the (a, b) pair
// satisfies `->(:Q)` and the (a, c) pair does not.
//
// The keys are the Go API's own, which is what lets a case mix a Cypher CREATE
// (handle-carrying slot) and a Go-API AddEdge (handle-less slot) on ONE pair.
func perSlotFixture(t *testing.T) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i, k := range []string{"a", "b", "c"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q, P): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(%q, id): %v", k, err)
		}
	}
	if err := g.SetNodeLabel("b", "Q"); err != nil {
		t.Fatalf("SetNodeLabel(b, Q): %v", err)
	}
	return g, NewEngine(g)
}

// createTyped issues a Cypher CREATE of one typed edge between the fixture nodes
// identified by their id properties, so the new slot carries a stable per-edge
// handle and its type is recorded against that handle.
func createTyped(t *testing.T, eng *Engine, srcID, dstID int, relType string) {
	t.Helper()
	mustRun(t, eng, fmt.Sprintf(
		"MATCH (x:P {id: %d}), (y:P {id: %d}) CREATE (x)-[:%s]->(y)", srcID, dstID, relType))
}

// perSlotCase is one shape of the (a → far) pair together with the hand-computed
// number of :K and :M relationships leaving "a".
type perSlotCase struct {
	name  string
	build func(t *testing.T, g *lpg.Graph[string, float64], eng *Engine)
	// far is the destination key of the pair under test; farQ records whether it
	// carries :Q, which fixes what the labelled spellings must return.
	far  string
	farQ bool
	// wantDeg / wantK / wantM are ABSOLUTE, hand-computed counts for node "a".
	wantDeg int
	wantK   int
	wantM   int
}

func perSlotCases() []perSlotCase {
	return []perSlotCase{
		{
			// SetEdgeLabel names the PAIR, so it types both handle-less slots.
			name: "two handle-less parallel slots typed by SetEdgeLabel",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "b", 2)
				g.SetEdgeLabel("a", "b", "K")
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 2, wantM: 0,
		},
		{
			name: "three handle-less parallel slots typed by SetEdgeLabel",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "b", 3)
				g.SetEdgeLabel("a", "b", "K")
			},
			far: "b", farQ: true, wantDeg: 3, wantK: 3, wantM: 0,
		},
		{
			// AddEdgeLabeled types only its OWN slot, so the untyped sibling is not
			// a :K relationship. This is the row the pre-fix ENUMERATION got wrong
			// (it reported 2), while the typed rewrite was already right.
			name: "AddEdgeLabeled then an untyped parallel AddEdge",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
					t.Fatalf("AddEdgeLabeled: %v", err)
				}
				addEdges(t, g, "a", "b", 1)
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 1, wantM: 0,
		},
		{
			// Different types on one pair, built per-slot: one K relationship and
			// one M relationship.
			name: "different types via AddEdgeLabeled twice",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				if err := g.AddEdgeLabeled("a", "b", 1, "K"); err != nil {
					t.Fatalf("AddEdgeLabeled(K): %v", err)
				}
				if err := g.AddEdgeLabeled("a", "b", 1, "M"); err != nil {
					t.Fatalf("AddEdgeLabeled(M): %v", err)
				}
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 1, wantM: 1,
		},
		{
			// Different types on one pair, built by naming the pair TWICE. The
			// first call types both free slots, so the second finds none free and
			// the type applies to both slots as well: two relationships that each
			// carry both types. That is what naming the pair means, and it is why
			// the interleaved build in the next case differs.
			name: "different types via SetEdgeLabel twice on two parallel slots",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "b", 2)
				g.SetEdgeLabel("a", "b", "K")
				g.SetEdgeLabel("a", "b", "M")
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 2, wantM: 2,
		},
		{
			// Interleaved: each SetEdgeLabel finds exactly one free slot, so the
			// two relationships end up with one type each. Distinguishing this from
			// the case above is only possible because the types are stored per-slot.
			name: "different types via interleaved AddEdge and SetEdgeLabel",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "b", 1)
				g.SetEdgeLabel("a", "b", "K")
				addEdges(t, g, "a", "b", 1)
				g.SetEdgeLabel("a", "b", "M")
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 1, wantM: 1,
		},
		{
			// A single slot named twice: the one relationship carries both types,
			// the second of which has no free slot and lives in the pair's overflow.
			name: "one slot named twice puts the second type in overflow",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "b", 1)
				g.SetEdgeLabel("a", "b", "K")
				g.SetEdgeLabel("a", "b", "M")
			},
			far: "b", farQ: true, wantDeg: 1, wantK: 1, wantM: 1,
		},
		{
			// MIXED: one Cypher-created handle-carrying slot plus one Go-API
			// handle-less slot, both typed :K. The handle record answers for the
			// first, the label column for the second.
			name: "mixed handle-carrying and handle-less slots both typed K",
			build: func(t *testing.T, g *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 1, "K")
				addEdges(t, g, "a", "b", 1)
				g.SetEdgeLabel("a", "b", "K")
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 2, wantM: 0,
		},
		{
			// MIXED, and the handle-less slot is left UNTYPED: only the
			// Cypher-created relationship is a :K.
			name: "mixed handle-carrying K and an untyped handle-less slot",
			build: func(t *testing.T, g *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 1, "K")
				addEdges(t, g, "a", "b", 1)
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 1, wantM: 0,
		},
		{
			// MIXED with different types: the Cypher slot is :M, the Go-API slot
			// is typed :K by naming the pair. Naming the pair must not overwrite
			// what the handle record says about the other slot.
			name: "mixed handle-carrying M and a handle-less slot named K",
			build: func(t *testing.T, g *lpg.Graph[string, float64], eng *Engine) {
				createTyped(t, eng, 0, 1, "M")
				addEdges(t, g, "a", "b", 1)
				g.SetEdgeLabel("a", "b", "K")
			},
			far: "b", farQ: true, wantDeg: 2, wantK: 1, wantM: 1,
		},
		{
			// Three Cypher-created parallel :K edges. The unbounded typed degree
			// used to take a column-only fast path that never consulted the handle
			// store and reported 1.
			name: "three Cypher-created parallel K edges",
			build: func(t *testing.T, _ *lpg.Graph[string, float64], eng *Engine) {
				for i := 0; i < 3; i++ {
					createTyped(t, eng, 0, 1, "K")
				}
			},
			far: "b", farQ: true, wantDeg: 3, wantK: 3, wantM: 0,
		},
		{
			// The far node does NOT carry :Q, so every labelled spelling is 0 while
			// the unlabelled ones are 2.
			name: "two handle-less parallel slots to a far node without the label",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "c", 2)
				g.SetEdgeLabel("a", "c", "K")
			},
			far: "c", farQ: false, wantDeg: 2, wantK: 2, wantM: 0,
		},
		{
			// SELF-LOOP: eleven untyped slots plus one typed slot. The pre-fix
			// enumeration reported twelve :T0 relationships because it read the
			// pair's derived union once per slot.
			name: "self-loop with eleven untyped slots and one typed slot",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "a", 11)
				if err := g.AddEdgeLabeled("a", "a", 1, "K"); err != nil {
					t.Fatalf("AddEdgeLabeled: %v", err)
				}
			},
			far: "a", farQ: false, wantDeg: 12, wantK: 1, wantM: 0,
		},
		{
			// SELF-LOOP whose untyped slots are then named: every one of the twelve
			// slots is a :K relationship.
			name: "self-loop with twelve slots all named by SetEdgeLabel",
			build: func(t *testing.T, g *lpg.Graph[string, float64], _ *Engine) {
				addEdges(t, g, "a", "a", 12)
				g.SetEdgeLabel("a", "a", "K")
			},
			far: "a", farQ: false, wantDeg: 12, wantK: 12, wantM: 0,
		},
	}
}

// addEdges appends n untyped parallel edges from src to dst through the Go API,
// so each lands on its own handle-less adjacency slot.
func addEdges(t *testing.T, g *lpg.Graph[string, float64], src, dst string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := g.AddEdge(src, dst, 1); err != nil {
			t.Fatalf("AddEdge(%q, %q): %v", src, dst, err)
		}
	}
}

// TestPerSlotRelType_EngineSpellingsAgreeWithAbsoluteOracle is acceptance criteria
// 1 and 2 of rmp #2258: every Cypher spelling of the typed count — the degree
// rewrite, the labelled-far-node rewrite, the relationship-variable enumeration,
// the pattern comprehension, the bare MATCH, and EXISTS — returns the
// hand-computed absolute number.
func TestPerSlotRelType_EngineSpellingsAgreeWithAbsoluteOracle(t *testing.T) {
	for _, tc := range perSlotCases() {
		t.Run(tc.name, func(t *testing.T) {
			g, eng := perSlotFixture(t)
			tc.build(t, g, eng)

			// The untyped degree counts slots and was never in question; asserting
			// it pins the fixture, so a wrong typed count cannot be blamed on a
			// fixture that did not build what the case claims.
			if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-->() }"); got[0] != enc(tc.wantDeg) {
				t.Fatalf("fixture not as claimed: COUNT { (a)-->() } = %v, want %s", got, enc(tc.wantDeg))
			}

			for _, ty := range []struct {
				relType string
				want    int
			}{{"K", tc.wantK}, {"M", tc.wantM}} {
				// Unlabelled far node: every spelling must return want.
				for _, q := range []string{
					// The typed degree rewrite.
					"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:%s]->() }",
					// The enumerating oracle: binding a relationship variable
					// forbids any degree shortcut.
					"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[r:%s]->(x) }",
					// The pattern comprehension, which enumerates through a
					// different operator chain.
					"MATCH (a:P {id: 0}) RETURN size([ (a)-[:%s]->(x) | 1 ])",
					// The plain MATCH, which goes through the edge-type filter.
					"MATCH (a:P {id: 0})-[r:%s]->() RETURN count(r)",
				} {
					query := fmt.Sprintf(q, ty.relType)
					if got := degreeRun(t, eng, query); got[0] != enc(ty.want) {
						t.Errorf("%s = %v, want %s", query, got, enc(ty.want))
					}
				}

				// Labelled far node: want when the far node carries :Q, else 0.
				wantQ := 0
				if tc.farQ {
					wantQ = ty.want
				}
				for _, q := range []string{
					"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:%s]->(:Q) }",
					"MATCH (a:P {id: 0}) RETURN COUNT { (a)-[r:%s]->(x:Q) }",
					"MATCH (a:P {id: 0}) RETURN size([ (a)-[:%s]->(x:Q) | 1 ])",
				} {
					query := fmt.Sprintf(q, ty.relType)
					if got := degreeRun(t, eng, query); got[0] != enc(wantQ) {
						t.Errorf("%s = %v, want %s", query, got, enc(wantQ))
					}
				}

				// EXISTS must agree with the count being non-zero, in both the
				// unlabelled and the labelled spelling.
				for _, e := range []struct {
					query string
					want  bool
				}{
					{fmt.Sprintf("MATCH (a:P {id: 0}) RETURN EXISTS { (a)-[:%s]->() }", ty.relType), ty.want > 0},
					{fmt.Sprintf("MATCH (a:P {id: 0}) RETURN EXISTS { (a)-[:%s]->(:Q) }", ty.relType), wantQ > 0},
				} {
					if got := degreeRun(t, eng, e.query); got[0] != fmt.Sprintf("%t\x1f", e.want) {
						t.Errorf("%s = %v, want %t", e.query, got, e.want)
					}
				}
			}
		})
	}
}

// TestPerSlotRelType_BoundedAndUnboundedDegreesAgree pins the lpg-level absolute
// counts behind the engine's rewrites, and the contract the bounded and unbounded
// walkers document: they may differ about WHEN they stop counting, never about
// WHICH edges count. The unbounded form used to take a column-only fast path that
// consulted neither the per-handle store nor the pair's overflow list, so it could
// disagree with the bounded form it shares its documentation with.
func TestPerSlotRelType_BoundedAndUnboundedDegreesAgree(t *testing.T) {
	for _, tc := range perSlotCases() {
		t.Run(tc.name, func(t *testing.T) {
			g, eng := perSlotFixture(t)
			tc.build(t, g, eng)

			if got, ok := g.OutDegree("a"); !ok || got != tc.wantDeg {
				t.Fatalf("fixture not as claimed: OutDegree(a) = (%d, %v), want (%d, true)", got, ok, tc.wantDeg)
			}
			for _, ty := range []struct {
				relType string
				want    int
			}{{"K", tc.wantK}, {"M", tc.wantM}} {
				lid, known := g.Registry().Lookup(ty.relType)
				if !known {
					if ty.want != 0 {
						t.Fatalf("relationship type %s was never interned but %d were expected",
							ty.relType, ty.want)
					}
					continue
				}
				if got, ok := g.OutDegreeByType("a", lid); !ok || got != ty.want {
					t.Errorf("OutDegreeByType(a, %s) = (%d, %v), want (%d, true)",
						ty.relType, got, ok, ty.want)
				}
				// A cap above the true count must give the true count.
				if got, ok := g.OutDegreeByTypeBounded("a", lid, tc.wantDeg+1); !ok || got != ty.want {
					t.Errorf("OutDegreeByTypeBounded(a, %s, %d) = (%d, %v), want (%d, true)",
						ty.relType, tc.wantDeg+1, got, ok, ty.want)
				}
				// The pair's DERIVED set is the union over its slots, so it reports
				// the type exactly when some slot carries it. Making the storage
				// per-slot must not change that: EdgeLabels/HasEdgeLabel are a
				// per-pair question and keep their per-pair answer.
				if got := g.HasEdgeLabel("a", tc.far, ty.relType); got != (ty.want > 0) {
					t.Errorf("HasEdgeLabel(a, %s, %s) = %v, want %v",
						tc.far, ty.relType, got, ty.want > 0)
				}
				// A cap below it must give the cap.
				if ty.want > 1 {
					if got, ok := g.OutDegreeByTypeBounded("a", lid, ty.want-1); !ok || got != ty.want-1 {
						t.Errorf("OutDegreeByTypeBounded(a, %s, %d) = (%d, %v), want (%d, true)",
							ty.relType, ty.want-1, got, ok, ty.want-1)
					}
				}
			}
			_ = eng
		})
	}
}

// TestPerSlotRelType_RemoveEdgeLabelClearsEverySlot is the inverse direction:
// SetEdgeLabel types every free slot of the pair, so RemoveEdgeLabel must clear
// every one of them. Clearing only the first left a parallel sibling still
// reporting the type — the same per-pair-versus-per-slot mismatch, in the write
// path rather than the read path.
func TestPerSlotRelType_RemoveEdgeLabelClearsEverySlot(t *testing.T) {
	g, eng := perSlotFixture(t)
	addEdges(t, g, "a", "b", 3)
	g.SetEdgeLabel("a", "b", "K")

	lid, known := g.Registry().Lookup("K")
	if !known {
		t.Fatal("relationship type K was not interned")
	}
	if got, _ := g.OutDegreeByType("a", lid); got != 3 {
		t.Fatalf("precondition: OutDegreeByType(a, K) = %d, want 3", got)
	}

	g.RemoveEdgeLabel("a", "b", "K")

	if got, _ := g.OutDegreeByType("a", lid); got != 0 {
		t.Errorf("after RemoveEdgeLabel: OutDegreeByType(a, K) = %d, want 0", got)
	}
	if g.HasEdgeLabel("a", "b", "K") {
		t.Error("after RemoveEdgeLabel: HasEdgeLabel(a, b, K) = true, want false")
	}
	if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-[:K]->() }"); got[0] != enc(0) {
		t.Errorf("after RemoveEdgeLabel: COUNT { (a)-[:K]->() } = %v, want %s", got, enc(0))
	}
	if got := degreeRun(t, eng, "MATCH (a:P {id: 0})-[r:K]->() RETURN count(r)"); got[0] != enc(0) {
		t.Errorf("after RemoveEdgeLabel: MATCH count(r) = %v, want %s", got, enc(0))
	}
	// The edges themselves survive, untyped.
	if got := degreeRun(t, eng, "MATCH (a:P {id: 0}) RETURN COUNT { (a)-->() }"); got[0] != enc(3) {
		t.Errorf("after RemoveEdgeLabel: COUNT { (a)-->() } = %v, want %s", got, enc(3))
	}
}

// enc renders an integer the way degreeRun renders a single scalar column.
func enc(n int) string { return fmt.Sprintf("%d\x1f", n) }
