package main

// catalogue_mgix.go — the MERGE/UNIQUE (§1.3) and index (§1.4) rows of the
// deterministic MVCC scenario catalogue (rmp #2933, stage B).
//
// # Cypher driver only
//
// Every row of this file runs through the Cypher engine alone. The lpg API has
// none of the shapes these rows exercise: it has no MERGE, the UNIQUE and NOT
// NULL constraints are the Cypher engine's registry, and a secondary index is
// maintained only by the change fan-out the Cypher engine drives at commit (see
// lpg.ErrIndexedRawWrite). A write made through the lpg API on an indexed graph
// is therefore either refused or invisible to the index by contract, so an lpg
// arm would test that contract rather than the row.
//
// # The seek = scan oracle
//
// An index row reads one predicate twice in ONE statement: through the access
// path under test (an equality, range or prefix predicate the planner serves from
// an index) and through a scan the planner cannot serve from one (`m.s + ''`,
// `m.k + 0`). The two counts come back as the columns seek and scan, and
// [seekEqualsScan] asserts they agree on every step and every final read. The
// scan reads the graph at the statement's snapshot and is the oracle.
// TestIXAccessPaths proves that the seek arm is planned as an index access and
// the scan arm is not; without it, a planner change could make both arms scans
// and the oracle would compare a plan with itself.

import (
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/internal/isolationtest"
)

// ---------------------------------------------------------------------------
// §1.3 MERGE and UNIQUE.

// uniqueK is the UNIQUE constraint most MG rows declare.
const uniqueK = "CREATE CONSTRAINT k_unique FOR (n:K) REQUIRE n.k IS UNIQUE"

// finalK lists every :K node, so a duplicate shows as a repeated key.
const finalK = "MATCH (n:K) RETURN n.k AS k, n.v AS v ORDER BY k, v"

func mg01(*world) *isolationtest.Spec {
	merge := func(by string) string {
		return "MERGE (p:K {k:'a'}) ON CREATE SET p.v = '" + by + "' RETURN p.v AS v"
	}
	return &isolationtest.Spec{
		Name: "mg01-merge-no-constraint",
		Doc: "MG01. Two explicit transactions MERGE (:K {k:'a'}) with no constraint. Each BEGIN\n" +
			"is a step, so every relative order of the two snapshots and the two commits is\n" +
			"enumerated. Both always succeed. When each snapshot precedes the other's commit,\n" +
			"both create and the graph holds two nodes; when one transaction begins after the\n" +
			"other committed, it matches the committed node and there is one (F10). PostgreSQL\n" +
			"MERGE without a unique index behaves the same way.",
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(begin("s1b"), q("s1m", merge("s1")), commit("s1c"))},
			{Name: "s2", Steps: steps(begin("s2b"), q("s2m", merge("s2")), commit("s2c"))},
		},
		Final: steps(q("final", finalK)),
	}
}

// mg02 builds one MG02 arm. end is the winner's last steps, loser and loserSetup
// are s2's steps and setup, and pairs are the adjacency the arm needs (the vacuum
// drain after a ROLLBACK).
func mg02(name, doc string, end, loser, loserSetup []isolationtest.Step,
	pairs ...[2]string) *isolationtest.Spec {
	s := &isolationtest.Spec{
		Name:  name,
		Doc:   doc,
		Setup: steps(q("ddl", uniqueK)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: append(steps(
				q("s1m", "MERGE (p:K {k:'a'}) ON CREATE SET p.v = 's1' RETURN p.v AS v")), end...)},
			{Name: "s2", Setup: loserSetup, Steps: loser},
		},
		Final: steps(q("final", finalK)),
	}
	if len(pairs) > 0 {
		return adjacentOnly(s, pairs...)
	}
	return s
}

const mg02Doc = "MG02. UNIQUE on (:K).k. s1 MERGEs k='a' and creates it, holding the reservation\n" +
	"open; s2 MERGEs the same key. While s1's reservation is in flight, s2's MERGE is a\n" +
	"ConstraintViolation — not a serialization conflict, so not retriable (F10, gap G6).\n" +
	"PostgreSQL's INSERT ... ON CONFLICT waits for s1 and then does nothing or updates;\n" +
	"InnoDB takes a shared lock and waits. GoGraph does not wait (F7).\n"

const mg02Merge = "MERGE (p:K {k:'a'}) ON CREATE SET p.v = 's2' RETURN p.v AS v"

func mg02Commit(*world) *isolationtest.Spec {
	return mg02("mg02-unique-merge-winner-commits",
		mg02Doc+"Commit arm: s1 commits. s2 is an explicit transaction whose snapshot predates\n"+
			"s1's commit, so it cannot match s1's node in any interleaving.",
		steps(commit("s1c")),
		steps(q("s2m", mg02Merge), commit("s2c")), steps(begin("s2b")))
}

