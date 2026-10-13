package cypher_test

// prop_remove_lost_update_config_test.go — rmp #2943 on the relationship
// pattern shapes prop_remove_lost_update_test.go does not reach: a directed
// and an undirected pattern binding the one stored relationship.

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// lostRmConfigEngine is lostRmEngine on an explicit adjacency configuration.
func lostRmConfigEngine(t *testing.T, cfg adjlist.Config, walBacked bool, setup ...string) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](cfg)
	var eng *cypher.Engine
	if walBacked {
		wr, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		t.Cleanup(func() { _ = wr.Close() })
		eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		}))
	} else {
		eng = cypher.NewEngine(g)
	}
	for _, q := range setup {
		if err := lostRmAutocommit(eng, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	return eng
}

// TestPropRemove_RelationshipShapesOnEveryConfiguration runs the relationship
// lost-update shapes through a directed and an undirected pattern,
// under both endings of T: after T's rollback P's committed write must be the
// final state, and after T's commit P and T may not both have committed.
func TestPropRemove_RelationshipShapesOnEveryConfiguration(t *testing.T) {
	configs := []struct {
		name string
		cfg  adjlist.Config
		tRel string // pattern T binds r through
		pRel string // pattern P binds r through
	}{
		{"directed-pattern", adjlist.Config{}, `(:A)-[r:R]->(:B)`, `(:A)-[r:R]->(:B)`},
		{"undirected-pattern", adjlist.Config{}, `(:A)-[r:R]-(:B)`, `(:A)-[r:R]-(:B)`},
	}
	type shape struct {
		name, setup, tOp, pOp, check, want string
	}
	shapes := func(tRel, pRel string) []shape {
		return []shape{
			{"remove-after-pending-remove-of-only-property", `CREATE (:A)-[:R {s: 'a'}]->(:B)`,
				`MATCH ` + tRel + ` REMOVE r.s`, `MATCH ` + pRel + ` REMOVE r.s`,
				`MATCH ` + tRel + ` RETURN r.s IS NULL AS v`, "true"},
			{"remove-after-pending-remove", `CREATE (:A)-[:R {k: 1, s: 'a'}]->(:B)`,
				`MATCH ` + tRel + ` REMOVE r.s`, `MATCH ` + pRel + ` REMOVE r.s`,
				`MATCH ` + tRel + ` RETURN r.s IS NULL AS v`, "true"},
			{"set-same-value-after-pending-remove", `CREATE (:A)-[:R {s: 'a'}]->(:B)`,
				`MATCH ` + tRel + ` REMOVE r.s`, `MATCH ` + pRel + ` SET r.s = 'a'`,
				`MATCH ` + tRel + ` RETURN r.s AS v`, `"a"`},
			{"replace-empty-after-pending-remove-of-only-property", `CREATE (:A)-[:R {s: 'a'}]->(:B)`,
				`MATCH ` + tRel + ` REMOVE r.s`, `MATCH ` + pRel + ` SET r = {}`,
				`MATCH ` + tRel + ` RETURN size(keys(r)) AS v`, "0"},
		}
	}
	for _, c := range configs {
		for _, walBacked := range []bool{false, true} {
			for _, s := range shapes(c.tRel, c.pRel) {
				for _, commitT := range []bool{false, true} {
					name := fmt.Sprintf("%s/wal=%v/%s/commitT=%v", c.name, walBacked, s.name, commitT)
					t.Run(name, func(t *testing.T) {
						eng := lostRmConfigEngine(t, c.cfg, walBacked, s.setup)
						if got := lostRmScalar(t, eng, `MATCH `+c.pRel+` RETURN count(r) AS v`); got != "1" {
							t.Fatalf("P's pattern binds %s relationships, want 1", got)
						}
						pErr, tEnd := lostRmRun(t, eng, &lostRmShape{tOp: s.tOp, pOp: s.pOp}, commitT)
						if pErr != nil && !errors.Is(pErr, mvcc.ErrSerializationConflict) {
							t.Fatalf("P refused with a non-retryable error: %v", pErr)
						}
						if tEnd != nil && !errors.Is(tEnd, mvcc.ErrSerializationConflict) {
							t.Fatalf("T end failed with a non-retryable error: %v", tEnd)
						}
						if commitT {
							if pErr == nil && tEnd == nil {
								t.Fatalf("T %q and P %q wrote the same relationship concurrently and BOTH committed", s.tOp, s.pOp)
							}
							return
						}
						if tEnd != nil {
							t.Fatalf("T rollback: %v", tEnd)
						}
						if pErr == nil {
							if got := lostRmScalar(t, eng, s.check); got != s.want {
								t.Fatalf("P %q committed, but after T's rollback %q = %s, want %s: P's committed write was lost",
									s.pOp, s.check, got, s.want)
							}
						}
					})
				}
			}
		}
	}
}
