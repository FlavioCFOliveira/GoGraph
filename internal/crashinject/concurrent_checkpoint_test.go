//go:build gograph_crashinject

package crashinject_test

// concurrent_checkpoint_test.go — rmp #2310, acceptance criterion 4.
//
// # What this covers that the other checkpoint crash tests do not
//
// graph_shape_test.go crashes a checkpoint whose workload had already finished
// committing, so the capture ran with no concurrent writer. Since rmp #2310 that is
// no longer the shape production runs: phase 1 holds the commit lock only long
// enough to read the durable WAL offset and open an MVCC snapshot, and the whole
// image is serialised outside the lock while transactions commit.
//
// A crash landing after that snapshot has been published and made durable, but
// BEFORE the WAL prefix it authorises discarding has been truncated, is the one that
// can expose a torn image: the artefact recovery will read is already on disk, and
// the WAL that could contradict it is still there to prove it wrong.
//
// # The oracle
//
// With concurrent writers there is no hand-computable transaction COUNT — which
// transactions had committed when the kill landed is up to the scheduler. There is a
// hand-computable SHAPE. Each transaction commits exactly two fresh nodes and the one
// arc between them, so:
//
//   - ATOMICITY/TEARING — a transaction is present with all three of its facts or
//     with none. Two endpoints without their arc is precisely the torn image this
//     task exists to prevent, and it shows up here as a partial transaction.
//   - DURABILITY — every transaction the child was acknowledged for is present,
//     complete.
//   - NO INVENTION — every live node and every arc belongs to some transaction in
//     the universe.
//   - THE PAIR IDENTITY — Order == 2*Size over the whole recovered graph, which is
//     the same absolute oracle store/checkpoint's capture_atomicity_test.go applies
//     to the non-crashing path.
//
// The file is compiled only under the gograph_crashinject build tag.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// The workload the parent commissions, passed to the child through the environment
// so the parent remains the single authority on the universe of transaction ids.
const (
	ccpWriters   = 4
	ccpWarmup    = 8
	ccpPerWriter = 400
)

// ccpUniverse is every transaction id the child may commit: the warm-up ids
// 1..ccpWarmup followed by ccpWriters disjoint ranges of ccpPerWriter each.
const ccpUniverse = ccpWarmup + ccpWriters*ccpPerWriter

// ccpSkipHits is how many hits of the breakpoint pass before the self-kill
// (GOGRAPH_CRASH_AFTER). The breakpoint fires once per CHECKPOINT, and the child
// triggers checkpoints back to back for the whole workload, so this must be large
// enough to land well past the sequential warm-up and small enough that the
// workload cannot finish first. Overshooting is not silent: the child then exits
// cleanly and runConcurrentCheckpointCrash fails, naming the countdown.
const ccpSkipHits = 6

// ccpTxnFacts is everything transaction id commits, hand-derived here from the
// scenario documented on cmd/crashinject-helper's runConcurrentCheckpointCrash
// rather than imported from it, so a change on either side has to be reconciled
// deliberately: two fresh nodes base and base+1, and the arc between them carrying
// weight id.
//
// The stride of 2 covers the id space with no gaps, so a recovered node belonging to
// no transaction is detectable rather than absorbed into an unused range.
func ccpTxnFacts(id int64) (src, dst, weight int64) {
	base := id * 2
	return base, base + 1, id
}

// ccpFactCount is the number of independently observable facts one transaction
// commits: its two nodes and its one arc.
const ccpFactCount = 3

// ccpPresentFacts counts how many of transaction id's facts the recovered graph
// carries. A weight mismatch counts the arc as ABSENT rather than
// present-but-wrong, for the same reason concPresentFacts does: either way the
// transaction did not land as committed.
func ccpPresentFacts(g *lpg.Graph[int64, int64], id int64) int {
	src, dst, weight := ccpTxnFacts(id)
	n := 0
	if _, ok := g.OutDegree(src); ok {
		n++
	}
	if _, ok := g.OutDegree(dst); ok {
		n++
	}
	for d, w := range g.AdjList().Neighbours(src) {
		if d == dst && w == weight {
			n++
			break
		}
	}
	return n
}

