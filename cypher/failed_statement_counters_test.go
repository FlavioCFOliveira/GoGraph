package cypher_test

// failed_statement_counters_test.go — regression gate for rmp #2823: a statement
// that FAILED must report no write-effect counters.
//
// # What was wrong
//
// `MERGE (m:X {key:'new'}) ON CREATE SET m.other = b`, with b bound to a node,
// raises InvalidPropertyType (the refusal rmp #2816 added) and is rolled back:
// the node it created inside the MERGE is undone, nothing is visible and nothing
// is durable. Yet the Result it handed back reported nodesCreated: 1,
// labelsAdded: 1 and propertiesSet: 1 — the effects applied EAGERLY before the
// error, which the undo log then reversed. The stored data was never wrong; the
// REPORT of it was, and ContainsUpdates answered true for a statement that
// contained no update.
//
// It is not a MERGE defect. Every shape that applies something and then fails
// reported the same way, so the table below drives three independent routes into
// the one rollback: MERGE's ON CREATE SET, a multi-assignment SET whose first
// assignment succeeds, and a CREATE followed by a failing SET on the node it
// just created.
//
// # What settled the intended behaviour
//
//   - The openCypher TCK does not pin it. Of the 3196 scenario blocks in
//     cypher/tck/features, 192 carry a `should be raised` step and 1423 carry a
//     side-effect step (`Then the side effects should be` / `And no side
//     effects`) — and the intersection is EMPTY. The TCK never states what a
//     failing statement's side effects are, so no baseline moves either way.
//   - Bolt cannot express it: a statement that fails mid-stream terminates with
//     a FAILURE message, which carries only a code and a message — there is no
//     stats field on it (bolt/proto.Failure), and bolt/server/session.go emits
//     exactly that. So no Bolt client of ANY server — this one, Neo4j, Memgraph —
//     has ever seen counters for a failed statement.
//   - The reference driver agrees: neo4j-go-driver v5.28.4 returns (nil, err)
//     from resultWithContext.Consume when the query failed, so the ResultSummary
//     that carries Counters() does not exist for a failed query.
//   - The module had already written the contract down: cypher.Result.Counters'
//     godoc asserted that "a statement that failed or rolled back never produces
//     a Result to report from". Every other failure path really does hand back no
//     Result — ExplicitTx.Exec returns the error alone; RunInTx does the same for
//     a WAL-commit, conflict or NOT NULL failure — leaving the autocommit
//     drain-error path as the single exception, which is this defect.
//
// # The fix, and what it must not silence
//
// Result.rollbackUnderBarrier drops the counter set alongside the effects it
// undoes, so the report and the graph agree by construction. The controls at the
// end are what keeps this honest: a SUCCEEDING write must still report its
// effects, a read must still report nil, and the statement AFTER a failure must
// still report its own effects in full.
//
// Layer: short.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// failedCountersSeed is the fixture every case starts from: two :FC nodes, so a
// node is bindable as the illegal property value and a second node is available
// as a write target.
const failedCountersSeed = `CREATE (a:FC {key:'a'}), (b:FC {key:'b'})`

// failingWriteCase is one statement that must fail, with the effects the
// pre-#2823 engine reported for it. preFix is not asserted — it is the record of
// what the defect looked like, so a reader can reproduce it by reverting the one
// line in rollbackUnderBarrier.
type failingWriteCase struct {
	name    string
	query   string
	preFix  string // counters the defective build reported
	wantErr string // substring the raised error must contain
}

var failingWriteCases = []failingWriteCase{
	{
		name:    "MergeOnCreateSet",
		query:   `MATCH (b:FC {key:'b'}) MERGE (m:FC {key:'new'}) ON CREATE SET m.other = b`,
		preFix:  "nodesCreated=1 labelsAdded=1 propertiesSet=1",
		wantErr: "InvalidPropertyType",
	},
	{
		name:    "MultiAssignmentSet",
		query:   `MATCH (b:FC {key:'b'}) MATCH (a:FC {key:'a'}) SET a.tag = 7, a.other = b`,
		preFix:  "propertiesSet=1",
		wantErr: "InvalidPropertyType",
	},
	{
		name:    "CreateThenFailingSet",
		query:   `CREATE (z:FC {key:'z'}) WITH z MATCH (b:FC {key:'b'}) SET z.other = b`,
		preFix:  "nodesCreated=1 labelsAdded=1 propertiesSet=1",
		wantErr: "InvalidPropertyType",
	},
}

// TestFailedStatementReportsNoCounters_InMemory drives the store-less engine.
func TestFailedStatementReportsNoCounters_InMemory(t *testing.T) {
	t.Parallel()
	runFailedCounterCases(t, newFailedCountersEngine(t, nil))
}

// TestFailedStatementReportsNoCounters_WALStore drives the WAL-backed engine,
// whose rollback additionally rolls the store transaction back. The counter
// drop is on the shared path, so both wirings must show it.
func TestFailedStatementReportsNoCounters_WALStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	runFailedCounterCases(t, newFailedCountersEngine(t, w))
}

