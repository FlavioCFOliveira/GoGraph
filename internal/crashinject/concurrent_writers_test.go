//go:build gograph_crashinject

package crashinject_test

// concurrent_writers_test.go — rmp #2302, acceptance criterion 5.
//
// Every other test in this package crashes a SINGLE-threaded child and asserts
// one hand-computed graph shape. That cannot be done with concurrent writers:
// which transactions had committed when the kill landed is up to the scheduler,
// so there is no single post-crash shape to write down.
//
// What is not up to the scheduler is the contract. So the child announces every
// acknowledged commit on stdout before it dies (an ACK line per commit, written
// only after Commit returned nil, i.e. after that transaction's frames and its
// OpCommit marker were fsynced), and these tests hold recovery to two
// properties over the surviving artefacts:
//
//   - DURABILITY — every acknowledged transaction is present after recovery,
//     complete. This is the property audit finding E5 breaks: with interleaved
//     appends, recovery's orphan filter discards frames whose TxnSeq differs
//     from the commit marker it is buffering against, silently dropping
//     COMMITTED data. Its only production symptom is a counter.
//   - ATOMICITY — every transaction present after recovery is present in FULL.
//     A transaction that contributed some of its five facts and not others is a
//     violation whether or not it was ever acknowledged.
//
// Plus two closing conditions: recovery invents nothing (the live node and arc
// totals must be fully accounted for by the universe of transaction ids), and the crash
// landed after real acknowledged work (a crash before the first acknowledgement
// would satisfy both properties vacuously — see GOGRAPH_CRASH_AFTER).
//
// The checker is a pure function returning violations, so the same code proves
// the assertions hold after a real crash AND, in
// TestConcurrentWritersOracle_ReportsViolations, that it can actually fail on
// both arms. A checker that has never been seen to fail is not evidence.
//
// The file is compiled only under the gograph_crashinject build tag: without it
// the WAL's crashpoints are elided no-ops, the child runs its whole workload and
// is never killed.

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/crashinject"
)

// The workload the parent commissions. These are passed to the child through
// the environment, so the parent is the single authority on the universe of
// transaction ids and the two sides cannot drift.
//
// concWriters is past the machine's core count on purpose: the point is to have
// more transactions in flight than there are cores, so appends and group-commit
// fsyncs genuinely overlap.
const (
	concWriters   = 8
	concWarmup    = 4
	concPerWriter = 200
)

// concUniverse is every transaction id the child may commit: the warm-up ids
// 1..concWarmup followed by concWriters disjoint ranges of concPerWriter each.
const concUniverse = concWarmup + concWriters*concPerWriter

// How many hits of the breakpoint are allowed through before the self-kill
// (GOGRAPH_CRASH_AFTER). Each must be large enough that the crash lands in the
// steady state — past the sequential warm-up commits — and small enough that the
// workload cannot finish first. Overshooting the second bound is not silent: the
// child then exits cleanly and runConcurrentCrash fails, naming the countdown.
//
// The two breakpoints are hit at very different rates, which is why they get
// different countdowns:
//
//   - the append point fires once per FRAME. Each transaction emits six op
//     frames plus its marker, so the warm-up alone accounts for 28 hits and the
//     whole workload for about 11 200. 300 is comfortably inside the concurrent
//     phase and nowhere near the end.
//   - the fsync point fires once per group-commit LEADER, and a group covers
//     many commits, so the whole workload may produce only a few hundred hits.
//     40 stays inside it with the same margin at the other end.
const (
	concSkipAppendHits = 300
	concSkipFsyncHits  = 40
)

// concEdge is one of the three arcs a scenario transaction commits.
type concEdge struct {
	src, dst, weight int64
}

