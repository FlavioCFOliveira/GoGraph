package main

// catalogue_hermitage.go — the Hermitage rows SK15, SK16 and SK17 of the
// deterministic catalogue (rmp #3016; docs/mvcc-scenario-catalogue.md §1.2).
//
// # Source and licence
//
// Adapted from Turso's Hermitage tests, core/mvcc/database/hermitage_tests.rs
// (tursodatabase/turso, read at 9cfcbdc5 for the catalogue and at d7f4945 for this
// adaptation): test_hermitage_g1a_aborted_reads (line 150),
// test_hermitage_g1b_intermediate_reads (200),
// test_hermitage_g1c_circular_information_flow (246),
// test_hermitage_otv_observed_transaction_vanishes (295) and
// test_hermitage_aborted_transaction_not_visible (862). Turso is MIT-licensed,
// "Copyright 2024 the Turso authors" (LICENSE.md); those tests are themselves adapted
// from Martin Kleppmann's Hermitage (https://github.com/ept/hermitage). The
// scenarios are carried over step for step, in Turso's order, as isolationtest
// specs: the two rows (id 1 => 10, id 2 => 20) are the nodes a and b, each
// UPDATE is a SET, each SELECT a read of the transaction's own snapshot.
//
// # Where GoGraph differs from Turso, and why
//
//   - Turso rolls a transaction refused by a write-write conflict back itself;
//     GoGraph poisons it and requires ROLLBACK (F8), so SK17's refused session
//     ends with an explicit ROLLBACK step.
//   - SK15 adds what Turso does not script: a third session that reads after T1
//     has ended, before and after the vacuum drain (H1), because an aborted head
//     stays in the chain until the vacuum withdraws it (F5) and is classified as
//     another transaction's uncommitted work only by mvcc.Visible
//     (graph/mvcc/mvcc.go, AbortedTS and Visible).
//
// Every spec runs NAMED interleavings: Turso's own order, plus the orders that
// place a read at the instants the anomaly would show — while the writer is
// open, after it ended and before the drain, after the drain. The full
// enumeration of SK15 and SK17 exceeds the short-layer ceiling; SK16 is small
// enough to run every interleaving.

import (
	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// hermitageFixture is Hermitage's two rows as two nodes.
const hermitageFixture = "CREATE (:H {name:'a', v:10}), (:H {name:'b', v:20})"

// hermitageReadA, hermitageReadB and hermitageReadAB read the rows.
const (
	hermitageReadA  = "MATCH (n:H {name:'a'}) RETURN n.v AS a"
	hermitageReadB  = "MATCH (n:H {name:'b'}) RETURN n.v AS b"
	hermitageReadAB = "MATCH (x:H {name:'a'}), (y:H {name:'b'}) RETURN x.v AS a, y.v AS b"
)

// hermitageFinal is the state a permutation leaves.
var hermitageFinal = steps(q("final", hermitageReadAB))

// sk15G1a is G1a, aborted reads: T1 writes 101 and rolls back. T2 (open since
// before the write) reads 10 before and after the rollback; T3, whose reads are
// autocommit snapshots, reads 10 while T1 is open, after its rollback before the
// drain, and after the drain.
func sk15G1a(w *world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk15-aborted-read-g1a",
		Doc: "SK15, G1a arm (Turso hermitage_tests.rs:150, :862; MIT, adapted from Hermitage).\n" +
			"T1 sets a = 101 and ROLLS BACK; the vacuum is drained after (H1). T2, open since\n" +
			"before T1's write, reads (10, 20) before and after the rollback. T3's autocommit\n" +
			"reads see a = 10 while T1 is open, after the rollback but before the drain (the\n" +
			"aborted head is still in the chain, F5), and after the drain: an aborted version\n" +
			"is another transaction's uncommitted work to every reader.",
		Setup: steps(q("mk", hermitageFixture)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u", "MATCH (n:H {name:'a'}) SET n.v = 101 RETURN n.v AS a"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r1", hermitageReadAB),
				q("s2r2", hermitageReadAB),
				commit("s2c"))},
			{Name: "s3", Steps: steps(
				q("s3r1", hermitageReadA),
				q("s3r2", hermitageReadA),
				q("s3r3", hermitageReadA))},
		},
		Final: hermitageFinal,
		Permutations: perms(
			// Turso's order, then T3 before and after the drain.
			"s1u s2r1 s1rb s2r2 s2c s3r1 s1dr s3r2 s3r3",
			// T3 reads while T1 is open, after the rollback, and after the drain.
			"s1u s3r1 s2r1 s1rb s3r2 s2r2 s1dr s3r3 s2c",
			// T2 reads before T1 writes and after the drain.
			"s2r1 s1u s3r1 s1rb s3r2 s1dr s3r3 s2r2 s2c",
		),
	}
}