func mg02RollsBack(w *world) *isolationtest.Spec {
	return mg02("mg02-unique-merge-winner-rolls-back",
		mg02Doc+"Rollback arm: s1 rolls back and the vacuum is drained (H1). A MERGE issued while\n"+
			"s1 is open is refused; one issued after the rollback creates the node, so the\n"+
			"reservation does not leak.",
		steps(rollback("s1rb"), w.drain("s1dr")),
		steps(q("s2m", mg02Merge), commit("s2c")), steps(begin("s2b")), [2]string{"s1rb", "s1dr"})
}

func mg02Autocommit(*world) *isolationtest.Spec {
	return mg02("mg02-unique-merge-autocommit-loser",
		mg02Doc+"Autocommit arm: s2's MERGE is an autocommit statement, so its snapshot is taken\n"+
			"when it runs. While s1 is open it meets the reservation; after s1 commits it\n"+
			"matches s1's node.",
		steps(commit("s1c")),
		steps(q("s2m", mg02Merge)), nil)
}

func mg03(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg03-unique-committed-after-snapshot",
		Doc: "MG03. UNIQUE on (:K).k. s2 begins; s1 creates k='u' in an autocommit statement;\n" +
			"s2 MERGEs k='u'. When s1 committed first, s2's snapshot cannot see the node, so\n" +
			"its MERGE tries to create it and meets the committed value: a ConstraintViolation.\n" +
			"When s2's MERGE runs first, s2 holds the reservation and s1's CREATE is refused.\n" +
			"PostgreSQL REPEATABLE READ raises a serialization failure in the first case.",
		Setup: steps(q("ddl", uniqueK)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(q("s1c", "CREATE (p:K {k:'u', v:'s1'}) RETURN p.v AS v"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2m", "MERGE (p:K {k:'u'}) ON CREATE SET p.v = 's2' RETURN p.v AS v"),
				commit("s2c"))},
		},
		Final: steps(q("final", finalK)),
	}
}

func mg04(*world) *isolationtest.Spec {
	read := "MATCH (n:K {k:42}) RETURN count(n) AS c"
	create := func(by string) string { return "CREATE (n:K {k:42, v:'" + by + "'}) RETURN n.v AS v" }
	return &isolationtest.Spec{
		Name: "mg04-read-then-insert-unique",
		Doc: "MG04. UNIQUE on (:K).k. Each transaction reads k=42 (none), then creates it. The\n" +
			"second CREATE meets the first's reservation or committed value and is a\n" +
			"ConstraintViolation at the statement; exactly one node commits in every\n" +
			"interleaving. PostgreSQL SERIALIZABLE waits and then raises a serialization\n" +
			"failure.",
		Setup: steps(q("ddl", uniqueK)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(q("s1r", read), q("s1w", create("s1")), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(q("s2r", read), q("s2w", create("s2")), commit("s2c"))},
		},
		Final: steps(q("final", finalK)),
	}
}

