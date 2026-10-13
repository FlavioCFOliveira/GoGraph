package cypher

// index_snapshot_read_test.go — regression battery for rmp #2937, #2938, #2946 and
// the rmp #2062 maintenance contract.
//
// Layer: short.
//
// rmp #2937: a MATCH access path that reads a property index must return its
// reader's snapshot. Every access path is driven with a reader that holds a
// snapshot while a peer commits a change to the indexed values, on the in-memory
// and the WAL wirings, through an explicit read transaction and an explicit write
// transaction; a differential stress runs seeks against label scans in the same
// snapshot under 8 and 64 writers. The negative control turns the proof off
// ([setTrustIndexSnapshot]) and requires the same tests to see the defect.
//
// These tests are NOT parallel: they assert on the process-global decline and
// build counters, which only a test running alone can attribute to itself.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// snapReadWirings are the two engine wirings every shape runs on.
var snapReadWirings = []struct {
	name string
	wal  bool
}{{"memory", false}, {"wal", true}}

// newSnapReadEngine seeds g with seed, which writes through the raw lpg API before
// any index exists, wraps it in an engine on the chosen wiring, and runs ddl.
func newSnapReadEngine(t *testing.T, walBacked bool, seed func(g *lpg.Graph[string, float64]), ddl ...string) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	seed(g)
	var eng *Engine
	if walBacked {
		wr, err := wal.OpenWithSyncLatency(filepath.Join(t.TempDir(), "wal"), synclatency.ForTest(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = wr.Close() })
		eng = NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		}))
	} else {
		eng = NewEngine(g)
	}
	for _, q := range ddl {
		commitStateExec(t, eng, q)
	}
	return eng
}

// snapReadNode adds a :L node with the given tag, string s and integer p.
func snapReadNode(t *testing.T, g *lpg.Graph[string, float64], tag, s string, p int64) {
	t.Helper()
	mustNoErr(t, g.AddNode(tag))
	mustNoErr(t, g.SetNodeLabel(tag, "L"))
	mustNoErr(t, g.SetNodeProperty(tag, "tag", lpg.StringValue(tag)))
	mustNoErr(t, g.SetNodeProperty(tag, "s", lpg.StringValue(s)))
	mustNoErr(t, g.SetNodeProperty(tag, "p", lpg.Int64Value(p)))
}

// snapReadSeed is 512 filler :L nodes (s = f000…, p = 0…511) and ten targets
// t0…t9 (s = v0…v9, p = 1000…1009), so every target range is selective enough
// for every range access path to fire.
func snapReadSeed(t *testing.T) func(g *lpg.Graph[string, float64]) {
	return func(g *lpg.Graph[string, float64]) {
		for i := 0; i < 512; i++ {
			snapReadNode(t, g, fmt.Sprintf("f%d", i), fmt.Sprintf("f%03d", i), int64(i))
		}
		for i := 0; i < 10; i++ {
			snapReadNode(t, g, fmt.Sprintf("t%d", i), fmt.Sprintf("v%d", i), int64(1000+i))
		}
	}
}

// snapReadDDL is a hash and a btree index on (:L, s) and a btree on (:L, p); each
// CREATE INDEX also registers the float64 numeric companion.
var snapReadDDL = []string{
	"CREATE INDEX FOR (n:L) ON (n.s)",
	"CREATE INDEX FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}",
	"CREATE INDEX FOR (n:L) ON (n.p) OPTIONS {indexType: 'btree'}",
}

// snapReadPeer moves t0 out of every target predicate: from s = 'v0', p = 1000 to
// s = 'x0', p = 5.
const snapReadPeer = `MATCH (n:L {tag: 't0'}) SET n.s = 'x0', n.p = 5`

// snapReadShape is one access path, the plan operator that proves it was chosen,
// the count at the reader's snapshot, and the count the defect produced.
type snapReadShape struct {
	name, q, op  string
	want, defect int64
	params       map[string]any
	xparams      map[string]expr.Value // params, for Explain
}