// concTxnFacts is everything transaction id commits, hand-derived here from the
// scenario documented on cmd/crashinject-helper's runConcurrentWriters rather
// than imported from it, so a change on either side has to be reconciled
// deliberately: a ring over the three nodes base..base+2 with id-derived
// weights, the label "T" on base, and txn=id on base+1.
//
// The stride of 10 between transactions is what makes per-transaction
// completeness checkable independently: no transaction can supply or hide
// another's nodes.
func concTxnFacts(id int64) (nodes [3]int64, edges [3]concEdge, labelNode, propNode, propValue int64) {
	base := id * 10
	nodes = [3]int64{base, base + 1, base + 2}
	edges = [3]concEdge{
		{src: base, dst: base + 1, weight: id*100 + 1},
		{src: base + 1, dst: base + 2, weight: id*100 + 2},
		{src: base + 2, dst: base, weight: id*100 + 3},
	}
	return nodes, edges, base, base + 1, id
}

// concFactCount is the number of independently observable facts one transaction
// commits: three arcs, one label, one property.
const concFactCount = 5

// concPresentFacts counts how many of transaction id's facts the recovered
// graph carries. A weight mismatch counts the arc as ABSENT rather than
// present-but-wrong: either way the transaction did not land as committed, and
// counting it absent surfaces as an atomicity violation rather than being
// silently tolerated.
func concPresentFacts(g *lpg.Graph[int64, int64], id int64) int {
	_, edges, labelNode, propNode, propValue := concTxnFacts(id)
	n := 0
	for _, e := range edges {
		for dst, weight := range g.AdjList().Neighbours(e.src) {
			if dst == e.dst && weight == e.weight {
				n++
				break
			}
		}
	}
	if g.HasNodeLabel(labelNode, "T") {
		n++
	}
	if v, ok := g.GetNodeProperty(propNode, "txn"); ok {
		if got, _ := v.Int64(); got == propValue {
			n++
		}
	}
	return n
}

// checkConcurrent is the oracle. It returns one message per violated property,
// and an empty slice when the recovered graph honours both ACID properties this
// scenario can observe.
//
// acked is the set of transaction ids the child announced as acknowledged;
// universe is every id it could possibly have committed.
func checkConcurrent(g *lpg.Graph[int64, int64], universe int64, acked map[int64]bool) []string {
	var violations []string
	var accountedNodes, accountedArcs int64
	for id := int64(1); id <= universe; id++ {
		nodes, edges, _, _, _ := concTxnFacts(id)
		switch n := concPresentFacts(g, id); n {
		case concFactCount:
			// Whole. Nothing to report.
		case 0:
			// Absent. Legitimate for an unacknowledged transaction; the
			// durability check below catches it if it was acknowledged.
			if acked[id] {
				violations = append(violations, fmt.Sprintf(
					"DURABILITY: transaction %d was acknowledged but is entirely absent after recovery", id))
			}
		default:
			violations = append(violations, fmt.Sprintf(
				"ATOMICITY: transaction %d is PARTIAL after recovery — %d of %d facts present",
				id, n, concFactCount))
			if acked[id] {
				violations = append(violations, fmt.Sprintf(
					"DURABILITY: transaction %d was acknowledged but recovered incomplete", id))
			}
		}
		// Tally what the universe can ACCOUNT FOR, whether or not the
		// transaction landed whole. Counting the accounted state rather than
		// three-per-complete-transaction is what keeps the totals check
		// orthogonal: a partial transaction is already reported above, and
		// double-reporting it here would swamp the real signal.
		for _, k := range nodes {
			if _, ok := g.OutDegree(k); ok {
				accountedNodes++
			}
		}
		for _, e := range edges {
			if g.AdjList().HasEdge(e.src, e.dst) {
				accountedArcs++
			}
		}
	}
	// Recovery must invent nothing. Every node key and every ordered pair in the
	// graph should belong to some transaction in the universe — the id stride of
	// 10 keeps those sets disjoint — so a total above what the loop accounted for
	// is state no committed transaction explains.
	//
	// This cannot detect a DUPLICATED arc: the graph is not a multigraph, so a
	// replayed AddEdge for the same ordered pair is idempotent and leaves one
	// arc. Duplicate-replay idempotency is covered by the single-threaded shape
	// assertions in graph_shape_test.go, which count occurrences per pair.
	if got := int64(g.AdjList().Size()); got != accountedArcs {
		violations = append(violations, fmt.Sprintf(
			"arc count = %d, but only %d arcs belong to a transaction in the universe", got, accountedArcs))
	}
	if got := int64(g.LiveOrder()); got != accountedNodes {
		violations = append(violations, fmt.Sprintf(
			"live node count = %d, but only %d nodes belong to a transaction in the universe",
			got, accountedNodes))
	}
	return violations
}