func mg05Spec(name, arm string, setup []isolationtest.Step) *isolationtest.Spec {
	read := "MATCH (i:Inv {y:2026}) RETURN max(i.n) AS m"
	next := "MATCH (i:Inv {y:2026}) WITH max(i.n) AS m CREATE (j:Inv {y:2026, n:m + 1}) RETURN j.n AS n"
	return &isolationtest.Spec{
		Name: name,
		Doc: "MG05, " + arm + ". A gapless per-year sequence: each transaction reads max(n)\n" +
			"for 2026 and creates max + 1. Without a constraint both read 1 and both create 2:\n" +
			"write skew, permitted under snapshot isolation (F1). With UNIQUE on (:Inv).n the\n" +
			"second CREATE is a ConstraintViolation and the sequence stays gapless.",
		Setup: append(setup, q("mk", "CREATE (:Inv {y:2026, n:1})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(q("s1r", read), q("s1w", next), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(q("s2r", read), q("s2w", next), commit("s2c"))},
		},
		Final: steps(q("final", "MATCH (i:Inv {y:2026}) RETURN i.n AS n ORDER BY n")),
	}
}

func mg05NoConstraint(*world) *isolationtest.Spec {
	return mg05Spec("mg05-gapless-sequence-no-constraint", "no constraint", nil)
}

func mg05Unique(*world) *isolationtest.Spec {
	return mg05Spec("mg05-gapless-sequence-unique", "UNIQUE on n",
		steps(q("ddl", "CREATE CONSTRAINT inv_n FOR (i:Inv) REQUIRE i.n IS UNIQUE")))
}

func mg06(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg06-merge-on-match-vs-update",
		Doc: "MG06. s1 updates n.v; s2 runs MERGE (n:K {k:'a'}) ON MATCH SET n.v = 2 with an\n" +
			"older snapshot. The MERGE matches at its snapshot, and its ON MATCH SET is a write\n" +
			"to a node whose head is not visible to it: refused (F2). PostgreSQL's\n" +
			"insert-conflict-do-update-3 updates a tuple its snapshot cannot see; GoGraph must\n" +
			"never do that.",
		Setup: steps(q("mk", "CREATE (:K {k:'a', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:K {k:'a'}) SET n.v = 1 RETURN n.v AS v"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2m", "MERGE (n:K {k:'a'}) ON MATCH SET n.v = 2 RETURN n.v AS v"), commit("s2c"))},
		},
		Final: steps(q("final", finalK)),
	}
}

func mg07(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg07-on-match-changes-unique-key",
		Doc: "MG07. UNIQUE on (:K).k, one node k='a'. s1 runs MERGE (n {k:'a'}) ON MATCH SET\n" +
			"n.k = 'b', which releases 'a' and reserves 'b'; s2 MERGEs k='b'; s3 creates k='a'\n" +
			"in an autocommit statement. Exactly one node per key in every interleaving, and\n" +
			"each loser gets a typed error. The release of 'a' is deferred to s1's commit\n" +
			"(rmp #2366), so s3 is refused while s1 is open.",
		Setup: steps(q("ddl", uniqueK), q("mk", "CREATE (:K {k:'a', v:'seed'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1m", "MERGE (n:K {k:'a'}) ON MATCH SET n.k = 'b' RETURN n.k AS k"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2m", "MERGE (n:K {k:'b'}) ON CREATE SET n.v = 's2' RETURN n.k AS k, n.v AS v"), commit("s2c"))},
			{Name: "s3", Steps: steps(q("s3c", "CREATE (n:K {k:'a', v:'s3'}) RETURN n.v AS v"))},
		},
		Final: steps(q("final", finalK)),
	}
}

func mg08(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg08-merge-vs-delete",
		Doc: "MG08. s1 DETACH DELETEs the node k='a'; s2, with an older snapshot, runs\n" +
			"MERGE (n:K {k:'a'}) ON MATCH SET n.v = 1 ON CREATE SET n.v = -1. s2 matches the node\n" +
			"at its snapshot, and its ON MATCH SET writes a node whose head is not visible to\n" +
			"it: refused (F2), never written onto a dead node and never resurrected. PostgreSQL\n" +
			"READ COMMITTED turns the MERGE into an INSERT.",
		Setup: steps(q("mk", "CREATE (:K {k:'a', v:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1d", "MATCH (n:K {k:'a'}) DETACH DELETE n RETURN count(*) AS deleted"), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2m", "MERGE (n:K {k:'a'}) ON MATCH SET n.v = 1 ON CREATE SET n.v = -1 RETURN n.v AS v"),
				commit("s2c"))},
		},
		Final: steps(q("final", finalK)),
	}
}

const mg09Doc = "MG09. UNIQUE on (:K).k, one node k='u'. Deleting a UNIQUE value and creating it\n" +
	"again must leave exactly one holder, never two and never none.\n"

func mg09SameTx(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg09-delete-recreate-same-tx",
		Doc: mg09Doc + "(a) s1 deletes k='u' and re-creates it in ONE transaction (fix 3b7bec2f); s2\n" +
			"creates k='u' in an autocommit statement. s2 meets the committed value or s1's\n" +
			"reservation in every interleaving.",
		Setup: steps(q("ddl", uniqueK), q("mk", "CREATE (:K {k:'u', v:'seed'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1d", "MATCH (n:K {k:'u'}) DELETE n RETURN count(*) AS deleted"),
				q("s1r", "CREATE (n:K {k:'u', v:'s1'}) RETURN n.v AS v"),
				commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2r", "CREATE (n:K {k:'u', v:'s2'}) RETURN n.v AS v"))},
		},
		Final: steps(q("final", finalK)),
	}
}

func mg09AfterCommit(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg09-delete-commit-then-recreate",
		Doc: mg09Doc + "(b) s1 deletes k='u' and commits; s2 creates k='u'. Each BEGIN is a step. s2's\n" +
			"CREATE succeeds once s1's delete has committed, whatever s2's snapshot; before\n" +
			"that it is refused, because the release of a UNIQUE value is deferred to the\n" +
			"deleter's commit (fca34a0c).",
		Setup: steps(q("ddl", uniqueK), q("mk", "CREATE (:K {k:'u', v:'seed'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(begin("s1b"),
				q("s1d", "MATCH (n:K {k:'u'}) DELETE n RETURN count(*) AS deleted"), commit("s1c"))},
			{Name: "s2", Steps: steps(begin("s2b"),
				q("s2r", "CREATE (n:K {k:'u', v:'s2'}) RETURN n.v AS v"), commit("s2c"))},
		},
		Final: steps(q("final", finalK)),
	}
}