// snapReadRows is the index nested-loop join's per-row keys.
var (
	snapReadRows  = map[string]any{"rows": []any{map[string]any{"a": int64(1000)}, map[string]any{"a": int64(1001)}}}
	snapReadXRows = map[string]expr.Value{"rows": expr.ListValue{
		expr.MapValue{"a": expr.IntegerValue(1000)}, expr.MapValue{"a": expr.IntegerValue(1001)},
	}}
)

var snapReadShapes = []snapReadShape{
	{"equality/old-value", `MATCH (n:L {s: 'v0'}) RETURN count(n) AS c`, "NodeByIndexSeek", 1, 0, nil, nil},
	{"equality/new-value", `MATCH (n:L {s: 'x0'}) RETURN count(n) AS c`, "NodeByIndexSeek", 0, 1, nil, nil},
	{"key-set", `MATCH (n:L) WHERE n.s = 'v0' OR n.s = 'v1' RETURN count(n) AS c`, "NodeByIndexSeekSet", 2, 1, nil, nil},
	{"string-range", `MATCH (n:L) WHERE n.s >= 'v' AND n.s < 'w' RETURN count(n) AS c`, "NodeByIndexRangeScan", 10, 9, nil, nil},
	{"prefix", `MATCH (n:L) WHERE n.s STARTS WITH 'v' RETURN count(n) AS c`, "NodeByIndexRangeScan", 10, 9, nil, nil},
	{"numeric-range", `MATCH (n:L) WHERE n.p >= 1000 AND n.p < 2000 RETURN count(n) AS c`, "NodeByIndexRangeScan", 10, 9, nil, nil},
	{"intersection", `MATCH (n:L) WHERE n.s >= 'v' AND n.s < 'w' AND n.p >= 1000 AND n.p < 2000 RETURN count(n) AS c`, "NodeByIndexRangeScan", 10, 9, nil, nil},
	{"index-nested-loop", `UNWIND $rows AS r MATCH (b:L) WHERE b.p = r.a RETURN count(b) AS c`, "IndexNestedLoopJoin", 2, 1, snapReadRows, snapReadXRows},
}

