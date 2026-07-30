package cypher

// proc_write_path_test.go — regression gate for rmp #2229.
//
// # The defect
//
// [buildOperatorWrite]'s fall-through branch handed [buildOperator] a literal
// nil where the procedure registry belongs, with a comment recording that
// [buildPlanWithMutatorFull] "does not thread procReg". It did not, so a
// `CALL db.*` inside any statement the classifier deemed a write could not be
// PLANNED at all:
//
//	Run / RunAny          →  CALL db.labels() succeeds
//	RunInTx / RunInTxAny  →  cypher: build plan: … procedure not found: db.labels
//
// The sharp edge was that this reached read-only statements too. Because
// [QueryHasWritingClause] applies its keyword regexp to the whole query text
// including comments and string literals (rmp #2230, tracked separately), a
// comment that merely mentions a writing keyword reclassifies a read as a write
// — and the misrouted read then hit this defect. Measured before the fix:
//
//	CALL db.labels() YIELD label RETURN label                          →  succeeds
//	// CREATE nothing here⏎CALL db.labels() YIELD label RETURN label   →  FAILS
//
// A working query broken by adding a comment to it.
//
// # What this file asserts
//
// Parity, enumerated from the registry rather than from a hand-written list, so
// a procedure added to one path only fails here. Plus the two shapes the
// enumeration cannot express: a statement that genuinely mixes a write clause
// with a CALL, and the unknown-procedure error, which must stay identical on
// both paths.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// procParityEngine seeds a graph with enough labels, relationship types,
// properties, an index and a constraint that every introspection procedure has
// something to report — a procedure returning zero rows on both paths would
// make the comparison vacuous.
func procParityEngine(t *testing.T) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(n, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(n, "sid", lpg.StringValue(n)); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("a", "b", "K")

	eng := NewEngine(g)
	for _, ddl := range []string{
		`CREATE INDEX p_sid FOR (x:P) ON (x.sid)`,
		`CREATE CONSTRAINT p_sid_unique FOR (x:P) REQUIRE x.sid IS UNIQUE`,
	} {
		// Drain AND close: an abandoned Result is collected by a later forced GC
		// and counted by TestResult_Close_DisarmsFinalizer, which samples the
		// process-global leak counter across its own GC. A fixture that discards
		// its Results fails that test from a different file.
		res, err := eng.RunAny(context.Background(), ddl, nil)
		if err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
		for res.Next() {
		}
		if err := res.Err(); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
		if err := res.Close(); err != nil {
			t.Fatalf("%s: close: %v", ddl, err)
		}
	}
	return eng
}

// procParityRun executes q through the chosen entry point and returns the rows
// rendered as comparable strings, or the error text.
func procParityRun(t *testing.T, eng *Engine, q string, inTx bool) (rows []string, errText string) {
	t.Helper()
	var (
		res *Result
		err error
	)
	if inTx {
		res, err = eng.RunInTxAny(context.Background(), q, nil)
	} else {
		res, err = eng.RunAny(context.Background(), q, nil)
	}
	if err != nil {
		return nil, err.Error()
	}
	for res.Next() {
		var b strings.Builder
		for i := range res.Columns() {
			fmt.Fprintf(&b, "%v\x1f", res.ValueAt(i))
		}
		rows = append(rows, b.String())
	}
	if drainErr := res.Err(); drainErr != nil {
		errText = drainErr.Error()
	}
	if closeErr := res.Close(); closeErr != nil && errText == "" {
		errText = closeErr.Error()
	}
	return rows, errText
}