// sk15G1b is G1b, intermediate reads: T1 writes 101, then 11, and commits. No
// reader ever sees 101; T2 keeps its snapshot's 10; T3 reads 10 before the
// commit and 11 after it.
func sk15G1b(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk15-intermediate-read-g1b",
		Doc: "SK15, G1b arm (Turso hermitage_tests.rs:200; MIT, adapted from Hermitage). T1\n" +
			"sets a = 101, then a = 11, and commits. T2, open since before T1's first write,\n" +
			"reads (10, 20) at every step (F6). T3's autocommit reads see 10 before T1's\n" +
			"commit and 11 after it, never the intermediate 101.",
		Setup: steps(q("mk", hermitageFixture)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1u1", "MATCH (n:H {name:'a'}) SET n.v = 101 RETURN n.v AS a"),
				q("s1u2", "MATCH (n:H {name:'a'}) SET n.v = 11 RETURN n.v AS a"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r1", hermitageReadAB),
				q("s2r2", hermitageReadAB),
				commit("s2c"))},
			{Name: "s3", Steps: steps(
				q("s3r1", hermitageReadA),
				q("s3r2", hermitageReadA))},
		},
		Final: hermitageFinal,
		Permutations: perms(
			// Turso's order, then T3 after the commit.
			"s1u1 s2r1 s1u2 s1c s2r2 s2c s3r1 s3r2",
			// T3 reads between the two writes (the intermediate instant) and after the commit.
			"s1u1 s3r1 s2r1 s1u2 s1c s3r2 s2r2 s2c",
			// T3 reads after the final write, before the commit.
			"s1u1 s1u2 s3r1 s2r1 s1c s2r2 s3r2 s2c",
		),
	}
}

// sk16G1c is G1c, circular information flow: two writers on disjoint nodes each
// read the other's node before either commits. Every interleaving runs.
func sk16G1c(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk16-circular-information-flow-g1c",
		Doc: "SK16, G1c (Turso hermitage_tests.rs:246; MIT, adapted from Hermitage). T1 sets\n" +
			"a = 11 and reads b; T2 sets b = 22 and reads a; both commit. Each reads the\n" +
			"other's node at its own snapshot (b = 20, a = 10) in every interleaving, never the\n" +
			"other's uncommitted write; both commit, the nodes are disjoint (F4, WW09), and the\n" +
			"final state is (11, 22).",
		Setup: steps(q("mk", hermitageFixture)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:H {name:'a'}) SET n.v = 11 RETURN n.v AS a"),
				q("s1r", hermitageReadB),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "MATCH (n:H {name:'b'}) SET n.v = 22 RETURN n.v AS b"),
				q("s2r", hermitageReadA),
				commit("s2c"))},
		},
		Final: hermitageFinal,
	}
}

// sk17OTV is OTV, observed transaction vanishes, in Turso's order. T2' is a
// fourth session that BEGINs after T1 committed: GoGraph takes the snapshot at
// BEGIN (F6), so a T2' begun with the others would be refused.
func sk17OTV(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "sk17-observed-transaction-vanishes-otv",
		Doc: "SK17, OTV (Turso hermitage_tests.rs:295; MIT, adapted from Hermitage). T1 sets\n" +
			"a = 11 and b = 19; T2 sets a = 12 and is refused at its statement (F2, F3), then\n" +
			"ROLLS BACK (GoGraph poisons it; Turso rolls it back itself, F8); T1 commits; T2'\n" +
			"BEGINs after that commit, sets a = 12 and b = 18, and commits. T3, open since the\n" +
			"start, reads (10, 20) at every step: a committed or refused peer's effects never\n" +
			"appear in, then vanish from, its snapshot. Final state (12, 18).",
		Setup: steps(q("mk", hermitageFixture)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1a", "MATCH (n:H {name:'a'}) SET n.v = 11 RETURN n.v AS a"),
				q("s1bw", "MATCH (n:H {name:'b'}) SET n.v = 19 RETURN n.v AS b"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2a", "MATCH (n:H {name:'a'}) SET n.v = 12 RETURN n.v AS a"),
				rollback("s2rb"))},
			{Name: "s3", Setup: steps(begin("s3b")), Steps: steps(
				q("s3r1", hermitageReadAB),
				q("s3r2", hermitageReadAB),
				q("s3r3", hermitageReadAB),
				q("s3r4", hermitageReadAB),
				commit("s3c"))},
			{Name: "s4", Steps: steps(
				begin("s4b"),
				q("s4a", "MATCH (n:H {name:'a'}) SET n.v = 12 RETURN n.v AS a"),
				q("s4bw", "MATCH (n:H {name:'b'}) SET n.v = 18 RETURN n.v AS b"),
				commit("s4c"))},
		},
		Final: hermitageFinal,
		Permutations: perms(
			// Turso's order: T3 reads a after T1's commit, b after T2' wrote, b
			// after T2' committed, a at the end.
			"s1a s1bw s2a s2rb s1c s3r1 s4b s4a s4bw s3r2 s4c s3r3 s3r4 s3c",
			// T3 also reads while T1 is open and T2 refused, before T1 commits.
			"s1a s1bw s3r1 s2a s3r2 s2rb s1c s3r3 s4b s4a s4bw s4c s3r4 s3c",
		),
	}
}