// snapReadTxCount runs q in tx and returns its single count.
func snapReadTxCount(t *testing.T, tx *ExplicitTx, q string, params map[string]any) int64 {
	t.Helper()
	res, err := tx.ExecAny(q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	n := snapReadCountOf(res)
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// snapReadCountOf returns the single count column of res, or -1 when it has none.
func snapReadCountOf(res *Result) int64 {
	var n int64 = -1
	for res.Next() {
		switch v := res.Record()["c"].(type) {
		case int64:
			n = v
		case int:
			n = int64(v)
		case expr.IntegerValue:
			n = int64(v)
		}
	}
	return n
}

// snapReadCount runs q autocommit and returns its single count.
func snapReadCount(t *testing.T, eng *Engine, q string, params map[string]any) int64 {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	n := snapReadCountOf(res)
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// runSnapReadShapes opens a reader of the given kind, lets the peer commit, and
// returns each shape's count in the reader's snapshot and the declines it caused.
func runSnapReadShapes(t *testing.T, eng *Engine, readOnly bool) (got []int64, declines uint64) {
	t.Helper()
	ctx := context.Background()
	var tx *ExplicitTx
	var err error
	if readOnly {
		tx, err = eng.BeginReadTx(ctx)
	} else {
		tx, err = eng.BeginTx(ctx)
	}
	mustNoErr(t, err)
	defer func() { _ = tx.Rollback() }()
	// The reader's snapshot holds t0 at s = 'v0', p = 1000; the peer's commit
	// lands after it.
	commitStateExec(t, eng, snapReadPeer)
	before := indexSnapshotDeclines()
	for _, sh := range snapReadShapes {
		got = append(got, snapReadTxCount(t, tx, sh.q, sh.params))
	}
	return got, indexSnapshotDeclines() - before
}

// TestIndexSnapshotRead_PeerCommitAfterSnapshot is the rmp #2937 reproduction
// over every MATCH access path that reads a property index, on both wirings and
// both kinds of explicit transaction. Before the fix the equality seek returned
// 0 for the value the snapshot holds and 1 for the value it does not, and every
// other path lost t0.
func TestIndexSnapshotRead_PeerCommitAfterSnapshot(t *testing.T) {
	for _, w := range snapReadWirings {
		for _, kind := range []struct {
			name     string
			readOnly bool
		}{{"BeginReadTx", true}, {"BeginTx", false}} {
			t.Run(w.name+"/"+kind.name, func(t *testing.T) {
				eng := newSnapReadEngine(t, w.wal, snapReadSeed(t), snapReadDDL...)
				// The access path each shape is meant to exercise is the one planned.
				for _, sh := range snapReadShapes {
					plan, err := eng.Explain(sh.q, sh.xparams)
					mustNoErr(t, err)
					if !strings.Contains(plan, sh.op) {
						t.Fatalf("%s: the plan does not use %s, so this shape tests nothing:\n%s", sh.name, sh.op, plan)
					}
				}
				inljBefore := indexNestedLoopBuildCount.Load()
				intersectBefore := indexIntersectBuildCount.Load()
				got, declines := runSnapReadShapes(t, eng, kind.readOnly)
				for i, sh := range snapReadShapes {
					if got[i] != sh.want {
						t.Errorf("%s: %d rows at the reader's snapshot, want %d: the index described the peer's commit", sh.name, got[i], sh.want)
					}
				}
				if indexNestedLoopBuildCount.Load() == inljBefore {
					t.Error("the index nested-loop join was not built inside the transaction")
				}
				if indexIntersectBuildCount.Load() == intersectBefore {
					t.Error("the index intersection was not built inside the transaction")
				}
				if declines < uint64(len(snapReadShapes)) {
					t.Errorf("%d index reads declined for %d shapes: a guarded path did not ask for the proof", declines, len(snapReadShapes))
				}
			})
		}
	}
}

// TestIndexSnapshotRead_NegativeControl turns the proof off and requires the
// reproduction to see the defect on every shape: a test that cannot fail proves
// nothing.
func TestIndexSnapshotRead_NegativeControl(t *testing.T) {
	for _, w := range snapReadWirings {
		t.Run(w.name, func(t *testing.T) {
			eng := newSnapReadEngine(t, w.wal, snapReadSeed(t), snapReadDDL...)
			setTrustIndexSnapshot(eng, true)
			got, _ := runSnapReadShapes(t, eng, true)
			for i, sh := range snapReadShapes {
				if got[i] != sh.defect {
					t.Errorf("%s: %d rows with the proof off, want the defect's %d", sh.name, got[i], sh.defect)
				}
			}
		})
	}
}

// TestIndexSnapshotRead_ProofHoldsServesTheIndex pins that the fix declines only
// when it must: with no commit after the reader's snapshot, every shape is
// answered from the index and none declines.
func TestIndexSnapshotRead_ProofHoldsServesTheIndex(t *testing.T) {
	for _, w := range snapReadWirings {
		t.Run(w.name, func(t *testing.T) {
			eng := newSnapReadEngine(t, w.wal, snapReadSeed(t), snapReadDDL...)
			tx, err := eng.BeginReadTx(context.Background())
			mustNoErr(t, err)
			defer func() { _ = tx.Rollback() }()
			before := indexSnapshotDeclines()
			// With no peer commit the graph is the snapshot every shape's want describes.
			for _, sh := range snapReadShapes {
				if got := snapReadTxCount(t, tx, sh.q, sh.params); got != sh.want {
					t.Errorf("%s: %d rows, want %d", sh.name, got, sh.want)
				}
			}
			if d := indexSnapshotDeclines() - before; d != 0 {
				t.Errorf("%d index reads declined with no commit after the snapshot", d)
			}
		})
	}
}

// snapStressKeys is the value domain the stress writers move their nodes across.
const snapStressKeys = 8

// runSnapReadStress drives writers autocommit writers, each moving its own two
// :L nodes across snapStressKeys string keys and 64 integer values, against four
// readers that each hold a read snapshot across at least one commit and compare,
// in that snapshot, every equality seek and a set of range seeks with the label
// scan the index replaces. It returns the comparisons made and the mismatches.
func runSnapReadStress(t *testing.T, eng *Engine, writers int, d time.Duration) (compared, mismatched int64) {
	t.Helper()
	ctx := context.Background()
	var commits atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var firstErr atomic.Pointer[error]
	fail := func(err error) { firstErr.CompareAndSwap(nil, &err) }
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 2937)) //nolint:gosec // a reproducible workload, not a secret
			for {
				select {
				case <-stop:
					return
				default:
				}
				tag := fmt.Sprintf("w%d_%d", w, r.IntN(2))
				res, err := eng.RunAny(ctx, `MATCH (n:L {tag: $t}) SET n.s = $s, n.p = $p`, map[string]any{
					"t": tag, "s": fmt.Sprintf("k%d", r.IntN(snapStressKeys)), "p": int64(r.IntN(64)),
				})
				if err == nil {
					for res.Next() {
					}
					err = res.Err()
					_ = res.Close()
				}
				if errors.Is(err, mvcc.ErrSerializationConflict) {
					// An autocommit statement with no session may not see this
					// writer's own previous commit yet; the write is simply retried.
					continue
				}
				if err != nil {
					fail(err)
					return
				}
				commits.Add(1)
			}
		}(w)
	}
	var cmp, mis atomic.Int64
	for rd := 0; rd < 4; rd++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tx, err := eng.BeginReadTx(ctx)
				if err != nil {
					fail(err)
					return
				}
				// Hold the snapshot across at least one peer commit.
				c0 := commits.Load()
				for commits.Load() == c0 {
					select {
					case <-stop:
						_ = tx.Rollback()
						return
					default:
						runtime.Gosched()
					}
				}
				count := func(q string, params map[string]any) int64 {
					res, err := tx.ExecAny(q, params)
					if err != nil {
						fail(err)
						return -1
					}
					defer func() { _ = res.Close() }()
					n := snapReadCountOf(res)
					if err := res.Err(); err != nil {
						fail(err)
					}
					return n
				}
				for k := 0; k < snapStressKeys; k++ {
					key := map[string]any{"k": fmt.Sprintf("k%d", k)}
					seek := count(`MATCH (n:L {s: $k}) RETURN count(n) AS c`, key)
					scan := count(`MATCH (n:L) WHERE n.s + '' = $k RETURN count(n) AS c`, key)
					cmp.Add(1)
					if seek != scan {
						mis.Add(1)
					}
				}
				for lo := int64(0); lo < 64; lo += 16 {
					rng := map[string]any{"lo": lo, "hi": lo + 8}
					seek := count(`MATCH (n:L) WHERE n.p >= $lo AND n.p < $hi RETURN count(n) AS c`, rng)
					scan := count(`MATCH (n:L) WHERE n.p + 0 >= $lo AND n.p + 0 < $hi RETURN count(n) AS c`, rng)
					cmp.Add(1)
					if seek != scan {
						mis.Add(1)
					}
				}
				_ = tx.Rollback()
			}
		}()
	}
	time.Sleep(d)
	close(stop)
	wg.Wait()
	if p := firstErr.Load(); p != nil {
		t.Fatalf("stress: %v", *p)
	}
	return cmp.Load(), mis.Load()
}