// TestProcWritePathParity_EveryRegisteredProcedure is the #2229 gate. It
// enumerates the Engine's own registry, so a procedure registered in a future
// change is covered without editing this test — which is the point: the defect
// was a path that silently resolved nothing.
//
// Each path gets its OWN engine over an identical fixture, rather than sharing
// one. That is not tidiness: a procedure may have side effects, and comparing a
// second call against a first would then measure the side effect instead of the
// path. db.stats.refresh is exactly that case — it is rate-limited to one
// rebuild per 30 s, so on a shared engine the read path returns
// `true / "planner statistics rebuilt"` and the write path
// `false / "refused: … retry in 30s"`, a difference that says nothing about
// #2229. Two fixtures make the comparison mean what it claims to mean.
func TestProcWritePathParity_EveryRegisteredProcedure(t *testing.T) {
	sigs := procParityEngine(t).Procs().List()
	if len(sigs) == 0 {
		t.Fatal("the registry is empty, so this test would pass vacuously")
	}
	var covered int
	for _, sig := range sigs {
		fqn := sig.Name
		if len(sig.Namespace) > 0 {
			fqn = strings.Join(sig.Namespace, ".") + "." + sig.Name
		}
		t.Run(fqn, func(t *testing.T) {
			if len(sig.Inputs) > 0 {
				// A procedure taking arguments needs values this test cannot
				// invent; skipping silently would hide a coverage hole, so say so.
				t.Skipf("procedure takes %d argument(s); parity for argument-taking "+
					"procedures needs a per-procedure fixture", len(sig.Inputs))
			}
			q := buildYieldQuery(fqn, &sig)

			readRows, readErr := procParityRun(t, procParityEngine(t), q, false)
			writeRows, writeErr := procParityRun(t, procParityEngine(t), q, true)

			if readErr != "" {
				t.Fatalf("read path failed, so the write path has nothing to be compared "+
					"against: %s\n  query: %s", readErr, q)
			}
			if writeErr != "" {
				t.Fatalf("write path failed where the read path succeeded — #2229 has "+
					"regressed: %s\n  query: %s", writeErr, q)
			}
			if len(readRows) != len(writeRows) {
				t.Fatalf("row count differs: read %d, write %d\n  query: %s",
					len(readRows), len(writeRows), q)
			}
			for i := range readRows {
				if readRows[i] != writeRows[i] {
					t.Fatalf("row %d differs:\n  read  %q\n  write %q\n  query: %s",
						i, readRows[i], writeRows[i], q)
				}
			}
			covered++
		})
	}
	t.Logf("%d registered procedures, %d compared on both paths", len(sigs), covered)
}

// buildYieldQuery renders a zero-argument CALL that yields every declared output
// column and returns them, so the comparison covers the procedure's whole result
// shape rather than just its first column.
func buildYieldQuery(fqn string, sig *procs.Signature) string {
	if len(sig.Outputs) == 0 {
		return fmt.Sprintf("CALL %s()", fqn)
	}
	names := make([]string, 0, len(sig.Outputs))
	for _, o := range sig.Outputs {
		names = append(names, "`"+o.Name+"`")
	}
	cols := strings.Join(names, ", ")
	return fmt.Sprintf("CALL %s() YIELD %s RETURN %s", fqn, cols, cols)
}

// TestProcWritePath_GenuineWriteMixedWithACall covers the shape the enumeration
// cannot: a statement that really does write AND calls a procedure. Before
// #2229 it could not be planned on any entry point, because a write clause
// routes it to the write path by construction rather than by misclassification.
func TestProcWritePath_GenuineWriteMixedWithACall(t *testing.T) {
	eng := procParityEngine(t)
	const stmt = `CALL db.labels() YIELD label CREATE (:Marker {l: label})`

	if _, errText := procParityRun(t, eng, stmt, false); errText != "" {
		t.Fatalf("RunAny: %s", errText)
	}
	rows, errText := procParityRun(t, eng, `MATCH (m:Marker) RETURN m.l ORDER BY m.l`, false)
	if errText != "" {
		t.Fatalf("read back: %s", errText)
	}
	// The fixture interns exactly :P and :Marker — but :Marker only exists once
	// this statement has run, so the label list it iterated held :P alone.
	if len(rows) != 1 {
		t.Fatalf("expected one :Marker per label reported by db.labels(), got %d rows: %v", len(rows), rows)
	}

	// The same statement inside an explicit transaction must plan too.
	if _, errText := procParityRun(t, procParityEngine(t), stmt, true); errText != "" {
		t.Fatalf("RunInTxAny: %s", errText)
	}
}