// parseAcks extracts the acknowledged transaction ids from the child's stdout.
// Lines that are not ACK lines (the helper's completion notice, anything a
// future scenario prints) are ignored; a malformed ACK line is a harness bug and
// fails the test rather than being skipped, since silently dropping
// acknowledgements would weaken the very oracle this provides.
func parseAcks(t *testing.T, stdout []byte) map[int64]bool {
	t.Helper()
	acked := make(map[int64]bool)
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	// A long steady-state run produces thousands of short lines; the default
	// 64 KiB token limit is per LINE, so it is ample, but the buffer is sized
	// explicitly to make that independent of bufio's defaults.
	sc.Buffer(make([]byte, 0, 4096), 4096)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, "ACK ")
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			t.Fatalf("malformed ACK line %q: %v", line, err)
		}
		acked[id] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning child stdout: %v", err)
	}
	return acked
}

// concOpts builds the child environment for one concurrent-writer run.
func concOpts(skipHits int) crashinject.Opts {
	return crashinject.Opts{
		Env: []string{
			"GOGRAPH_CRASH_WRITERS=" + strconv.Itoa(concWriters),
			"GOGRAPH_CRASH_WARMUP=" + strconv.Itoa(concWarmup),
			"GOGRAPH_CRASH_PERWRITER=" + strconv.Itoa(concPerWriter),
			"GOGRAPH_CRASH_AFTER=" + strconv.Itoa(skipHits),
		},
		// The workload is thousands of durable commits; the package default of
		// 30 s is sized for scenarios that commit a handful.
		Timeout: 120 * time.Second,
	}
}

// runConcurrentCrash runs one concurrent-writer scenario, asserts the child was
// really SIGKILL'd at the breakpoint and that the crash landed after real
// acknowledged work, and returns the artefact directory plus the acknowledged
// set.
func runConcurrentCrash(t *testing.T, scenario string, skipHits int) (string, map[int64]bool) {
	t.Helper()
	out, err := crashinject.Run(t, scenario, concOpts(skipHits))
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if out.TimedOut {
		t.Fatalf("child timed out instead of crashing at %s\nstderr: %s", scenario, out.Stderr)
	}
	if !out.Killed {
		t.Fatalf("child not SIGKILL'd at %s (exit code %d) — the countdown (%d) outlived the workload, "+
			"so nothing was crashed and the assertions below would be vacuous\nstderr: %s",
			scenario, out.ExitCode, skipHits, out.Stderr)
	}
	acked := parseAcks(t, out.Stdout)
	// Non-degeneracy. A crash before the warm-up finished acknowledging leaves
	// nothing whose survival could be checked, so both ACID properties would
	// hold trivially and the test would pass while proving nothing.
	if len(acked) < concWarmup {
		t.Fatalf("%s: only %d acknowledged commits before the crash, want at least the %d warm-up commits — "+
			"the crash landed too early for the durability assertion to mean anything\nstderr: %s",
			scenario, len(acked), concWarmup, out.Stderr)
	}
	t.Logf("%s: child SIGKILL'd after %d acknowledged commits (countdown %d)", scenario, len(acked), skipHits)
	return out.Dir, acked
}

// TestCrashRecovery_ConcurrentWriters_MidAppend crashes the child inside one
// transaction's WAL frame run, while eight writers are committing durably
// through the store API and every other writer is queued behind the WAL mutex.
//
// The killed transaction's frames are partly in the writer's buffer (lost with
// the process) or, once the run has crossed bufio's buffer size, partly in the
// file with no OpCommit marker — which recovery must discard whole. Everything
// acknowledged before the kill must survive.
func TestCrashRecovery_ConcurrentWriters_MidAppend(t *testing.T) {
	const scenario = "wal.appendrun.frame-emitted"
	dir, acked := runConcurrentCrash(t, scenario, concSkipAppendHits)
	if v := checkConcurrent(recoverGraph(t, dir), concUniverse, acked); len(v) > 0 {
		t.Errorf("%s: recovery violated the ACID contract:\n%s", scenario, strings.Join(v, "\n"))
	}
}

