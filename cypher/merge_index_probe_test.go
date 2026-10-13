package cypher

// Regression and differential battery for rmp #2812: the node MERGE match phase
// probes a property index instead of walking the label posting list, and must
// return exactly the matches the walk returns.
//
// Every differential case runs on two engines seeded identically: the PROBE arm
// (default options) and the WALK arm (EngineOptions.DisableIndexSeek, which turns
// the probe off). Both the statement's own rows and the whole graph afterwards
// must be identical, and the probe arm must actually have probed — or, for the
// cases built to force a decline, must have declined — which the build counters
// mergeIndexProbeCount and mergeIndexProbeDeclineCount assert. Explain is not
// used: it plans against a committed snapshot and cannot see a run-time decline.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergeProbePopulation is large enough that a walk and a probe differ by orders
// of magnitude in the nodes examined, so a probe that silently fell back would be
// visible in the benchmark, while the counters make it visible here.
const mergeProbePopulation = 512

// newMergeProbeEngine seeds mergeProbePopulation :L nodes, each with a unique tag
// t<i> (the identity the differential compares by), a string key s = "s<i%128>"
// (so keys repeat four times, and MERGE must bind every match), and a numeric key
// p = i%128 stored as an integer on even i and as a float on odd i (so the index
// holds both kinds under one key). Every eighth node also carries :M. It then
// creates a hash index on (:L, s) and a btree index on (:L, p), whose numeric
// companion serves the numeric probe.
func newMergeProbeEngine(t *testing.T, disableIndexSeek bool) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < mergeProbePopulation; i++ {
		n := fmt.Sprintf("n%d", i)
		mustNoErr(t, g.AddNode(n))
		mustNoErr(t, g.SetNodeLabel(n, "L"))
		if i%8 == 0 {
			mustNoErr(t, g.SetNodeLabel(n, "M"))
		}
		mustNoErr(t, g.SetNodeProperty(n, "tag", lpg.StringValue(fmt.Sprintf("t%d", i))))
		mustNoErr(t, g.SetNodeProperty(n, "s", lpg.StringValue(fmt.Sprintf("s%d", i%128))))
		p := lpg.Int64Value(int64(i % 128))
		if i%2 == 1 {
			p = lpg.Float64Value(float64(i % 128))
		}
		mustNoErr(t, g.SetNodeProperty(n, "p", p))
	}
	// Edge cases for the numeric key: an integer above 2^53, whose float64 key
	// collides with 2^53, and a negative zero.
	extra := []struct {
		name string
		p    lpg.PropertyValue
	}{
		{"big", lpg.Int64Value(1<<53 + 1)},
		{"negzero", lpg.Float64Value(negativeZero())},
		{"strnum", lpg.StringValue("5")},
	}
	for _, x := range extra {
		mustNoErr(t, g.AddNode(x.name))
		mustNoErr(t, g.SetNodeLabel(x.name, "L"))
		mustNoErr(t, g.SetNodeProperty(x.name, "tag", lpg.StringValue(x.name)))
		mustNoErr(t, g.SetNodeProperty(x.name, "p", x.p))
		if x.name == "strnum" {
			mustNoErr(t, g.SetNodeProperty(x.name, "s", x.p))
		}
	}
	eng := NewEngineWithOptions(g, EngineOptions{DisableIndexSeek: disableIndexSeek})
	for _, ddl := range []string{
		"CREATE INDEX FOR (n:L) ON (n.s)",
		"CREATE INDEX FOR (n:L) ON (n.p) OPTIONS {indexType: 'btree'}",
	} {
		if _, err := eng.Run(context.Background(), ddl, nil); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	return eng
}

func negativeZero() float64 {
	z := 0.0
	return -z
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mergeProbeRows renders every row of res as one string, in result order.
func mergeProbeRows(t *testing.T, q string, res *Result, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprintf("%s=%v", c, rec[c])
		}
		out = append(out, strings.Join(parts, ","))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iterate %s: %v", q, err)
	}
	return out
}

