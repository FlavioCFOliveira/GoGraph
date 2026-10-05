package cypher_test

// autocommit_failure_aborts_2976_test.go — regression gate for rmp #2976.
//
// A FAILED autocommit statement ended its versioned transaction as a COMMIT: the
// physical undo replay restored the values, and the shared write bracket then
// PUBLISHED the transaction's record. A published record is a committed write
// after the start of every transaction that began earlier, so such a transaction
// writing the same object afterwards was refused by first-updater-wins
// ("serialization conflict in node properties") although nothing it could see
// had changed; and the substrate counted the failure as a commit. A failed
// statement must ABORT: no committed version, Aborts +1, Commits unchanged.
//
// Driven on both engine wirings (in-memory and WAL-backed) and through three
// failure exits: an error raised while the statement drains, a refusal at
// commit time (NOT NULL), and a panic converted by the recover boundary.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestAutocommitFailureAborts_2976(t *testing.T) {
	engines := []struct {
		name string
		mk   func(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64])
	}{
		{
			name: "in-memory",
			mk: func(*testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
				g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
				return cypher.NewEngine(g), g
			},
		},
		{
			name: "WAL",
			mk: func(t *testing.T) (*cypher.Engine, *lpg.Graph[string, float64]) {
				e, g, _ := newWALStoreEngine(t)
				return e, g
			},
		},
	}
	failures := []struct {
		name  string
		ddl   string // optional schema statement run first
		query string // the failing autocommit statement; it writes n.x first
		quiet bool   // silence the recover boundary's panic log
	}{
		{
			name:  "drain error",
			query: "MATCH (n:Item {name: 'n'}) SET n.x = 1 WITH n RETURN n.x / 0",
		},
		{
			name:  "NOT NULL refusal at commit",
			ddl:   "CREATE CONSTRAINT item_x_nn FOR (n:Item) REQUIRE n.x IS NOT NULL",
			query: "MATCH (n:Item {name: 'n'}) SET n.x = 1 CREATE (:Item {name: 'm'})",
		},
		{
			name:  "panic",
			query: "MATCH (n:Item {name: 'n'}) SET n.x = 1 WITH n RETURN boom()",
			quiet: true,
		},
	}
	for _, en := range engines {
		for _, f := range failures {
			t.Run(en.name+"/"+f.name, func(t *testing.T) {
				if f.quiet {
					quietLogs(t)
				}
				ctx := context.Background()
				e, g := en.mk(t)
				if f.ddl != "" {
					res, err := e.Run(ctx, f.ddl, nil)
					if err != nil {
						t.Fatalf("schema: %v", err)
					}
					_ = res.Close() // DDL result carries nothing to read
				}
				if _, err := e.RunAny(ctx, "CREATE (:Item {name: 'n', x: 0})", nil); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				t2, err := e.BeginTx(ctx) // the OLDER snapshot
				if err != nil {
					t.Fatalf("BeginTx T2: %v", err)
				}
				before := g.MVCCStats().Write
				// A drain error travels on the Result, not on RunAny's own return.
				if res, err := e.RunAny(ctx, f.query, nil); err == nil {
					rerr := res.Err()
					_ = res.Close() // the statement's outcome is rerr; Close repeats it
					if rerr == nil {
						t.Fatalf("RunAny(%q) succeeded; the statement must fail", f.query)
					}
				}
				after := g.MVCCStats().Write
				if after.Commits != before.Commits {
					t.Errorf("failed statement counted as a commit: Commits %d -> %d", before.Commits, after.Commits)
				}
				if after.Aborts != before.Aborts+1 {
					t.Errorf("failed statement not counted as an abort: Aborts %d -> %d, want +1", before.Aborts, after.Aborts)
				}
				g.ReclaimNow()
				if _, err := t2.Exec("MATCH (n:Item {name: 'n'}) SET n.x = 2", nil); err != nil {
					_ = t2.Rollback() // the failure under test is already reported
					t.Fatalf("T2 write after the failed statement: %v", err)
				}
				if err := t2.Commit(); err != nil {
					t.Fatalf("T2 Commit: %v", err)
				}
				res, err := e.RunAny(ctx, "MATCH (n:Item) RETURN collect(n.name + '=' + toString(n.x)) AS v", nil)
				if err != nil {
					t.Fatalf("read back: %v", err)
				}
				defer func() { _ = res.Close() }() // read-only use; Close error carries nothing here
				if !res.Next() {
					t.Fatalf("read back: no row (err %v)", res.Err())
				}
				if got := res.ValueAt(0).String(); got != `["n=2"]` {
					t.Fatalf("state after T2 committed: %s, want [\"n=2\"]", got)
				}
			})
		}
	}
}