// snapStressSeed is 512 fillers outside every stressed value and two nodes per
// writer, all starting at s = 'k0', p = 0.
func snapStressSeed(t *testing.T, writers int) func(g *lpg.Graph[string, float64]) {
	return func(g *lpg.Graph[string, float64]) {
		for i := 0; i < 512; i++ {
			snapReadNode(t, g, fmt.Sprintf("f%d", i), fmt.Sprintf("f%03d", i), int64(10000+i))
		}
		for w := 0; w < writers; w++ {
			for j := 0; j < 2; j++ {
				snapReadNode(t, g, fmt.Sprintf("w%d_%d", w, j), "k0", 0)
			}
		}
	}
}

var snapStressDDL = []string{
	"CREATE INDEX FOR (n:L) ON (n.s)",
	"CREATE INDEX FOR (n:L) ON (n.p) OPTIONS {indexType: 'btree'}",
}

// TestIndexSnapshotRead_SeekVsScanStress is the differential under concurrency:
// every seek must equal the label scan in the same snapshot, at 8 and at 64
// writers, on both wirings.
func TestIndexSnapshotRead_SeekVsScanStress(t *testing.T) {
	for _, w := range snapReadWirings {
		for _, writers := range []int{8, 64} {
			t.Run(fmt.Sprintf("%s/writers=%d", w.name, writers), func(t *testing.T) {
				eng := newSnapReadEngine(t, w.wal, snapStressSeed(t, writers), snapStressDDL...)
				before := indexSnapshotDeclines()
				compared, mismatched := runSnapReadStress(t, eng, writers, 500*time.Millisecond)
				t.Logf("compared %d, mismatched %d, declined %d", compared, mismatched, indexSnapshotDeclines()-before)
				if compared == 0 {
					t.Fatal("no comparison was made")
				}
				if mismatched != 0 {
					t.Errorf("%d of %d seeks disagreed with the label scan in the same snapshot", mismatched, compared)
				}
				if indexSnapshotDeclines() == before {
					t.Error("no index read declined: the readers never overlapped a commit with the proof asked")
				}
			})
		}
	}
}