// checkConcurrentCheckpoint is the oracle. It returns one message per violated
// property and an empty slice when the recovered graph honours all of them.
//
// acked is the set of transaction ids the child announced as acknowledged; universe
// is every id it could possibly have committed.
func checkConcurrentCheckpoint(g *lpg.Graph[int64, int64], universe int64, acked map[int64]bool) []string {
	var violations []string
	var accountedNodes, accountedArcs int64
	for id := int64(1); id <= universe; id++ {
		src, dst, _ := ccpTxnFacts(id)
		switch n := ccpPresentFacts(g, id); n {
		case ccpFactCount:
			// Whole.
		case 0:
			// Absent — legitimate for an unacknowledged transaction.
			if acked[id] {
				violations = append(violations, fmt.Sprintf(
					"DURABILITY: transaction %d was acknowledged but is entirely absent after recovery", id))
			}
		default:
			// This is the TORN case. Two endpoints and no arc (n == 2) is exactly the
			// partial transaction a capture that read its components at different
			// instants folds into the image.
			violations = append(violations, fmt.Sprintf(
				"ATOMICITY: transaction %d is PARTIAL after recovery — %d of %d facts present "+
					"(nodes %d,%d and their arc must appear together or not at all)",
				id, n, ccpFactCount, src, dst))
			if acked[id] {
				violations = append(violations, fmt.Sprintf(
					"DURABILITY: transaction %d was acknowledged but recovered incomplete", id))
			}
		}
		for _, k := range [2]int64{src, dst} {
			if _, ok := g.OutDegree(k); ok {
				accountedNodes++
			}
		}
		if g.AdjList().HasEdge(src, dst) {
			accountedArcs++
		}
	}
	// Recovery must invent nothing.
	if got := int64(g.AdjList().Size()); got != accountedArcs {
		violations = append(violations, fmt.Sprintf(
			"arc count = %d, but only %d arcs belong to a transaction in the universe", got, accountedArcs))
	}
	if got := int64(g.LiveOrder()); got != accountedNodes {
		violations = append(violations, fmt.Sprintf(
			"live node count = %d, but only %d nodes belong to a transaction in the universe",
			got, accountedNodes))
	}
	// THE PAIR IDENTITY, over the whole graph rather than per transaction. It is
	// implied by the per-transaction checks above when the universe is fully
	// enumerated, and it is asserted anyway because it is the one line that states
	// the property in the form the capture can violate: an image holding an odd
	// number of nodes per arc came from more than one instant.
	if order, size := int64(g.LiveOrder()), int64(g.AdjList().Size()); order != 2*size {
		violations = append(violations, fmt.Sprintf(
			"PARTIAL TRANSACTION: recovered Order=%d, Size=%d, want Order == 2*Size (%d) — "+
				"every transaction contributes exactly two nodes and one arc, so this graph "+
				"holds endpoints whose arc it lost or an arc whose endpoints it lost",
			order, size, 2*size))
	}
	return violations
}

// ccpOpts builds the child environment for one concurrent-checkpoint run.
func ccpOpts(skipHits int) crashinject.Opts {
	return crashinject.Opts{
		Env: []string{
			"GOGRAPH_CRASH_WORKLOAD=checkpoint-concurrent",
			"GOGRAPH_CRASH_WRITERS=" + strconv.Itoa(ccpWriters),
			"GOGRAPH_CRASH_WARMUP=" + strconv.Itoa(ccpWarmup),
			"GOGRAPH_CRASH_PERWRITER=" + strconv.Itoa(ccpPerWriter),
			"GOGRAPH_CRASH_AFTER=" + strconv.Itoa(skipHits),
		},
		// Thousands of durable commits plus back-to-back checkpoints; the package
		// default of 30 s is sized for scenarios that commit a handful.
		Timeout: 180 * time.Second,
	}
}

