package cypher_test

// plan_cache_ddl_window_test.go — rmp #2854.
//
// A plan-cache entry memoises paramTypes, which is inferred from the index
// catalog while the entry is compiled. A DDL statement registers or drops an
// index and then clears the plan cache. The question is whether a compilation
// that read the catalog BEFORE the DDL can publish its entry AFTER the clear,
// so that the pre-change inference survives the invalidation.
//
// The interleaving is forced without sleeps. A gated hash subscriber parks the
// compiling goroutine inside the catalog read (hashIndexKind calls its
// DistinctValues); the DDL runs to completion while it is parked; the
// subscriber is then released and the compilation finishes. The verdict is
// read from a SEQUENTIAL control engine that runs the same DDL and the same
// query with no interleaving: the raced engine must give the same answer.
//
// Layer: short. goleak-clean: the compiling goroutine is joined before return.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

const (
	ddlWindowQuery = "MATCH (n:P) WHERE n.name = $p RETURN n.name"
	ddlWindowWait  = 10 * time.Second
)

// gatedHashSub is a string-keyed hash subscriber whose first DistinctValues
// call after arm parks until release is closed. distinct is the population it
// reports: 0 gives the parameter inference no signal, >0 proves String.
type gatedHashSub struct {
	armed    atomic.Bool
	reached  chan struct{}
	release  chan struct{}
	distinct uint64
}

func newGatedHashSub(distinct uint64) *gatedHashSub {
	return &gatedHashSub{reached: make(chan struct{}), release: make(chan struct{}), distinct: distinct}
}

func (s *gatedHashSub) Apply(index.Change)                           {}
func (s *gatedHashSub) Kind() string                                 { return "hash" }
func (s *gatedHashSub) LookupAppend(_ string, dst []uint64) []uint64 { return dst }

func (s *gatedHashSub) DistinctValues() uint64 {
	if s.armed.CompareAndSwap(true, false) {
		close(s.reached)
		<-s.release
	}
	return s.distinct
}

func ddlWindowEngine(t *testing.T) (*cypher.Engine, *index.Manager) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	res, err := eng.RunAny(context.Background(), "CREATE (:P {name: 'a'}), (:P {name: 'b'})", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = res.Close()
	return eng, g.IndexManager()
}

func ddlWindowExec(t *testing.T, eng *cypher.Engine, stmt string) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), stmt, nil)
	if err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	_ = res.Close()
}

// ddlWindowVerdict runs the query with an INTEGER parameter and reports
// whether the parameter type check rejected it.
func ddlWindowVerdict(t *testing.T, eng *cypher.Engine) bool {
	t.Helper()
	res, err := eng.Run(context.Background(), ddlWindowQuery, map[string]expr.Value{"p": expr.IntegerValue(1)})
	if err == nil {
		_ = res.Close()
		return false
	}
	var pte *sema.ParamTypeError
	if !errors.As(err, &pte) {
		t.Fatalf("query: unexpected error %v", err)
	}
	return true
}

// raceCompileAgainstDDL starts the query on a cache miss, parks it inside the
// catalog read, runs ddl to completion, releases the compile and joins it.
func raceCompileAgainstDDL(t *testing.T, eng *cypher.Engine, gate *gatedHashSub, ddl string) {
	t.Helper()
	gate.armed.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := eng.Run(context.Background(), ddlWindowQuery, map[string]expr.Value{"p": expr.IntegerValue(1)})
		if err == nil {
			_ = res.Close()
		}
	}()
	select {
	case <-gate.reached:
	case <-time.After(ddlWindowWait):
		close(gate.release)
		<-done
		t.Fatal("the compilation never read the gated index: the fixture does not reach the catalog read")
	}
	ddlWindowExec(t, eng, ddl)
	close(gate.release)
	select {
	case <-done:
	case <-time.After(ddlWindowWait):
		t.Fatal("the compiling query did not finish after release")
	}
}

func TestPlanCache_CompileOverlappingDDLDoesNotPublishStaleEntry(t *testing.T) {
	t.Run("CREATE INDEX", func(t *testing.T) {
		const ddl = "CREATE INDEX FOR (n:P) ON (n.name)"
		// Control: the DDL, then the query, with no interleaving. The gated
		// subscriber is registered here too, unarmed, so both engines hold the
		// same catalog.
		control, controlMgr := ddlWindowEngine(t)
		if err := controlMgr.CreateIndex("gate_hash", newGatedHashSub(0)); err != nil {
			t.Fatalf("register gate: %v", err)
		}
		if ddlWindowVerdict(t, control) {
			t.Fatal("control premise: before the index the integer parameter must be accepted")
		}
		ddlWindowExec(t, control, ddl)
		want := ddlWindowVerdict(t, control)
		if !want {
			t.Fatal("control premise: after CREATE INDEX over string data the integer parameter must be rejected")
		}

		eng, mgr := ddlWindowEngine(t)
		gate := newGatedHashSub(0)
		if err := mgr.CreateIndex("gate_hash", gate); err != nil {
			t.Fatalf("register gate: %v", err)
		}
		raceCompileAgainstDDL(t, eng, gate, ddl)
		if got := ddlWindowVerdict(t, eng); got != want {
			t.Fatalf("after CREATE INDEX the integer parameter is rejected=%v, want %v: a plan compiled "+
				"from the pre-DDL catalog was published after the DDL cleared the plan cache", got, want)
		}
	})

	t.Run("DROP INDEX", func(t *testing.T) {
		const ddl = "DROP INDEX p_name_hash"
		// The gated subscriber IS the index the DDL drops: while registered it
		// proves the property String, once dropped nothing does.
		control, controlMgr := ddlWindowEngine(t)
		if err := controlMgr.CreateIndex("p_name_hash", newGatedHashSub(1)); err != nil {
			t.Fatalf("register gate: %v", err)
		}
		if !ddlWindowVerdict(t, control) {
			t.Fatal("control premise: with the index the integer parameter must be rejected")
		}
		ddlWindowExec(t, control, ddl)
		want := ddlWindowVerdict(t, control)
		if want {
			t.Fatal("control premise: after DROP INDEX the integer parameter must be accepted")
		}

		eng, mgr := ddlWindowEngine(t)
		gate := newGatedHashSub(1)
		if err := mgr.CreateIndex("p_name_hash", gate); err != nil {
			t.Fatalf("register gate: %v", err)
		}
		raceCompileAgainstDDL(t, eng, gate, ddl)
		if got := ddlWindowVerdict(t, eng); got != want {
			t.Fatalf("after DROP INDEX the integer parameter is rejected=%v, want %v: a plan compiled "+
				"from the pre-DDL catalog was published after the DDL cleared the plan cache", got, want)
		}
	})
}