// TestIndexSnapshotRead_SeekVsScanStressNegativeControl is the stress with the
// proof off; it must see seeks disagree with the scan.
func TestIndexSnapshotRead_SeekVsScanStressNegativeControl(t *testing.T) {
	eng := newSnapReadEngine(t, false, snapStressSeed(t, 64), snapStressDDL...)
	setTrustIndexSnapshot(eng, true)
	compared, mismatched := runSnapReadStress(t, eng, 64, 500*time.Millisecond)
	t.Logf("compared %d, mismatched %d", compared, mismatched)
	if compared == 0 {
		t.Fatal("no comparison was made")
	}
	if mismatched == 0 {
		t.Errorf("with the proof off, all %d seeks agreed with the scan: the stress cannot detect the defect", compared)
	}
}

// TestUnboundIndex_NeverServesARead is rmp #2938: an index registered through the
// Go API without a binding is maintained by nothing, so no MATCH access path may
// read it. Before the fix an empty unbound hash index under the auto-generated
// name, or under any other name, made the equality seek return 0 rows for a value
// the label scan found.
func TestUnboundIndex_NeverServesARead(t *testing.T) {
	for _, w := range snapReadWirings {
		for _, name := range []string{"l_s_hash", "custom_unbound"} {
			t.Run(w.name+"/"+name, func(t *testing.T) {
				eng := newSnapReadEngine(t, w.wal, snapReadSeed(t))
				mgr := eng.g.IndexManager()
				mustNoErr(t, mgr.CreateIndex(name, indexhash.New[string]()))
				mustNoErr(t, mgr.CreateIndex(name+"_btree", indexbtree.New[string]()))
				for _, q := range []string{
					`MATCH (n:L {s: 'v3'}) RETURN count(n) AS c`,
					`MATCH (n:L) WHERE n.s = 'v3' OR n.s = 'v4' RETURN count(n) AS c`,
					`MATCH (n:L) WHERE n.s STARTS WITH 'v' RETURN count(n) AS c`,
				} {
					plan, err := eng.Explain(q, nil)
					mustNoErr(t, err)
					if strings.Contains(plan, "NodeByIndex") {
						t.Errorf("%s plans an index read over an unbound index:\n%s", q, plan)
					}
				}
				// Written through the engine after the registration: the unbound
				// index is not told, and no read may depend on it.
				commitStateExec(t, eng, `CREATE (:L {tag: 'new', s: 'v3'})`)
				if c := commitStateCount(t, eng, `MATCH (n:L {s: 'v3'}) RETURN count(n) AS c`); c != 2 {
					t.Errorf("MATCH (n:L {s: 'v3'}) returned %d rows, want 2", c)
				}
				if c := commitStateCount(t, eng, `MATCH (n:L) WHERE n.s STARTS WITH 'v' RETURN count(n) AS c`); c != 11 {
					t.Errorf("the prefix match returned %d rows, want 11", c)
				}
			})
		}
	}
}

