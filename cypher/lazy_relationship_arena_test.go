package cypher

// Tests for the pooled-RowContext relationship arena (rmp #3104): the lazy
// relationship value the non-escaping evaluation path binds is drawn from a
// reusable per-unit arena instead of being allocated per row. Each test below
// guards one way an arena can go wrong — growing the pooled unit past one cache
// line, aliasing two bindings of one row, leaking a previous row's fields into a
// rebound slot, or answering differently from the eager materialiser.

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestPooledRowCtxFitsOneCacheLine pins the size constraint that made the #2388
// arena variant a loss: a pooledRowCtx past 64 bytes cost a reproducible ~1.7%
// on an aggregation binding no relationship. The relationship arena is admissible
// only while the unit stays at one cache line.
func TestPooledRowCtxFitsOneCacheLine(t *testing.T) {
	if got := unsafe.Sizeof(pooledRowCtx{}); got > 64 {
		t.Fatalf("pooledRowCtx is %d bytes, want <= 64: the relationship arena must not grow the pooled unit past one cache line", got)
	}
}

// TestBorrowLazyRelDistinctSlotsAndFullRebind pins the two arena invariants: two
// borrows within one row return distinct slots, and a slot reused for a later
// row is rebound field-for-field to what the constructor builds, with nothing of
// the previous row left behind.
func TestBorrowLazyRelDistinctSlotsAndFullRebind(t *testing.T) {
	p := acquireRowCtx(2)
	defer releaseRowCtx(p)

	res := lazyRelResolver{}
	srcA := expr.RelSource{StartKey: "a", EndKey: "b", Handle: 7, ByHandle: true}
	srcB := expr.RelSource{StartKey: "b", EndKey: "c"}
	v1 := p.borrowLazyRel(7, 10, 11, "R1", srcA, res)
	v2 := p.borrowLazyRel(8, 20, 21, "R2", srcB, res)
	if v1 == v2 {
		t.Fatal("two bindings of one row share one arena slot")
	}
	if want := expr.NewLazyRelationshipValue(7, 10, 11, "R1", srcA, res); !reflect.DeepEqual(*v1, *want) {
		t.Fatalf("first slot = %+v, want %+v", *v1, *want)
	}
	if want := expr.NewLazyRelationshipValue(8, 20, 21, "R2", srcB, res); !reflect.DeepEqual(*v2, *want) {
		t.Fatalf("second slot = %+v, want %+v", *v2, *want)
	}

	// The next row: the cursor resets and the first slot is rebound with values
	// that differ in every field, including a nil resolver and a zero source.
	p.used, p.relUsed = 0, 0
	v3 := p.borrowLazyRel(9, 30, 31, "R3", expr.RelSource{}, nil)
	// v1 may point into a backing array the second borrow's append outgrew; it
	// stays a valid, unmodified value (checked above, after that append), but the
	// slot reused from now on is the arena's current first element.
	if v3 != &p.rels[0] {
		t.Fatal("the cursor reset did not reuse the first slot; the arena is not being reused")
	}
	if want := expr.NewLazyRelationshipValue(9, 30, 31, "R3", expr.RelSource{}, nil); !reflect.DeepEqual(*v3, *want) {
		t.Fatalf("rebound slot = %+v, want %+v: a field of the previous row survived", *v3, *want)
	}
	if got := v3.Property("w"); !expr.IsNull(got) {
		t.Fatalf("rebound slot with a nil resolver read %v, want null", got)
	}
}

