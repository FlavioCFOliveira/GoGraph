package main

// catalogue_ix11.go — IX11, one large commit re-values an indexed property while
// readers seek (rmp #3017; docs/mvcc-scenario-catalogue.md §1.4).
//
// Re-implemented from Turso's core/mvcc/database/tests.rs
// test_reader_consistent_during_large_indexed_commit_rewrite (line 4940) and
// test_reader_does_not_see_inflight_index_tombstone (5550), read at d7f4945
// (MIT, "Copyright 2024 the Turso authors"). Turso rewrites 10 000 rows; this
// golden rewrites the 1 100 nodes of ixSeed, the catalogue's shared index fixture,
// on which the planner serves the seek arm from the index (TestIXAccessPaths: the
// literal-valued seeks are planned from the committed value domain), and far above
// the only batching boundary the commit-time index delivery has (the two-change
// inline buffer of exec.IndexBuffer; index.Manager.ApplyBatchInState applies a
// batch whole).
//
// The golden pins the deterministic instants — before the commit, while it is
// open, after it — for a reader pinned at BEGIN and for autocommit readers. The
// instant INSIDE the delivery (between ApplyBatchInState and FinishApplied) is
// not reachable by a step script; the IX11 ladder arm (ladder_turso.go) covers it
// under load.

import (
	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// ix11Rewrite re-values s on every :L node to its successor's value: node ni,
// which holds 'v<i>', takes 'v<i+1>' (ixSeed sets k = 2i). Every index entry is
// removed and re-inserted, as Turso's suffix rewrite does, and the new values stay
// inside the fixture's value domain, where the planner serves btree seeks from the
// index ('v1100' sorts between 'v110' and 'v111').
const ix11Rewrite = "MATCH (n:L) SET n.s = 'v' + toString(n.k / 2 + 1) RETURN count(n) AS rewritten"

// ix11Mix counts, at the reader's instant, the :L nodes holding the rewritten
// value and all :L nodes: a reader sees 0 of 1100 or 1100 of 1100, never a mix.
const ix11Mix = "MATCH (n:L) RETURN sum(CASE WHEN n.s = 'v' + toString(n.k / 2 + 1) THEN 1 ELSE 0 END) AS rewritten, count(n) AS total"

// ix11 builds one arm. oldRead and newRead are seek-vs-scan reads whose counts
// change with the rewrite; each is served by the arm's index at the fixture's
// value domain (TestIXAccessPaths).
func ix11(name, ddl, kind, reads, oldRead, newRead string) func(*world) *isolationtest.Spec {
	return func(*world) *isolationtest.Spec {
		return &isolationtest.Spec{
			Name: name,
			Doc: "IX11, " + kind + " (Turso tests.rs:4940, :5550; MIT). s1 re-values s on all 1 100\n" +
				":L nodes in one transaction, each to its successor's value, and commits. s2 is a\n" +
				"read-only transaction pinned at BEGIN; s3 reads in autocommit statements. " + reads + "\n" +
				"Every seek equals its own scan at every instant: the old counts before the\n" +
				"publish, the new counts after it, never a mix (a seek declines to the scan while\n" +
				"the index cannot prove the reader's instant, F12).",
			Setup: steps(q("ix", ddl), q("mk", ixSeed)),
			Sessions: []*isolationtest.Session{
				{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
					q("s1w", ix11Rewrite),
					commit("s1c"))},
				{Name: "s2", Setup: steps(beginRead("s2b")), Steps: steps(
					q("s2o", oldRead),
					q("s2n", newRead),
					q("s2m", ix11Mix)),
					Teardown: steps(commit("s2c"))},
				{Name: "s3", Steps: steps(
					q("s3o", oldRead),
					q("s3n", newRead),
					q("s3m", ix11Mix))},
			},
			Final: steps(q("fo", oldRead), q("fn", newRead), q("fm", ix11Mix)),
			Permutations: perms(
				// Every read before the commit, one of them while it is open.
				"s2o s1w s3o s2n s3n s2m s3m s1c",
				// The pinned reader straddles the commit; the autocommit reader follows it.
				"s2o s1w s1c s2n s2m s3o s3n s3m",
				// The autocommit reader straddles the commit.
				"s1w s3o s1c s3n s3m s2o s2n s2m",
			),
		}
	}
}

var (
	ix11Hash = ix11("ix11-large-indexed-commit-hash", ixHash, "hash index",
		"The equality seek of 'v0' counts 1 before\nthe rewrite and 0 after; of 'v1100', 0 before and 1 after.",
		eqS("v0"), eqS("v1100"))
	// The btree arm names the node rather than counting it: 'v5' and 'v500' are
	// held by one node before the rewrite (n5, n500) and by another after it (n4,
	// n499), and both stay inside the value domain, so the seek is planned from the
	// index at every instant. A count would not change across the rewrite.
	ix11Btree = ix11("ix11-large-indexed-commit-btree", ixBtree, "btree index",
		"The equality seek of 'v5' finds n5 before\nthe rewrite and n4 after; of 'v500', n500 before and n499 after.",
		eqNames("v5"), eqNames("v500"))
)

// eqNames compares the equality seek on s = v with a scan, as the names of the
// nodes each finds; seekEqualsScan compares the two columns.
func eqNames(v string) string {
	return "MATCH (n:L {s:'" + v + "'}) WITH collect(n.name) AS seek OPTIONAL MATCH (m:L) WHERE m.s + '' = '" + v +
		"' RETURN seek, collect(m.name) AS scan"
}