// mergeProbeGraph renders the whole graph as a sorted list of (tag, labels, s, p)
// tuples, with the value kind of p, so a duplicate node, a lost write or a kind
// change all show up as a difference.
func mergeProbeGraph(t *testing.T, eng *Engine) []string {
	t.Helper()
	const q = `MATCH (n) RETURN n.tag AS tag, labels(n) AS l, n.s AS s, n.p AS p, n.x AS x`
	res, err := eng.Run(context.Background(), q, nil)
	rows := mergeProbeRows(t, q, res, err)
	sort.Strings(rows)
	return rows
}

// mergeProbeCase is one statement sequence run on both arms. stmts run in one
// explicit transaction when inTx is set, and as autocommit statements otherwise.
type mergeProbeCase struct {
	name  string
	stmts []string
	inTx  bool
	// wantProbe and wantDecline state what the probe arm's counters must show:
	// at least one answered probe, and at least one decline.
	wantProbe   bool
	wantDecline bool
}

func runMergeProbeArm(t *testing.T, eng *Engine, c mergeProbeCase) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	if c.inTx {
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		for _, q := range c.stmts {
			res, err := tx.ExecAny(q, nil)
			out = append(out, mergeProbeRows(t, q, res, err)...)
		}
		mustNoErr(t, tx.Commit())
		return out
	}
	for _, q := range c.stmts {
		res, err := eng.RunAny(ctx, q, nil)
		out = append(out, mergeProbeRows(t, q, res, err)...)
	}
	return out
}