// TestBuildRelationshipValueFromRowUsesTheArena is the reachability oracle: with
// an arena, the production materialiser returns a pointer INTO the arena for a
// scalar-only use, and that value answers every property exactly as the
// arena-less (freshly allocated) value does. Without it, every Cypher-level
// equivalence below could pass while the arena is never engaged.
func TestBuildRelationshipValueFromRowUsesTheArena(t *testing.T) {
	for _, route := range []struct {
		name  string
		build func(*testing.T) *lpg.Graph[string, float64]
	}{
		{"per_pair", buildLazyRelPerPairGraph},
		{"by_handle", buildLazyRelByHandleGraph},
	} {
		t.Run(route.name, func(t *testing.T) {
			g := route.build(t)
			c := lazyRelEdgeCoords(t, g)
			uses := relUsesFromMemo(t, g, `MATCH (a:P)-[r:R]->(b:P) WHERE r.p_int < 0 RETURN count(*) AS n`, "r")
			if len(uses) == 0 {
				t.Fatal("no memoised analysis binds r, so this shape proves nothing")
			}
			row := exec.Row{expr.IntegerValue(c.srcID), expr.IntegerValue(c.handle), expr.IntegerValue(c.dstID)}
			meta := edgeVarInfo{edgeType: "R", acceptedTypes: []string{"R"}, srcCol: 0, edgeCol: 1, dstCol: 2}
			p := acquireRowCtx(3)
			defer releaseRowCtx(p)
			for _, use := range uses {
				p.used, p.relUsed = 0, 0
				got, ok := buildRelationshipValueFromRow(row, &meta, c.view, nil, use, p)
				if !ok {
					t.Fatal("buildRelationshipValueFromRow refused the synthesised row")
				}
				lazy, isLazy := got.(*expr.LazyRelationshipValue)
				if !isLazy {
					t.Fatalf("expected a lazy relationship, got %T", got)
				}
				if len(p.rels) == 0 || lazy != &p.rels[0] {
					t.Fatal("the lazy relationship was not drawn from the arena")
				}
				fresh, _ := buildRelationshipValueFromRow(row, &meta, c.view, nil, use, nil)
				fl := fresh.(*expr.LazyRelationshipValue)
				if fl == lazy {
					t.Fatal("the arena-less call returned the arena slot")
				}
				for _, k := range []string{"p_str", "p_int", "p_bigint", "p_float", "p_bool", "p_list", "p_bytes", "absent"} {
					if a, b := lazy.Property(k), fl.Property(k); !reflect.DeepEqual(a, b) {
						t.Fatalf("r.%s: arena value %v, fresh value %v", k, a, b)
					}
				}
				if lazy.ID() != fl.ID() || lazy.StartID() != fl.StartID() || lazy.EndID() != fl.EndID() || lazy.RelType() != fl.RelType() {
					t.Fatalf("identity differs: arena %+v, fresh %+v", *lazy, *fl)
				}
			}
		})
	}
}

// arenaEquivQueries pairs each pooled-path query (scalar-only relationship use:
// the WHERE predicate and the scalar projection both run through evalRowPooled)
// with an eager twin that reads the same values through properties(), which
// forces whole materialisation and never touches the arena. Two relationship
// variables in one row is the shape an aliasing arena would corrupt.
var arenaEquivQueries = [][2]string{
	{
		`MATCH (a)-[r1]->(b)-[r2]->(c) WHERE r1.w < r2.w RETURN id(r1) AS i, id(r2) AS j`,
		`MATCH (a)-[r1]->(b)-[r2]->(c) WHERE properties(r1).w < properties(r2).w RETURN id(r1) AS i, id(r2) AS j`,
	},
	{
		`MATCH (a)-[r1]->(b)-[r2]->(c) RETURN r1.w AS x, r2.w AS y, r1.w - r2.w AS d`,
		`MATCH (a)-[r1]->(b)-[r2]->(c) RETURN properties(r1).w AS x, properties(r2).w AS y, properties(r1).w - properties(r2).w AS d`,
	},
	{
		`MATCH (a)-[r:T1|T2]->(b) WHERE r.w >= 2 RETURN r.w AS w, r.q AS q`,
		`MATCH (a)-[r:T1|T2]->(b) WHERE properties(r).w >= 2 RETURN properties(r).w AS w, properties(r).q AS q`,
	},
	{
		`MATCH (a)-[r]-(b) WHERE r.w IS NOT NULL RETURN r.w AS w`,
		`MATCH (a)-[r]-(b) WHERE properties(r).w IS NOT NULL RETURN properties(r).w AS w`,
	},
}