func mg09PeerRollsBack(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "mg09-delete-open-then-rollback",
		Doc: mg09Doc + "(c) s1 deletes k='u' and holds it open; s2 creates k='u' in an autocommit\n" +
			"statement; s1 rolls back and the vacuum is drained (H1). While s1 is open s2 is\n" +
			"refused (the release is deferred to commit); after the rollback u is back and s2\n" +
			"is refused by it. Exactly one u in every interleaving.",
		Setup: steps(q("ddl", uniqueK), q("mk", "CREATE (:K {k:'u', v:'seed'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1d", "MATCH (n:K {k:'u'}) DELETE n RETURN count(*) AS deleted"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(q("s2r", "CREATE (n:K {k:'u', v:'s2'}) RETURN n.v AS v"))},
		},
		Final: steps(q("final", finalK)),
	}, [2]string{"s1rb", "s1dr"})
}

func mg10(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg10-intra-statement-duplicate",
		Doc: "MG10. UNIQUE on (:K).k. One statement writes the same value to two nodes:\n" +
			"SET a.k = 'x', b.k = 'x'. The statement is rejected and nothing it wrote is visible\n" +
			"to anyone (cypher/exec/constraints.go); the transaction is poisoned, so the client\n" +
			"rolls it back (F8). s2 creates k='x' in an autocommit statement. Between the failed\n" +
			"statement and the ROLLBACK the value is still reserved by the open transaction,\n" +
			"and s2 is refused (G6); after the ROLLBACK s2 succeeds, so no reservation leaks.",
		Setup: steps(q("ddl", uniqueK), q("mk", "CREATE (:K {k:'p', v:'a'}), (:K {k:'q', v:'b'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (a:K {v:'a'}), (b:K {v:'b'}) SET a.k = 'x', b.k = 'x' RETURN a.k AS a, b.k AS b"),
				rollback("s1rb"))},
			{Name: "s2", Steps: steps(q("s2c", "CREATE (n:K {k:'x', v:'s2'}) RETURN n.v AS v"))},
		},
		Final: steps(q("final", finalK)),
	}
}

// notNullP is the NOT NULL constraint MG12 declares.
const notNullP = "CREATE CONSTRAINT r_p FOR (n:R) REQUIRE n.p IS NOT NULL"

const finalR = "MATCH (n:R) RETURN n.name AS name, n.p AS p ORDER BY name"

func mg12InTx(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg12-not-null-at-commit",
		Doc: "MG12. NOT NULL on (:R).p, checked at COMMIT. s1 creates r1 without p and sets p\n" +
			"later in the same transaction: it commits. s2 creates r2 with p and removes p\n" +
			"later in the same transaction: its COMMIT is refused and nothing of it is applied.",
		Setup: steps(q("ddl", notNullP), q("mk", "CREATE (:R {name:'r0', p:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1c", "CREATE (n:R {name:'r1'}) RETURN n.name AS name"),
				q("s1s", "MATCH (n:R {name:'r1'}) SET n.p = 1 RETURN n.p AS p"),
				commit("s1x"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2c", "CREATE (n:R {name:'r2', p:2}) RETURN n.p AS p"),
				q("s2r", "MATCH (n:R {name:'r2'}) REMOVE n.p RETURN n.p AS p"),
				commit("s2x"))},
		},
		Final: steps(q("final", finalR)),
	}
}

func mg12PeerRemoval(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "mg12-not-null-peer-removal",
		Doc: "MG12, peer removal. NOT NULL on (:R).p. s1 sets r0.p; s2 removes r0.p. s2's\n" +
			"removal is refused, by the constraint at its COMMIT and, when s1 holds the node,\n" +
			"by the write-write conflict (F2); s1's write is refused when s2 holds the node.\n" +
			"r0.p is never null after any commit.",
		Setup: steps(q("ddl", notNullP), q("mk", "CREATE (:R {name:'r0', p:0})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1s", "MATCH (n:R {name:'r0'}) SET n.p = 1 RETURN n.p AS p"), commit("s1x"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2r", "MATCH (n:R {name:'r0'}) REMOVE n.p RETURN n.p AS p"), commit("s2x"))},
		},
		Final: steps(q("final", finalR)),
	}
}