// TestMergeIndexProbe_Differential is the acceptance differential: for every
// case, the probe arm and the walk arm return the same rows in the same order and
// leave the same graph, and the probe arm used — or declined — the index as the
// case requires.
func TestMergeIndexProbe_Differential(t *testing.T) {
	const ret = ` RETURN n.tag AS tag, n.s AS s, n.p AS p`
	cases := []mergeProbeCase{
		{name: "string/match-all-duplicates", stmts: []string{`MERGE (n:L {s: 's7'})` + ret}, wantProbe: true},
		{name: "string/no-match-creates", stmts: []string{`MERGE (n:L {s: 'absent'})` + ret}, wantProbe: true},
		{name: "string/multi-label", stmts: []string{`MERGE (n:L:M {s: 's8'})` + ret}, wantProbe: true},
		{name: "string/second-label-drives", stmts: []string{`MERGE (n:M:L {s: 's16'})` + ret}, wantProbe: true},
		{name: "string/every-property-rechecked", stmts: []string{`MERGE (n:L {s: 's9', p: 9})` + ret}, wantProbe: true},
		{name: "string/partial-property-mismatch-creates", stmts: []string{`MERGE (n:L {s: 's9', p: 10})` + ret}, wantProbe: true},
		// A btree CREATE INDEX also builds a string btree, and a hash CREATE INDEX a
		// numeric companion, so a key of the "other" kind is still probed.
		{name: "string/vs-stored-number", stmts: []string{`MERGE (n:L {p: '5'})` + ret}, wantProbe: true},
		{name: "numeric/int-matches-int-and-float", stmts: []string{`MERGE (n:L {p: 3})` + ret}, wantProbe: true},
		{name: "numeric/float-matches-int-and-float", stmts: []string{`MERGE (n:L {p: 4.0})` + ret}, wantProbe: true},
		{name: "numeric/non-integral-float-creates", stmts: []string{`MERGE (n:L {p: 4.5})` + ret}, wantProbe: true},
		{name: "numeric/above-2^53", stmts: []string{`MERGE (n:L {p: 9007199254740992})` + ret}, wantProbe: true},
		{name: "numeric/above-2^53-exact", stmts: []string{`MERGE (n:L {p: 9007199254740993})` + ret}, wantProbe: true},
		{name: "numeric/zero-vs-negative-zero", stmts: []string{`MERGE (n:L {p: 0})` + ret}, wantProbe: true},
		{name: "numeric/vs-stored-string", stmts: []string{`MERGE (n:L {s: 5})` + ret}, wantProbe: true},
		{name: "unindexed-kind/bool", stmts: []string{`MERGE (n:L {p: true})` + ret}, wantDecline: true},
		{name: "on-match-set", stmts: []string{`MERGE (n:L {s: 's11'}) ON MATCH SET n.x = 1` + ret}, wantProbe: true},
		{
			name:  "unwind/row-aware-literals",
			stmts: []string{`UNWIND ['s1', 's2', 3, 3.0] AS v MERGE (n:L {s: v})` + ret},
			// 's1' and 's2' probe; 3 is a string index asked for a number.
			wantProbe: true, wantDecline: true,
		},
		{
			// The self-write case that would be a DUPLICATE: the first row creates
			// the node, the index does not hold it until commit, and the second and
			// third rows must find it. After the first create the probe must decline.
			name:      "unwind/in-statement-self-write/string",
			stmts:     []string{`UNWIND ['fresh', 'fresh', 'fresh'] AS v MERGE (n:L {s: v})` + ret},
			wantProbe: true, wantDecline: true,
		},
		{
			name:      "unwind/in-statement-self-write/cross-type",
			stmts:     []string{`UNWIND [1000, 1000.0, 1000] AS v MERGE (n:L {p: v})` + ret},
			wantProbe: true, wantDecline: true,
		},
		{
			name: "tx/self-write-across-statements",
			stmts: []string{
				`MERGE (n:L {s: 'txnew'})` + ret,
				`MERGE (n:L {s: 'txnew'})` + ret,
			},
			inTx: true, wantProbe: true, wantDecline: true,
		},
		{
			// An uncommitted SET moves a key: the index still files the node under
			// the OLD value and not under the new one.
			name: "tx/moved-key",
			stmts: []string{
				`MATCH (n:L {tag: 't20'}) SET n.s = 'moved'`,
				`MERGE (n:L {s: 'moved'})` + ret,
				`MERGE (n:L {s: 's20'})` + ret,
			},
			inTx: true, wantDecline: true,
		},
		{
			name: "tx/removed-label",
			stmts: []string{
				`MATCH (n:L {tag: 't21'}) REMOVE n:L`,
				`MERGE (n:L {s: 's21'})` + ret,
			},
			inTx: true, wantDecline: true,
		},
		{
			name: "tx/deleted-node",
			stmts: []string{
				`MATCH (n:L {tag: 't22'}) DETACH DELETE n`,
				`MERGE (n:L {s: 's22'})` + ret,
			},
			inTx: true, wantDecline: true,
		},
		{
			// A write to an unrelated coordinate must not cost the probe.
			name: "tx/unrelated-write-keeps-probe",
			stmts: []string{
				`MATCH (n:L {tag: 't23'}) SET n.x = 5`,
				`MERGE (n:L {s: 's23'})` + ret,
			},
			inTx: true, wantProbe: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probeEng := newMergeProbeEngine(t, false)
			walkEng := newMergeProbeEngine(t, true)

			p0, d0 := mergeIndexProbeCount.Load(), mergeIndexProbeDeclineCount.Load()
			walkRows := runMergeProbeArm(t, walkEng, c)
			p1, d1 := mergeIndexProbeCount.Load(), mergeIndexProbeDeclineCount.Load()
			if p1 != p0 || d1 != d0 {
				t.Fatalf("walk arm consulted the probe (answered %d, declined %d): the control is not a walk", p1-p0, d1-d0)
			}
			probeRows := runMergeProbeArm(t, probeEng, c)
			probed, declined := mergeIndexProbeCount.Load()-p1, mergeIndexProbeDeclineCount.Load()-d1

			if strings.Join(probeRows, "\n") != strings.Join(walkRows, "\n") {
				t.Fatalf("statement rows differ\nprobe:\n  %s\nwalk:\n  %s",
					strings.Join(probeRows, "\n  "), strings.Join(walkRows, "\n  "))
			}
			pg, wg := mergeProbeGraph(t, probeEng), mergeProbeGraph(t, walkEng)
			if strings.Join(pg, "\n") != strings.Join(wg, "\n") {
				t.Fatalf("graphs differ after the statements (%d vs %d nodes)", len(pg), len(wg))
			}
			if c.wantProbe && probed == 0 {
				t.Fatalf("the probe never answered (declined %d): the case did not exercise the index path", declined)
			}
			if !c.wantProbe && probed != 0 {
				t.Fatalf("the probe answered %d time(s) where it must decline", probed)
			}
			if c.wantDecline && declined == 0 {
				t.Fatalf("the probe never declined (answered %d): the case did not exercise the fallback", probed)
			}
		})
	}
}