// TestProcWritePath_CommentDoesNotBreakACall pins reproduction B: a leading
// comment that merely MENTIONS a write keyword must not break an otherwise
// working read-only CALL.
//
// It used to skip when the classifier stopped reading comments, with a message
// saying the case "can be removed". #2230 landed, so the skip became permanent —
// a test retired by a skip rather than by a decision. That was the wrong shape
// (rmp #2261): the PREMISE went away, but the ASSERTION did not. Whether the
// classifier routes this statement to the write path or the read path is an
// implementation detail; that adding a comment must not change the ANSWER is the
// property worth pinning, and it is now checked unconditionally.
func TestProcWritePath_CommentDoesNotBreakACall(t *testing.T) {
	eng := procParityEngine(t)
	const plain = `CALL db.labels() YIELD label RETURN label`
	const commented = "// CREATE nothing here\n" + plain

	plainRows, plainErr := procParityRun(t, eng, plain, false)
	if plainErr != "" {
		t.Fatalf("uncommented control failed: %s", plainErr)
	}
	gotRows, gotErr := procParityRun(t, eng, commented, false)
	if gotErr != "" {
		t.Fatalf("adding a comment broke a working query — #2229 has regressed: %s", gotErr)
	}
	if len(gotRows) != len(plainRows) {
		t.Fatalf("commented form returned %d rows, uncommented %d", len(gotRows), len(plainRows))
	}
}

// TestProcWritePath_ConcurrentRegistrationAndPlanBuild proves the registry can
// be shared, rather than snapshotted per query, without a data race.
//
// The task asked whether the queryReg/labelSrc-style per-query snapshot applies
// to procedures too. It does not need to: [procs.Registry] guards its map with
// an RWMutex, so a Register concurrent with a plan build's Lookup is safe at the
// data level, and each Lookup is individually consistent. What a shared registry
// does NOT give is a single consistent view across two lookups in one plan build
// — but the READ path has had exactly that property since #1506 and it has never
// been a defect, because a plan that resolved a procedure keeps the entry it
// resolved. Sharing therefore matches the read path rather than diverging from it.
//
// Run under -race this fails if that reasoning is wrong.
func TestProcWritePath_ConcurrentRegistrationAndPlanBuild(t *testing.T) {
	eng := procParityEngine(t)

	const (
		builders   = 8
		registrars = 4
		iterations = 40
	)
	var wg sync.WaitGroup

	for w := 0; w < builders; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Alternate the two entry points so the write-path builder (the one
			// #2229 changed) is exercised alongside the read path.
			for i := 0; i < iterations; i++ {
				inTx := (w+i)%2 == 0
				var (
					res *Result
					err error
				)
				const q = `CALL db.labels() YIELD label RETURN label`
				if inTx {
					res, err = eng.RunInTxAny(context.Background(), q, nil)
				} else {
					res, err = eng.RunAny(context.Background(), q, nil)
				}
				if err != nil {
					t.Errorf("worker %d iteration %d (inTx=%v): %v", w, i, inTx, err)
					return
				}
				for res.Next() {
				}
				_ = res.Close()
			}
		}(w)
	}

	for r := 0; r < registrars; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				sig := procs.Signature{
					Namespace: []string{"test"},
					Name:      fmt.Sprintf("p%d_%d", r, i),
					Outputs:   []procs.NamedType{{Name: "v", Kind: expr.KindInteger}},
				}
				// A duplicate name is an ordinary error, not a failure of this test;
				// the point is that Register races the concurrent plan builds.
				_ = eng.Procs().Register(sig, func(context.Context, []expr.Value) ([][]expr.Value, error) {
					return [][]expr.Value{{expr.IntegerValue(1)}}, nil
				})
			}
		}(r)
	}

	wg.Wait()
}

// TestProcWritePath_UnknownProcedureFailsIdenticallyOnBothPaths guards the other
// direction: the fix must not make the write path resolve something the read
// path rejects, and both must report the same error.
func TestProcWritePath_UnknownProcedureFailsIdenticallyOnBothPaths(t *testing.T) {
	eng := procParityEngine(t)
	const stmt = `CALL db.nosuchprocedure() YIELD x RETURN x`

	_, readErr := procParityRun(t, eng, stmt, false)
	_, writeErr := procParityRun(t, eng, stmt, true)

	if readErr == "" {
		t.Fatal("the read path resolved a procedure that does not exist")
	}
	if writeErr == "" {
		t.Fatal("the write path resolved a procedure that does not exist")
	}
	if readErr != writeErr {
		t.Fatalf("the two paths report different errors for the same unknown procedure:\n"+
			"  read  %s\n  write %s", readErr, writeErr)
	}
	if !strings.Contains(readErr, "procedure not found") {
		t.Fatalf("expected an ErrProcNotFound-derived message, got: %s", readErr)
	}
}