// runConcurrentCheckpointCrash runs the scenario, asserts the child was really
// SIGKILL'd at the breakpoint and that the crash landed after real acknowledged
// work, and returns the artefact directory plus the acknowledged set.
func runConcurrentCheckpointCrash(t *testing.T, scenario string, skipHits int) (string, map[int64]bool) {
	t.Helper()
	out, err := crashinject.Run(t, scenario, ccpOpts(skipHits))
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if out.TimedOut {
		t.Fatalf("child timed out instead of crashing at %s\nstderr: %s", scenario, out.Stderr)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s (exit code %d) — the countdown (%d) outlived the "+
			"workload, so nothing was crashed and the assertions below would be vacuous\nstderr: %s",
			scenario, out.ExitCode, skipHits, out.Stderr)
	}
	acked := parseAcks(t, out.Stdout)
	if len(acked) < ccpWarmup {
		t.Fatalf("%s: only %d acknowledged commits before the crash, want at least the %d warm-up "+
			"commits — the crash landed too early for the durability assertion to mean "+
			"anything\nstderr: %s", scenario, len(acked), ccpWarmup, out.Stderr)
	}
	t.Logf("%s: child SIGKILL'd after %d acknowledged commits (countdown %d)",
		scenario, len(acked), skipHits)
	return out.Dir, acked
}

// TestCrashRecovery_ConcurrentCheckpoint_PrePrefixTruncate crashes the child inside a
// checkpoint that is running CONCURRENTLY with four committing writers, after the
// self-sufficient snapshot has been published and fsynced but before the WAL prefix
// is truncated.
//
// Recovery therefore reads a durable snapshot taken at an MVCC instant, plus the
// entire un-truncated WAL, and must reconstruct a state in which every transaction is
// whole and every acknowledged one is present.
func TestCrashRecovery_ConcurrentCheckpoint_PrePrefixTruncate(t *testing.T) {
	const scenario = "checkpoint.p2-snapshot-published-pre-truncate"
	dir, acked := runConcurrentCheckpointCrash(t, scenario, ccpSkipHits)
	g := recoverGraph(t, dir)
	if v := checkConcurrentCheckpoint(g, ccpUniverse, acked); len(v) > 0 {
		t.Errorf("%s: recovery violated the ACID contract:\n%s", scenario, strings.Join(v, "\n"))
	}
	t.Logf("%s: recovered %d nodes / %d arcs, %d acknowledged transactions all whole",
		scenario, g.LiveOrder(), g.AdjList().Size(), len(acked))
}

// TestCrashRecovery_ConcurrentCheckpoint_OracleReportsTearing is the checker's own
// negative control: it feeds checkConcurrentCheckpoint a graph carrying a
// deliberately TORN transaction — two endpoints whose arc is missing, which is
// exactly what a capture reading its components at two instants produces — and
// asserts the oracle reports it.
//
// A checker that has never been seen to fail is not evidence. This runs in the same
// build as the crash test and needs no child process.
func TestCrashRecovery_ConcurrentCheckpoint_OracleReportsTearing(t *testing.T) {
	g := lpg.New[int64, int64](adjlist.Config{Directed: true})
	defer func() { _ = g.Close() }()

	// Two whole transactions.
	acked := map[int64]bool{1: true, 2: true, 3: true}
	for _, id := range []int64{1, 2} {
		src, dst, w := ccpTxnFacts(id)
		if err := g.AddEdge(src, dst, w); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
	}
	// Transaction 3, TORN: its endpoints exist, its arc does not.
	src3, dst3, _ := ccpTxnFacts(3)
	if err := g.AddNode(src3); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.AddNode(dst3); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	v := checkConcurrentCheckpoint(g, ccpUniverse, acked)
	if len(v) == 0 {
		t.Fatal("the oracle reported no violation for a graph holding a transaction's two " +
			"endpoints without its arc — it cannot detect the tear it exists to detect, so " +
			"TestCrashRecovery_ConcurrentCheckpoint_PrePrefixTruncate proves nothing")
	}
	joined := strings.Join(v, "\n")
	for _, want := range []string{"ATOMICITY: transaction 3", "PARTIAL TRANSACTION"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the oracle fired but did not report %q; it said:\n%s", want, joined)
		}
	}
	t.Logf("oracle correctly reported the injected tear:\n%s", joined)
}