// constraintStraddleShape is one rmp #2946 shape: the committed seed, the open
// transaction's eager statement, and the DDL.
type constraintStraddleShape struct {
	name, eager, ddl, violating string
}

var constraintStraddleShapes = []constraintStraddleShape{
	{"unique/eager-duplicate", `CREATE (:L {s: 'v'})`,
		`CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS UNIQUE`,
		`MATCH (n:L) WHERE n.s = 'v' RETURN count(n) AS c`},
	{"not-null/eager-remove", `MATCH (n:L) REMOVE n.s`,
		`CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS NOT NULL`,
		`MATCH (n:L) WHERE n.s IS NULL RETURN count(n) AS c`},
	{"not-null/eager-create", `CREATE (:L)`,
		`CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS NOT NULL`,
		`MATCH (n:L) WHERE n.s IS NULL RETURN count(n) AS c`},
}

// TestCreateConstraint_ValidatesCommittedStateOnly is rmp #2946 on both wirings:
// with a committed (:L {s: 'v'}) and an open transaction whose eager write would
// violate the constraint, the DDL validates committed state and succeeds, and the
// transaction is refused at its own commit, so the committed graph satisfies the
// constraint. Before the fix every shape's DDL was refused.
func TestCreateConstraint_ValidatesCommittedStateOnly(t *testing.T) {
	for _, w := range snapReadWirings {
		for _, sh := range constraintStraddleShapes {
			t.Run(w.name+"/"+sh.name, func(t *testing.T) {
				eng := newSnapReadEngine(t, w.wal, func(*lpg.Graph[string, float64]) {})
				commitStateExec(t, eng, `CREATE (:L {s: 'v'})`)
				tx, err := eng.BeginTx(context.Background())
				mustNoErr(t, err)
				res, err := tx.ExecAny(sh.eager, nil)
				mustNoErr(t, err)
				_ = res.Close()
				if _, err := eng.RunAny(context.Background(), sh.ddl, nil); err != nil {
					_ = tx.Rollback()
					t.Fatalf("%s was refused over compliant committed data: %v", sh.ddl, err)
				}
				cerr := tx.Commit()
				if cerr == nil {
					t.Fatalf("the transaction open across %q committed its violating write", sh.ddl)
				}
				if !errors.Is(cerr, exec.ErrConstraintViolation) {
					t.Errorf("the commit was refused, but not as a constraint violation: %v", cerr)
				}
				want := int64(0)
				if strings.Contains(sh.name, "unique") {
					want = 1
				}
				if c := commitStateCount(t, eng, sh.violating); c != want {
					t.Errorf("%s = %d after the refused commit, want %d", sh.violating, c, want)
				}
			})
		}
	}
}

// TestCreateConstraint_CommittedViolationStillRefused is the sensitivity control
// for the test above: a committed violation that an open transaction has eagerly
// hidden is still found, because the DDL reads committed state.
func TestCreateConstraint_CommittedViolationStillRefused(t *testing.T) {
	for _, w := range snapReadWirings {
		for _, c := range []struct{ name, seed, eager, ddl string }{
			{"unique", `CREATE (:L {s: 'v'}), (:L {s: 'v', x: 1})`, `MATCH (n:L {x: 1}) SET n.s = 'w'`,
				`CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS UNIQUE`},
			{"not-null", `CREATE (:L {s: 'v'}), (:L {x: 1})`, `MATCH (n:L {x: 1}) SET n.s = 'w'`,
				`CREATE CONSTRAINT c FOR (n:L) REQUIRE n.s IS NOT NULL`},
		} {
			t.Run(w.name+"/"+c.name, func(t *testing.T) {
				eng := newSnapReadEngine(t, w.wal, func(*lpg.Graph[string, float64]) {})
				commitStateExec(t, eng, c.seed)
				tx, err := eng.BeginTx(context.Background())
				mustNoErr(t, err)
				defer func() { _ = tx.Rollback() }()
				res, err := tx.ExecAny(c.eager, nil)
				mustNoErr(t, err)
				_ = res.Close()
				_, derr := eng.RunAny(context.Background(), c.ddl, nil)
				if !errors.Is(derr, exec.ErrConstraintViolation) {
					t.Errorf("%s over violating committed data: err = %v, want a constraint violation", c.ddl, derr)
				}
			})
		}
	}
}