// TestCrashRecovery_ConcurrentWriters_MidFsync crashes the child inside a group
// commit's fsync: the leader's buffered suffix is flushed to the OS, nothing
// below it is durable, and every follower in the group is parked in SyncGroup on
// a watermark that will never be published.
//
// No member of that group has acknowledged, so recovery may legitimately restore
// or discard the whole flushed suffix — but whatever it restores must be whole
// transactions, and every EARLIER group's acknowledged commits must still be
// there.
func TestCrashRecovery_ConcurrentWriters_MidFsync(t *testing.T) {
	const scenario = "wal.sync.pre-datasync"
	dir, acked := runConcurrentCrash(t, scenario, concSkipFsyncHits)
	if v := checkConcurrent(recoverGraph(t, dir), concUniverse, acked); len(v) > 0 {
		t.Errorf("%s: recovery violated the ACID contract:\n%s", scenario, strings.Join(v, "\n"))
	}
}

// TestCrashRecovery_ConcurrentWriters_NoCrashCompletesWholly is the end-to-end
// negative control. It runs the identical concurrent workload with a countdown
// far beyond any hit the workload can produce, so the child finishes and closes
// its WAL cleanly, then asserts that recovery restores the ENTIRE universe with
// no violations.
//
// Without it, a passing crash test could mean the oracle is blind: if the
// workload committed nothing observable, "every acknowledged transaction
// survived" and "nothing is partial" would both hold over an empty graph. This
// pins the other end — full acknowledgement, full recovery — using the same
// checker.
func TestCrashRecovery_ConcurrentWriters_NoCrashCompletesWholly(t *testing.T) {
	const scenario = "wal.appendrun.frame-emitted"
	// One past every frame the workload can emit: seven frames per transaction
	// (six ops plus the marker), so the countdown can never drain.
	const beyondReach = concUniverse*7 + 1
	out, err := crashinject.Run(t, scenario, concOpts(beyondReach))
	if err != nil {
		t.Fatalf("crashinject.Run(%s): %v", scenario, err)
	}
	if out.TimedOut {
		t.Fatalf("child timed out\nstderr: %s", out.Stderr)
	}
	if out.Killed {
		t.Fatalf("child was SIGKILL'd with a countdown of %d, which the workload cannot reach", beyondReach)
	}
	if out.ExitCode != 0 {
		t.Fatalf("child exited %d\nstdout: %s\nstderr: %s", out.ExitCode, out.Stdout, out.Stderr)
	}
	acked := parseAcks(t, out.Stdout)
	if len(acked) != concUniverse {
		t.Fatalf("acknowledged %d transactions, want the whole universe of %d", len(acked), concUniverse)
	}
	if v := checkConcurrent(recoverGraph(t, out.Dir), concUniverse, acked); len(v) > 0 {
		t.Errorf("uncrashed run did not recover wholly:\n%s", strings.Join(v, "\n"))
	}
}