// newFailedCountersEngine returns a seeded engine, WAL-backed when w is non-nil.
func newFailedCountersEngine(t *testing.T, w *wal.Writer) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	var eng *cypher.Engine
	if w == nil {
		eng = cypher.NewEngine(g)
	} else {
		eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w,
			txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			}))
	}
	if _, _, err := runFailable(eng, failedCountersSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return eng
}

// runFailable executes a statement and returns its counters and its error,
// whether raised at plan time or during iteration.
func runFailable(eng *cypher.Engine, query string) (*exec.QueryCounters, int, error) {
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		return nil, 0, err
	}
	rows := 0
	for res.Next() {
		rows++
	}
	iterErr := res.Err()
	c := res.Counters()
	if cerr := res.Close(); cerr != nil && iterErr == nil {
		iterErr = cerr
	}
	return c, rows, iterErr
}

// runReadCounted executes a statement on the engine's READ path
// ([cypher.Engine.Run]) and returns its counters and row count. The read path is
// what reports nil counters for a statement with no write surface; RunInTx builds
// a write mutator whatever the statement is, so a read routed through it reports
// a non-nil all-zero set (cypher: TestQueryCounters_ReadOnlyReportsNil).
func runReadCounted(eng *cypher.Engine, query string) (*exec.QueryCounters, int, error) {
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		return nil, 0, err
	}
	rows := 0
	for res.Next() {
		rows++
	}
	iterErr := res.Err()
	c := res.Counters()
	if cerr := res.Close(); cerr != nil && iterErr == nil {
		iterErr = cerr
	}
	return c, rows, iterErr
}

// nodeCount returns how many :FC nodes the engine can see, the observable the
// counter report is cross-checked against.
func nodeCount(t *testing.T, eng *cypher.Engine) int {
	t.Helper()
	_, rows, err := runReadCounted(eng, `MATCH (n:FC) RETURN n`)
	if err != nil {
		t.Fatalf("MATCH (n:FC): %v", err)
	}
	return rows
}

// runFailedCounterCases drives every case in [failingWriteCases] plus the
// controls that keep the gate from passing by silencing everything.
func runFailedCounterCases(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	for _, tc := range failingWriteCases {
		t.Run(tc.name, func(t *testing.T) {
			before := nodeCount(t, eng)
			c, _, err := runFailable(eng, tc.query)
			after := nodeCount(t, eng)

			// NON-VACUITY: the statement really has to fail, or the assertion
			// below would hold for the trivial reason that nothing happened.
			if err == nil {
				t.Fatalf("%s did not fail; this case exists because it must raise %s", tc.query, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s raised %v, want an error containing %q", tc.query, err, tc.wantErr)
			}
			// The rollback itself is pre-existing behaviour and stays correct;
			// asserting it is what makes the counter claim meaningful, because
			// "no effects applied" is precisely what "no counters" must report.
			if after != before {
				t.Fatalf("%s changed the node count from %d to %d; the statement must roll back",
					tc.query, before, after)
			}
			if c != nil {
				t.Errorf("%s reported counters %+v on a failed, rolled-back statement; want none "+
					"(the defective build reported %s)", tc.query, *c, tc.preFix)
			}
			if c.ContainsUpdates() {
				t.Errorf("%s reported ContainsUpdates although it applied nothing", tc.query)
			}
		})
	}

	t.Run("ControlSucceedingWriteStillReports", func(t *testing.T) {
		c, _, err := runFailable(eng, `CREATE (:FC {key:'ok', n: 1})`)
		if err != nil {
			t.Fatalf("control write: %v", err)
		}
		if c == nil {
			t.Fatal("a successful write reported nil counters; the fix must not silence the true case")
		}
		if c.NodesCreated != 1 || c.LabelsAdded != 1 || c.PropertiesSet != 2 {
			t.Errorf("control write reported %+v, want +nodes 1 +labels 1 +properties 2", *c)
		}
		if !c.ContainsUpdates() {
			t.Error("a successful write reported ContainsUpdates = false")
		}
	})

	t.Run("ControlReadStillReportsNil", func(t *testing.T) {
		c, _, err := runReadCounted(eng, `MATCH (n:FC) RETURN n.key`)
		if err != nil {
			t.Fatalf("control read: %v", err)
		}
		if c != nil {
			t.Errorf("a read reported counters %+v, want nil", *c)
		}
	})

	t.Run("ControlStatementAfterFailureReportsItsOwn", func(t *testing.T) {
		if _, _, err := runFailable(eng,
			`MATCH (b:FC {key:'b'}) MATCH (a:FC {key:'a'}) SET a.other = b`); err == nil {
			t.Fatal("the seeding failure did not fail")
		}
		c, _, err := runFailable(eng, `CREATE (:FC {key:'after'})`)
		if err != nil {
			t.Fatalf("statement after a failure: %v", err)
		}
		if c == nil || c.NodesCreated != 1 || c.LabelsAdded != 1 || c.PropertiesSet != 1 {
			t.Errorf("statement after a failure reported %v, want exactly its own effects", c)
		}
	})
}