// TestIndexMaintenance_RawEdgeWritesCannotStaleAnEngineIndex is the rmp #2062
// guard for the contract documented on lpg.ErrIndexedRawWrite: the raw edge
// mutators are admitted on an indexed graph because no index the engine builds
// consumes an edge change, while the raw node mutators are refused.
func TestIndexMaintenance_RawEdgeWritesCannotStaleAnEngineIndex(t *testing.T) {
	eng := newSnapReadEngine(t, false, snapReadSeed(t), append(append([]string{}, snapReadDDL...),
		`CREATE CONSTRAINT u FOR (n:L) REQUIRE n.tag IS UNIQUE`)...)
	g := eng.g
	mgr := g.IndexManager()
	lid := labelIDOf(g.Registry(), "L")
	edgeOps := []index.ChangeOp{index.OpAddEdgeLabel, index.OpRemoveEdgeLabel, index.OpSetEdgeProperty, index.OpDelEdgeProperty}
	names := mgr.ListIndexes()
	if len(names) < 4 {
		t.Fatalf("%d indexes registered, want the hash, the two btrees, the companions and the UNIQUE backing index: %v", len(names), names)
	}
	for _, name := range names {
		sub, err := mgr.GetIndex(name)
		mustNoErr(t, err)
		f, ok := sub.(index.ChangeFilter)
		if !ok {
			t.Errorf("index %q has no ChangeFilter, so the fan-out assumes it concerned by node changes only", name)
			continue
		}
		for _, op := range edgeOps {
			for _, key := range []string{"s", "p", "tag"} {
				c := index.Change{Op: op, Node: 1, Dst: 2, Label: lid, Property: keyIDOf(g.PropertyKeys(), key)}
				if f.Concerns(c) {
					t.Errorf("index %q claims edge change %v on %q", name, op, key)
				}
				if mgr.Concerns([]index.Change{c}) {
					t.Errorf("the manager would deliver edge change %v on %q", op, key)
				}
			}
		}
	}
	// The raw edge mutators are admitted and leave every index answer intact.
	mustNoErr(t, g.AddEdge("t1", "t2", 1))
	if err := g.SetEdgeLabel("t1", "t2", "R"); err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, g.SetEdgeProperty("t1", "t2", "s", lpg.StringValue("v5")))
	mustNoErr(t, g.AddEdge("t3", "brand-new", 1))
	must(t).E(g.RemoveEdge("t1", "t2"))
	// No node was touched, so every shape still sees the seeded graph.
	for _, sh := range snapReadShapes {
		if c := snapReadCount(t, eng, sh.q, sh.params); c != sh.want {
			t.Errorf("%s after raw edge writes: %d rows, want %d", sh.name, c, sh.want)
		}
	}
	// The raw node mutators are refused.
	if err := g.SetNodeProperty("t1", "s", lpg.StringValue("zz")); !errors.Is(err, lpg.ErrIndexedRawWrite) {
		t.Errorf("raw SetNodeProperty on an indexed graph: err = %v, want ErrIndexedRawWrite", err)
	}
}

// labelIDOf interns name in r and returns its id as an index.Change carries it.
// Test names are short constants that the token bound (lpg.MaxTokenLen) cannot
// refuse, so a refusal is a defect in the test itself and panics.
func labelIDOf(r *lpg.LabelRegistry, name string) uint32 {
	id, err := r.Intern(name)
	if err != nil {
		panic(err)
	}
	return uint32(id)
}

// keyIDOf is [labelIDOf] for a property key.
func keyIDOf(r *lpg.PropertyKeyRegistry, name string) uint32 {
	id, err := r.Intern(name)
	if err != nil {
		panic(err)
	}
	return uint32(id)
}