// ---------------------------------------------------------------------------
// §1.4 Indexes.

// ixSeed creates 1 100 :L nodes n<i> with s = 'v<i>' and k = 2i. The population
// sits above the planner's floor for index access, so the seek arm of every read
// is planned as an index access; TestIXAccessPaths proves it.
//
// The planner estimates a btree or numeric predicate from the committed value
// domain, and plans a label scan for a bound outside it. A row that writes a value
// and then seeks it through a btree or a numeric index therefore writes a value
// INSIDE the domain and held by no node: 'v999x' sorts between 'v999' and 'v99:',
// and a moved k takes the key of another node (k = 16 is n8's), so the seek counts
// two nodes once the move is visible and one before.
const ixSeed = "UNWIND range(0, 1099) AS i CREATE (:L {name:'n' + toString(i), s:'v' + toString(i), k:2 * i})"

const (
	ixHash  = "CREATE INDEX l_s FOR (n:L) ON (n.s)"
	ixBtree = "CREATE INDEX l_s FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}"
)

// seekScan reads one predicate through an index (seek, which binds n) and through
// a scan (scan, a MATCH that binds m), in one statement, as the columns seek and
// scan.
//
// The scan arm is an OPTIONAL MATCH. With a plain MATCH, a scan that finds no node
// leaves no row for RETURN seek, count(m) to group, so the statement returns NO
// row and a seek that found a node the scan did not — #2814's stale entry — would
// vanish from the transcript instead of disagreeing in it.
func seekScan(seek, scan string) string {
	return seek + " WITH count(n) AS seek OPTIONAL " + scan + " RETURN seek, count(m) AS scan"
}

// eqS compares the equality seek on s = v, which a hash index serves.
func eqS(v string) string {
	return seekScan("MATCH (n:L {s:'"+v+"'})", "MATCH (m:L) WHERE m.s + '' = '"+v+"'")
}

// prefixS compares the prefix seek on s, which a btree index serves.
//
// The scan arm spells the prefix test with left(), not STARTS WITH: a STARTS WITH
// in the WHERE of a MATCH that follows a WITH clause fails to parse (see
// README.md, "Defects found").
func prefixS(p string) string {
	return seekScan("MATCH (n:L) WHERE n.s STARTS WITH '"+p+"'",
		"MATCH (m:L) WHERE left(m.s, size('"+p+"')) = '"+p+"'")
}

// rangeS compares the range seek s >= lo, which a btree index serves.
func rangeS(lo string) string {
	return seekScan("MATCH (n:L) WHERE n.s >= '"+lo+"'", "MATCH (m:L) WHERE m.s + '' >= '"+lo+"'")
}

func ix01(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ix01-own-write-hash-seek",
		Doc: "IX01 (#2814). Hash index on (:L).s over 1 100 nodes. Inside one transaction s1\n" +
			"moves n7 from s='v7' to s='zzz' and creates a node with s='qqq', reading each value\n" +
			"after the write through the equality seek and through a scan. Its own writes are\n" +
			"visible to the seek: 1 for 'zzz', 0 for 'v7', 1 for 'qqq', and seek = scan (F12).\n" +
			"s2 reads the same values in an autocommit statement and sees s1's writes only\n" +
			"after s1 commits. InnoDB's full-text index hides own uncommitted rows by design.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz' RETURN n.s AS s"),
				q("s1new", eqS("zzz")),
				q("s1old", eqS("v7")),
				q("s1cr", "CREATE (n:L {name:'c1', s:'qqq'}) RETURN n.s AS s"),
				q("s1crr", eqS("qqq")),
				commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2r", eqS("zzz")))},
		},
		Final: steps(q("fnew", eqS("zzz")), q("fold", eqS("v7")), q("fcr", eqS("qqq"))),
	}
}

func ix02(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ix02-own-write-btree-seek",
		Doc: "IX02 (#2814). Btree index on (:L).s over 1 100 nodes. Inside one transaction s1\n" +
			"moves n7 to s='v999x' and creates s='v999y'; the range seek s >= 'v999' and the\n" +
			"prefix seek STARTS WITH 'v999' must each equal the scan after every write (2, then\n" +
			"3). s2 reads the range in an autocommit statement and sees s1's writes only after\n" +
			"its commit.",
		Setup: steps(q("ix", ixBtree), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:L {name:'n7'}) SET n.s = 'v999x' RETURN n.s AS s"),
				q("s1rg", rangeS("v999")),
				q("s1px", prefixS("v999")),
				q("s1cr", "CREATE (n:L {name:'c1', s:'v999y'}) RETURN n.s AS s"),
				q("s1rg2", rangeS("v999")),
				q("s1px2", prefixS("v999")),
				commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2r", rangeS("v999")))},
		},
		Final: steps(q("frg", rangeS("v999")), q("fpx", prefixS("v999"))),
	}
}