// buildArenaEquivGraph seeds a two-hop graph through Cypher (by-handle route)
// with multi-type parallel pairs, a zero-property sibling, and a deleted edge;
// goAPI adds a Go-API (per-pair route) chain beside it.
func buildArenaEquivGraph(t *testing.T, goAPI bool) (*lpg.Graph[string, float64], *Engine) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	e := NewEngine(g)
	runWrite(t, e, `CREATE (a:N {k:'a'}), (b:N {k:'b'}), (c:N {k:'c'}), (d:N {k:'d'})`)
	runWrite(t, e, `MATCH (a:N {k:'a'}), (b:N {k:'b'}) CREATE (a)-[:T1 {w:1, q:'x'}]->(b), (a)-[:T2 {w:5}]->(b), (a)-[:T1]->(b)`)
	runWrite(t, e, `MATCH (b:N {k:'b'}), (c:N {k:'c'}) CREATE (b)-[:T2 {w:3, q:'y'}]->(c), (b)-[:T1 {w:4}]->(c), (b)-[:T1 {w:9}]->(c)`)
	runWrite(t, e, `MATCH (c:N {k:'c'}), (d:N {k:'d'}) CREATE (c)-[:T1 {w:2}]->(d), (c)-[:T2 {w:0}]->(d)`)
	runWrite(t, e, `MATCH (:N {k:'b'})-[r:T1 {w:9}]->(:N {k:'c'}) DELETE r`)
	if goAPI {
		for _, k := range []string{"x", "y", "z"} {
			if err := g.AddNode(k); err != nil {
				t.Fatal(err)
			}
		}
		for _, ed := range []struct {
			s, d string
			w    int64
		}{{"x", "y", 6}, {"y", "z", 7}, {"z", "x", 1}} {
			if err := g.AddEdge(ed.s, ed.d, 1); err != nil {
				t.Fatal(err)
			}
			if err := g.SetEdgeLabel(ed.s, ed.d, "T1"); err != nil {
				t.Fatal(err)
			}
			if err := g.SetEdgeProperty(ed.s, ed.d, "w", lpg.Int64Value(ed.w)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g, e
}

func arenaSorted(rows []string) []string {
	out := append([]string(nil), rows...)
	sort.Strings(out)
	return out
}

// TestLazyRelationshipArenaMatchesEagerEndToEnd runs each pooled-path query and
// its eager twin over Cypher-created and Go-API edges and asserts identical bags
// of rows. A non-empty answer is required, so the comparison is never vacuous.
func TestLazyRelationshipArenaMatchesEagerEndToEnd(t *testing.T) {
	for _, goAPI := range []bool{false, true} {
		t.Run(fmt.Sprintf("goAPI=%v", goAPI), func(t *testing.T) {
			_, e := buildArenaEquivGraph(t, goAPI)
			for _, qp := range arenaEquivQueries {
				lazy := arenaSorted(drainRows(t, e, qp[0]))
				eager := arenaSorted(drainRows(t, e, qp[1]))
				if len(lazy) == 0 {
					t.Fatalf("%s returned no rows, so nothing was compared", qp[0])
				}
				if !reflect.DeepEqual(lazy, eager) {
					t.Fatalf("pooled path %q\n  got  %v\n  eager %v", qp[0], lazy, eager)
				}
			}
		})
	}
}

// TestLazyRelationshipArenaAtPinnedSnapshot pins the arena under a read
// transaction's pinned snapshot: writes committed after BEGIN (a property
// change, a new edge, a delete) must be invisible to both the pooled path and
// its eager twin, and the two must agree with the answer taken before the
// writes.
func TestLazyRelationshipArenaAtPinnedSnapshot(t *testing.T) {
	_, e := buildArenaEquivGraph(t, false)
	before := make([][]string, len(arenaEquivQueries))
	for i, qp := range arenaEquivQueries {
		before[i] = arenaSorted(drainRows(t, e, qp[1]))
	}
	tx, err := e.BeginReadTx(context.Background())
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	runWrite(t, e, `MATCH ()-[r:T2 {w:3}]->() SET r.w = 100`)
	runWrite(t, e, `MATCH (a:N {k:'a'}), (d:N {k:'d'}) CREATE (d)-[:T1 {w:8}]->(a)`)
	runWrite(t, e, `MATCH ()-[r:T1 {w:4}]->() DELETE r`)
	for i, qp := range arenaEquivQueries {
		for _, q := range qp {
			res, err := tx.Exec(q, nil)
			if err != nil {
				t.Fatalf("Exec(%q): %v", q, err)
			}
			cols := res.Columns()
			var rows []string
			for res.Next() {
				rec := res.Record()
				var sb []byte
				for j, c := range cols {
					if j > 0 {
						sb = append(sb, '|')
					}
					sb = append(sb, fmt.Sprintf("%s=%v", c, rec[c])...)
				}
				rows = append(rows, string(sb))
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Err(%q): %v", q, err)
			}
			_ = res.Close()
			if got := arenaSorted(rows); !reflect.DeepEqual(got, before[i]) {
				t.Fatalf("at the pinned snapshot %q\n  got  %v\n  want %v", q, got, before[i])
			}
		}
	}
	// Non-vacuity: the writes were visible to a fresh statement.
	after := arenaSorted(drainRows(t, e, arenaEquivQueries[2][1]))
	if reflect.DeepEqual(after, before[2]) {
		t.Fatal("the post-BEGIN writes changed nothing, so the snapshot assertion is vacuous")
	}
}