// TestMergeIndexProbe_NoDuplicateOnSelfWrite pins the ACID-relevant outcome of
// the self-write cases directly, independently of the walk arm: a key written
// earlier in the same statement or transaction is merged, never duplicated.
func TestMergeIndexProbe_NoDuplicateOnSelfWrite(t *testing.T) {
	ctx := context.Background()
	eng := newMergeProbeEngine(t, false)
	count := func(q string) int64 {
		res, err := eng.Run(ctx, q, nil)
		rows := mergeProbeRows(t, q, res, err)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", q, len(rows))
		}
		var n int64
		if _, err := fmt.Sscanf(rows[0], "c=%d", &n); err != nil {
			t.Fatalf("%s: %q: %v", q, rows[0], err)
		}
		return n
	}
	drain := func(q string) {
		res, err := eng.RunAny(ctx, q, nil)
		mergeProbeRows(t, q, res, err)
	}
	drain(`UNWIND range(1, 50) AS i UNWIND ['dup-a', 'dup-b'] AS v MERGE (:L {s: v})`)
	if got := count(`MATCH (n:L) WHERE n.s IN ['dup-a', 'dup-b'] RETURN count(n) AS c`); got != 2 {
		t.Fatalf("UNWIND self-write: %d nodes, want 2 (one per key)", got)
	}
	drain(`UNWIND [1077, 1077.0, 1077] AS v MERGE (:L {p: v})`)
	if got := count(`MATCH (n:L) WHERE n.p = 1077 RETURN count(n) AS c`); got != 1 {
		t.Fatalf("UNWIND cross-type self-write: %d nodes, want 1", got)
	}
	// The committed node is now in the index, and the probe must find it.
	before := mergeIndexProbeCount.Load()
	drain(`MERGE (:L {s: 'dup-a'})`)
	if mergeIndexProbeCount.Load() == before {
		t.Fatal("a MERGE on a committed key did not probe the index")
	}
	if got := count(`MATCH (n:L {s: 'dup-a'}) RETURN count(n) AS c`); got != 1 {
		t.Fatalf("MERGE after commit: %d nodes, want 1", got)
	}
}

// TestMergeIndexProbe_UsesIndex is the regression test for the defect itself: an
// indexed MERGE must answer from the index. Before rmp #2812 the match phase had
// no index path at all, so the counter could not move.
func TestMergeIndexProbe_UsesIndex(t *testing.T) {
	eng := newMergeProbeEngine(t, false)
	for _, q := range []string{
		`MERGE (n:L {s: 's3'}) RETURN count(n) AS c`,
		`MERGE (n:L {p: 3}) RETURN count(n) AS c`,
		`UNWIND ['s3'] AS v MERGE (n:L {s: v}) RETURN count(n) AS c`,
	} {
		before := mergeIndexProbeCount.Load()
		res, err := eng.RunAny(context.Background(), q, nil)
		rows := mergeProbeRows(t, q, res, err)
		if mergeIndexProbeCount.Load() == before {
			t.Fatalf("%s: the match phase did not probe the index", q)
		}
		if len(rows) != 1 || rows[0] != "c=4" {
			t.Fatalf("%s: rows %v, want [c=4]", q, rows)
		}
	}
}