func ix03(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ix03-own-write-autocommit",
		Doc: "IX03 (#2814). Hash index on (:L).s. ONE autocommit statement writes and then reads\n" +
			"through the equality seek: CREATE then MATCH must count the created node, and SET\n" +
			"then MATCH must count the moved one. #2814 reproduced with a single statement.\n" +
			"s2 reads 'q' in its own autocommit statement.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Steps: steps(
				q("s1cr", "CREATE (:L {name:'c1', s:'q'}) WITH 1 AS x MATCH (n:L {s:'q'}) RETURN count(n) AS c"),
				q("s1set", "MATCH (n:L {name:'n9'}) SET n.s = 'yyy' WITH 1 AS x MATCH (m:L {s:'yyy'}) RETURN count(m) AS c"))},
			{Name: "s2", Steps: steps(q("s2r", eqS("q")))},
		},
		Final: steps(q("fq", eqS("q")), q("fy", eqS("yyy")), q("fold", eqS("v9"))),
	}
}

// ix04 is the #2931 four-step reproduction, enumerated: the label is removed in
// the fixture; s1 re-values the node and rolls back; s2 adds the label back in an
// autocommit statement and reads both values. pv is the predicate value that
// finds s1's uncommitted value: the value itself for the equality seek, and its
// committed prefix 'v999' for the prefix seek, which the planner serves from the
// btree only for a prefix the committed domain holds.
func ix04(name, ddl, kind string, cmp func(string) string, pv string) func(*world) *isolationtest.Spec {
	return func(w *world) *isolationtest.Spec {
		return adjacentOnly(&isolationtest.Spec{
			Name: name,
			Doc: "IX04 (#2931), " + kind + ". n33 loses :L in a committed statement. s1 opens and sets\n" +
				"n33.s = 'v999p' (the 'polluted' value of #2931, inside the btree's value domain);\n" +
				"s2 adds :L back in an autocommit statement; s1 rolls back and the vacuum is\n" +
				"drained (H1). The label add must index n33 under its COMMITTED value 'v33', never\n" +
				"under s1's uncommitted 'v999p', so seek = scan for both values in every step and\n" +
				"after the run (F12).",
			Setup: steps(q("ix", ddl), q("mk", ixSeed), q("rm", "MATCH (n:L {name:'n33'}) REMOVE n:L")),
			Sessions: []*isolationtest.Session{
				{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
					q("s1w", "MATCH (n {name:'n33'}) SET n.s = 'v999p' RETURN n.s AS s"),
					rollback("s1rb"), w.drain("s1dr"))},
				{Name: "s2", Steps: steps(
					q("s2l", "MATCH (n {name:'n33'}) SET n:L RETURN n.name AS name"),
					q("s2v", cmp("v33")),
					q("s2p", cmp(pv)))},
			},
			Final: steps(q("fv", cmp("v33")), q("fp", cmp(pv))),
		}, [2]string{"s1rb", "s1dr"})
	}
}

var (
	ix04Hash  = ix04("ix04-peer-rollback-label-add-hash", ixHash, "hash index, equality seek", eqS, "v999p")
	ix04Btree = ix04("ix04-peer-rollback-label-add-btree", ixBtree, "btree index, prefix seek", prefixS, "v999")
)

// countL compares the :L count read through the label store with a scan that
// tests the label per node.
const countL = "MATCH (n:L) WITH count(n) AS seek OPTIONAL MATCH (m) WHERE 'L' IN labels(m) RETURN seek, count(m) AS scan"