// TestConcurrentWritersOracle_ReportsViolations validates the instrument. It
// builds graphs by hand — no crash, no subprocess — and asserts that
// checkConcurrent reports exactly the violation each one carries, and nothing on
// the one that carries none.
//
// A checker that has never been observed to fail is not evidence, and both of
// its arms have to be exercised separately: a partial transaction (the shape
// audit finding E5 produces when recovery's orphan filter discards part of an
// interleaved commit) and an acknowledged transaction that vanished entirely.
func TestConcurrentWritersOracle_ReportsViolations(t *testing.T) {
	// commit writes transaction id's facts into g, omitting the property when
	// whole is false — a transaction that landed 4 of its 5 facts.
	commit := func(g *lpg.Graph[int64, int64], id int64, whole bool) {
		t.Helper()
		_, edges, labelNode, propNode, propValue := concTxnFacts(id)
		for _, e := range edges {
			if err := g.AddEdge(e.src, e.dst, e.weight); err != nil {
				t.Fatalf("AddEdge(%d->%d): %v", e.src, e.dst, err)
			}
		}
		if err := g.SetNodeLabel(labelNode, "T"); err != nil {
			t.Fatalf("SetNodeLabel(%d): %v", labelNode, err)
		}
		if !whole {
			return
		}
		if err := g.SetNodeProperty(propNode, "txn", lpg.Int64Value(propValue)); err != nil {
			t.Fatalf("SetNodeProperty(%d): %v", propNode, err)
		}
	}
	newGraph := func() *lpg.Graph[int64, int64] {
		return lpg.New[int64, int64](adjlist.Config{Directed: true})
	}

	t.Run("clean graph reports nothing", func(t *testing.T) {
		g := newGraph()
		commit(g, 1, true)
		commit(g, 2, true)
		if v := checkConcurrent(g, 2, map[int64]bool{1: true, 2: true}); len(v) > 0 {
			t.Errorf("checker reported violations on a correct graph:\n%s", strings.Join(v, "\n"))
		}
	})

	t.Run("partial transaction is an atomicity violation", func(t *testing.T) {
		g := newGraph()
		commit(g, 1, true)
		commit(g, 2, false) // 4 of 5 facts — exactly what a dropped op frame leaves
		got := checkConcurrent(g, 2, map[int64]bool{1: true, 2: true})
		assertViolations(t, got, []string{"ATOMICITY: transaction 2", "DURABILITY: transaction 2"})
	})

	t.Run("lost acknowledged transaction is a durability violation", func(t *testing.T) {
		g := newGraph()
		commit(g, 1, true)
		// 2 was acknowledged and is entirely absent: the signature of recovery
		// discarding an interleaved transaction's frames as orphans.
		got := checkConcurrent(g, 2, map[int64]bool{1: true, 2: true})
		assertViolations(t, got, []string{"DURABILITY: transaction 2 was acknowledged but is entirely absent"})
	})

	t.Run("unacknowledged absent transaction is not a violation", func(t *testing.T) {
		g := newGraph()
		commit(g, 1, true)
		// 2 never acknowledged, never landed: the ordinary outcome of a crash,
		// and the case the checker must NOT flag or every crash test fails.
		if v := checkConcurrent(g, 2, map[int64]bool{1: true}); len(v) > 0 {
			t.Errorf("checker flagged a legitimately absent unacknowledged transaction:\n%s",
				strings.Join(v, "\n"))
		}
	})

	t.Run("an invented node is a surplus violation", func(t *testing.T) {
		g := newGraph()
		commit(g, 1, true)
		// A node no transaction committed: recovery inventing state. The
		// per-transaction loop cannot see it, so this is what the totals are for.
		if err := g.AddNode(999999); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		assertViolations(t, checkConcurrent(g, 1, map[int64]bool{1: true}),
			[]string{"live node count = 4, but only 3 nodes belong"})
	})
}

// assertViolations checks that got contains one violation matching each wanted
// substring and no others, so the instrument is pinned to reporting the right
// failure and not merely reporting something.
func assertViolations(t *testing.T, got, wantSubstrings []string) {
	t.Helper()
	if len(got) != len(wantSubstrings) {
		t.Fatalf("got %d violations, want %d:\ngot:  %s\nwant substrings: %s",
			len(got), len(wantSubstrings), strings.Join(got, " | "), strings.Join(wantSubstrings, " | "))
	}
	// Order-independent match so the checker's iteration order is not part of
	// the contract.
	remaining := append([]string(nil), got...)
	sort.Strings(remaining)
	for _, want := range wantSubstrings {
		found := -1
		for i, g := range remaining {
			if strings.Contains(g, want) {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("no violation containing %q; got: %s", want, strings.Join(remaining, " | "))
		}
		remaining = append(remaining[:found], remaining[found+1:]...)
	}
}