// TestMergeIndexProbe_ConcurrentCommitsNeverHideAMatch is the MVCC half of the
// contract. The index is written at commit time and read at the present, and a
// committer delivers its changes BEFORE it publishes, so an index can already
// describe a commit this snapshot cannot see. Writers churn the indexed key of a
// small population while a checker, inside its own write transaction, asks the
// probe and walks the graph through the same transaction view; whenever the probe
// answers, every node the walk matches must be among its candidates.
//
// The writers change the key, add and remove the label, and run explicit
// transactions that re-value a node and roll back. The last two shapes need the
// index to be maintained from committed state (rmp #2931): before that, a label
// add read a peer's uncommitted value, and a rollback left the index wrong for
// good, which this test detected as a probe missing a match.
//
// It also requires the run to have produced both outcomes — answers, and declines
// caused by concurrent commits — so a run that never overlapped a commit cannot
// pass vacuously.
func TestMergeIndexProbe_ConcurrentCommitsNeverHideAMatch(t *testing.T) {
	const (
		nodes   = 64
		keys    = 8
		writers = 4
	)
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < nodes; i++ {
		n := fmt.Sprintf("c%d", i)
		mustNoErr(t, g.AddNode(n))
		mustNoErr(t, g.SetNodeLabel(n, "L"))
		mustNoErr(t, g.SetNodeProperty(n, "tag", lpg.StringValue(n)))
		mustNoErr(t, g.SetNodeProperty(n, "s", lpg.StringValue(fmt.Sprintf("k%d", i%keys))))
	}
	eng := NewEngine(g)
	if _, err := eng.Run(context.Background(), "CREATE INDEX FOR (n:L) ON (n.s)", nil); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var writes atomic.Int64
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, seed+1)) //nolint:gosec // a reproducible workload, not a secret
			for !stop.Load() {
				tag := fmt.Sprintf("c%d", rng.IntN(nodes))
				var q string
				switch rng.IntN(5) {
				case 0:
					// An explicit transaction re-values a node, holds the write open
					// across other writers' commits, and rolls it back: the shape
					// that used to leave the index permanently wrong (rmp #2931).
					tx, err := eng.BeginTx(context.Background())
					if err != nil {
						continue
					}
					if res, err := tx.ExecAny(fmt.Sprintf("MATCH (n {tag: '%s'}) SET n.s = 'k%d'", tag, rng.IntN(keys)), nil); err == nil {
						for res.Next() {
						}
						_ = res.Close()
					}
					time.Sleep(time.Duration(rng.IntN(200)) * time.Microsecond)
					_ = tx.Rollback()
					writes.Add(1)
					continue
				case 1:
					q = fmt.Sprintf("MATCH (n {tag: '%s'}) REMOVE n:L", tag)
				case 2:
					q = fmt.Sprintf("MATCH (n {tag: '%s'}) SET n:L", tag)
				default:
					q = fmt.Sprintf("MATCH (n {tag: '%s'}) SET n.s = 'k%d'", tag, rng.IntN(keys))
				}
				res, err := eng.RunAny(context.Background(), q, nil)
				if err != nil {
					continue // a serialization conflict is an expected outcome here
				}
				for res.Next() {
				}
				_ = res.Close()
				writes.Add(1)
			}
		}(uint64(w) + 1)
	}

	var answered, declined, checks int
	deadline := time.Now().Add(1500 * time.Millisecond)
	rng := rand.New(rand.NewPCG(99, 100)) //nolint:gosec // a reproducible workload, not a secret
	for time.Now().Before(deadline) {
		key := fmt.Sprintf("k%d", rng.IntN(keys))
		err := g.ApplyVersioned(func(wtx lpg.WriteTx) error {
			a := &lpgMutatorAdapter{g: g, buf: &exec.IndexBuffer{}, wtx: wtx}
			probe := a.MergeIndexProbe("L", "s")
			if probe == nil {
				return fmt.Errorf("no probe for (:L, s)")
			}
			ids, ok := probe.Candidates(lpg.StringValue(key), nil)
			// The walk, through the same accessors the MERGE re-check uses.
			var walk []graph.NodeID
			a.WalkNodeIDs(func(id graph.NodeID) bool {
				n, ok := a.ResolveNodeLabel(id)
				if !ok || !a.HasNodeLabelInTx(n, "L") {
					return true
				}
				if v, ok := a.NodePropertyInTx(n, "s"); ok {
					if s, _ := v.String(); v.Kind() == lpg.PropString && s == key {
						walk = append(walk, id)
					}
				}
				return true
			})
			checks++
			if !ok {
				declined++
				return nil
			}
			answered++
			have := make(map[uint64]struct{}, len(ids))
			for _, id := range ids {
				have[id] = struct{}{}
			}
			for _, id := range walk {
				if _, ok := have[uint64(id)]; !ok {
					return fmt.Errorf("probe for %q answered %v but the walk also matches node %d", key, ids, id)
				}
			}
			return nil
		})
		if err != nil {
			t.Error(err)
			break
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("checks=%d answered=%d declined=%d writes=%d", checks, answered, declined, writes.Load())
	if answered == 0 || declined == 0 || writes.Load() == 0 {
		t.Fatalf("the run did not exercise both outcomes (answered=%d declined=%d writes=%d)",
			answered, declined, writes.Load())
	}
}

// TestMergeIndexProbe_IndexCreatedInsideAnOpenTransaction is the regression test
// for the audit finding against rmp #2812. An explicit transaction holds no
// schema gate between its statements, so an index can be created — built from a
// snapshot, and caught up to its registration — after the transaction began. For
// that transaction the index describes the wrong instant: a node the transaction
// still sees has already been deleted from it. A MERGE that trusted it would
// create a duplicate of a node the transaction can see. Registration therefore
// raises the watermark the probe checks, and the probe declines.
//
// Two shapes: the index created fresh, and dropped and re-created.
func TestMergeIndexProbe_IndexCreatedInsideAnOpenTransaction(t *testing.T) {
	for _, dropFirst := range []bool{false, true} {
		name := "create"
		if dropFirst {
			name = "drop-then-create"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			g := lpg.New[string, float64](adjlist.Config{})
			eng := NewEngine(g)
			run := func(q string) {
				res, err := eng.RunAny(ctx, q, nil)
				mergeProbeRows(t, q, res, err)
			}
			if dropFirst {
				run("CREATE INDEX l_s FOR (n:L) ON (n.s)")
			}
			run(`CREATE (:L {s: 'v', tag: 'orig'})`)

			tx, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			res, err := tx.ExecAny(`MATCH (n:L) RETURN count(n) AS c`, nil)
			if rows := mergeProbeRows(t, "count", res, err); len(rows) != 1 || rows[0] != "c=1" {
				t.Fatalf("fixture: the transaction sees %v, want one node", rows)
			}

			// Dropped BEFORE the delete, so the delete delivers to no index and
			// raises no watermark; only the re-creation can tell the probe.
			if dropFirst {
				run("DROP INDEX l_s")
			}
			run(`MATCH (n:L {s: 'v'}) DETACH DELETE n`)
			run("CREATE INDEX l_s FOR (n:L) ON (n.s)")

			res, err = tx.ExecAny(`MERGE (n:L {s: 'v'}) RETURN n.tag AS tag`, nil)
			rows := mergeProbeRows(t, "merge", res, err)
			if len(rows) != 1 || rows[0] != `tag="orig"` {
				t.Fatalf("MERGE in the open transaction returned %v, want [tag=\"orig\"]: it created a "+
					"duplicate of a node its own snapshot still holds", rows)
			}
		})
	}
}