func ix05(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ix05-rollback-leaves-no-trace",
		Doc: "IX05. Hash index on (:L).s, UNIQUE on (:K).k. s1 sets an indexed property\n" +
			"(n7.s = 'zzz'), adds the indexed label to m (s = 'lab'), creates an indexed node\n" +
			"(s = 'created') and takes a UNIQUE value (k = 'taken'), then rolls back; the vacuum\n" +
			"is drained (H1). s2 re-values m in an autocommit statement while s1 may hold m's\n" +
			"label, then creates k = 'taken'. Every seek — hash, label count, UNIQUE — must equal\n" +
			"its scan, and the final state must be the pre-state plus s2's commits alone: a\n" +
			"rolled-back transaction leaves no index entry and no reservation behind.",
		Setup: steps(q("ix", ixHash), q("ddl", uniqueK), q("mk", ixSeed),
			q("mk2", "CREATE (:M {name:'m', s:'lab'}), (:K {k:'k0'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1p", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz' RETURN n.s AS s"),
				q("s1l", "MATCH (m:M {name:'m'}) SET m:L RETURN m.s AS s"),
				q("s1c", "CREATE (:L {name:'c1', s:'created'}), (k:K {k:'taken'}) RETURN k.k AS k"),
				rollback("s1rb"), w.drain("s1dr"))},
			{Name: "s2", Steps: steps(
				q("s2w", "MATCH (m:M {name:'m'}) SET m.s = 'peer' RETURN m.s AS s"),
				q("s2u", "CREATE (k:K {k:'taken'}) RETURN k.k AS k"))},
		},
		Final: steps(
			q("fzzz", eqS("zzz")), q("fv7", eqS("v7")), q("flab", eqS("lab")), q("fpeer", eqS("peer")),
			q("fcr", eqS("created")), q("fcount", countL),
			q("ftaken", seekScan("MATCH (n:K {k:'taken'})", "MATCH (m:K) WHERE m.k + '' = 'taken'"))),
	}, [2]string{"s1rb", "s1dr"})
}

func ix06(*world) *isolationtest.Spec {
	return &isolationtest.Spec{
		Name: "ix06-moved-out-of-index-domain",
		Doc: "IX06. Hash index on (:L).s. s1 is a read-only transaction pinned at BEGIN. s2\n" +
			"moves n7 to s = 'moved' and removes :L from n8, each in its own autocommit\n" +
			"statement. s1 finds n7 under 'v7' and n8 under 'v8', and nothing under 'moved',\n" +
			"in every interleaving; the final read, at a new snapshot, finds only the new\n" +
			"state. Seek = scan everywhere.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(beginRead("s1b")), Steps: steps(
				q("s1v7", eqS("v7")), q("s1mv", eqS("moved")), q("s1v8", eqS("v8")), commit("s1c"))},
			{Name: "s2", Steps: steps(
				q("s2mv", "MATCH (n:L {name:'n7'}) SET n.s = 'moved' RETURN n.s AS s"),
				q("s2rm", "MATCH (n:L {name:'n8'}) REMOVE n:L RETURN n.name AS name"))},
		},
		Final: steps(q("fv7", eqS("v7")), q("fmv", eqS("moved")), q("fv8", eqS("v8"))),
	}
}

func ix07(*world) *isolationtest.Spec {
	both := seekScan("MATCH (n:L) WHERE n.a = 1 AND n.b = 2",
		"MATCH (m:L) WHERE m.a + 0 = 1 AND m.b + 0 = 2")
	return &isolationtest.Spec{
		Name: "ix07-two-index-predicate-pinned",
		Doc: "IX07. Hash indexes on (:L).a and (:L).b; one node matches a = 1 AND b = 2. s1 is a\n" +
			"read-only transaction pinned at BEGIN; s2 creates a second matching node in an\n" +
			"explicit transaction. s1 counts 1 in every interleaving, through the indexes and\n" +
			"through the scan; the final read counts 2.",
		Setup: steps(
			q("ixa", "CREATE INDEX l_a FOR (n:L) ON (n.a)"),
			q("ixb", "CREATE INDEX l_b FOR (n:L) ON (n.b)"),
			q("mk", "UNWIND range(0, 1099) AS i CREATE (:L {name:'n' + toString(i), a:i + 10, b:i + 10})"),
			q("mk2", "CREATE (:L {name:'hit', a:1, b:2}), (:L {name:'a-only', a:1, b:3}), (:L {name:'b-only', a:3, b:2})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(beginRead("s1b")), Steps: steps(q("s1r", both), q("s1r2", both), commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2w", "CREATE (n:L {name:'new', a:1, b:2}) RETURN n.name AS name"), commit("s2c"))},
		},
		Final: steps(q("final", both)),
	}
}

func ix08(*world) *isolationtest.Spec {
	read := seekScan("MATCH (n:L {s:'x'})", "MATCH (m:L) WHERE m.s + '' = 'x'")
	return &isolationtest.Spec{
		Name: "ix08-index-created-after-snapshot",
		Doc: "IX08. No index at first; one node has s = 'x'. s1 is a read-only transaction\n" +
			"pinned at BEGIN. s2 creates a hash index on (:L).s in an autocommit statement; s3\n" +
			"creates a second node with s = 'x'. s1 counts only its snapshot's node, whether\n" +
			"its read is planned before or after the index exists (commit 71a0541e). The\n" +
			"final read counts both.",
		Setup: steps(q("mk", ixSeed), q("mk2", "CREATE (:L {name:'x1', s:'x'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(beginRead("s1b")), Steps: steps(q("s1r", read), commit("s1c"))},
			{Name: "s2", Steps: steps(q("s2ix", ixHash))},
			{Name: "s3", Steps: steps(q("s3w", "CREATE (n:L {name:'x2', s:'x'}) RETURN n.name AS name"))},
		},
		Final: steps(q("final", read)),
	}
}

func ix09(w *world) *isolationtest.Spec {
	return adjacentOnly(&isolationtest.Spec{
		Name: "ix09-label-index-scan-agree",
		Doc: "IX09. Hash index on (:L).s. s1 removes :L from n3 and adds it to the :X node x1\n" +
			"(s = 'xs') in one transaction and commits; s2 moves n5 to s = 'moved' and rolls\n" +
			"back, and the vacuum is drained (H1). At every step and after the run the label\n" +
			"count, the property index and the scan agree.",
		Setup: steps(q("ix", ixHash), q("mk", ixSeed), q("mk2", "CREATE (:X {name:'x1', s:'xs'})")),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1rm", "MATCH (n:L {name:'n3'}) REMOVE n:L RETURN n.name AS name"),
				q("s1add", "MATCH (n:X {name:'x1'}) SET n:L RETURN n.name AS name"),
				commit("s1c"))},
			{Name: "s2", Setup: steps(begin("s2b")), Steps: steps(
				q("s2mv", "MATCH (n:L {name:'n5'}) SET n.s = 'moved' RETURN n.s AS s"),
				rollback("s2rb"), w.drain("s2dr"))},
		},
		Final: steps(q("fcount", countL), q("fv3", eqS("v3")), q("fxs", eqS("xs")),
			q("fv5", eqS("v5")), q("fmv", eqS("moved"))),
	}, [2]string{"s2rb", "s2dr"})
}

func ix10(*world) *isolationtest.Spec {
	byParamS := seekScan("MATCH (n:L {s:$v})", "MATCH (m:L) WHERE m.s + '' = $v")
	byParamK := seekScan("MATCH (n:L {k:$k})", "MATCH (m:L) WHERE m.k + 0 = $k")
	pq := func(name, query string, params map[string]any) isolationtest.Step {
		return isolationtest.Step{Name: name, Query: query, Params: params}
	}
	return &isolationtest.Spec{
		Name: "ix10-parameter-seek-own-write",
		Doc: "IX10 (#2814 through a second access path, 3fd78c5e). Hash indexes on (:L).s and\n" +
			"(:L).k. Inside one transaction s1 moves n7 to s = 'zzz' and k = 16 (n8's key), then\n" +
			"reads both through a PARAMETER seek ($v string, $k integer). Each equals the\n" +
			"literal answer and the scan. s2 reads the same parameters in an autocommit\n" +
			"statement.",
		Setup: steps(q("ix", ixHash), q("ixk", "CREATE INDEX l_k FOR (n:L) ON (n.k)"), q("mk", ixSeed)),
		Sessions: []*isolationtest.Session{
			{Name: "s1", Setup: steps(begin("s1b")), Steps: steps(
				q("s1w", "MATCH (n:L {name:'n7'}) SET n.s = 'zzz', n.k = 16 RETURN n.s AS s, n.k AS k"),
				pq("s1ps", byParamS, map[string]any{"v": "zzz"}),
				pq("s1pk", byParamK, map[string]any{"k": 16}),
				pq("s1po", byParamS, map[string]any{"v": "v7"}),
				q("s1ls", eqS("zzz")),
				q("s1lk", seekScan("MATCH (n:L {k:16})", "MATCH (m:L) WHERE m.k + 0 = 16")),
				commit("s1c"))},
			{Name: "s2", Steps: steps(pq("s2ps", byParamS, map[string]any{"v": "zzz"}))},
		},
		Final: steps(pq("fps", byParamS, map[string]any{"v": "zzz"}),
			pq("fpk", byParamK, map[string]any{"k": 16}),
			pq("fpo", byParamS, map[string]any{"v": "v7"})),
	}
}

// seekStepQueries returns the distinct seek-vs-scan queries of a spec's steps and
// final reads: every query carrying the seek and scan columns.
func seekStepQueries(s *isolationtest.Spec) []isolationtest.Step {
	var out []isolationtest.Step
	seen := map[string]bool{}
	add := func(st isolationtest.Step) {
		if !strings.Contains(st.Query, "AS seek") || seen[st.Query] {
			return
		}
		seen[st.Query] = true
		out = append(out, st)
	}
	for _, sess := range s.Sessions {
		for _, st := range sess.Steps {
			add(st)
		}
	}
	for _, st := range s.Final {
		add(st)
	}
	return out
}
