//go:build drivercompat

package server_test

// driver_compat_test.go — the standing driver-compatibility suite (rmp #2191).
//
// The round-3 audit drove the official neo4j-go-driver v5.28.4 against the in-process
// Bolt server and found 13 hard failures and 2 degraded results across 37 checks. That
// probe was ad hoc: it lived in a scratch directory, nothing in the repository prevented
// those regressions, and nothing detected new ones.
//
// This file makes it permanent and RATCHETED, in the same spirit as the TCK execution
// baseline: every check runs, the pass/fail/degraded tally is reported, and the suite
// FAILS if the passing count drops below the recorded floor. A check moving from fail to
// pass is a win that only needs the floor raised; a check moving the other way breaks the
// build.
//
// # Why a build tag
//
// Each check stands up a server and a real driver connection, so the suite is slower than
// the short layer's budget allows. It is gated behind the `drivercompat` tag and run
// explicitly:
//
//	go test -tags=drivercompat -run TestDriverCompatibility -v ./bolt/server/
//
// # What a status means
//
//   - PASS      — the driver got what the Bolt specification says it should.
//   - FAIL      — a real incompatibility. Counted against the ratchet.
//   - DEGRADED  — the call succeeds but the value is not what a Neo4j client would get
//                 (e.g. a timing field the server does not measure). Tracked, not
//                 ratcheted, because the fix is a feature rather than a defect.
//   - SKIP      — the check cannot run in this environment; never counted as a pass.

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// checkStatus is the outcome of one compatibility check.
type checkStatus int

const (
	statusPass checkStatus = iota
	statusFail
	statusDegraded
	statusSkip
)

func (s checkStatus) String() string {
	switch s {
	case statusPass:
		return "PASS"
	case statusFail:
		return "FAIL"
	case statusDegraded:
		return "DEGRADED"
	default:
		return "SKIP"
	}
}

// checkResult is one check's verdict plus the detail that explains it.
type checkResult struct {
	name   string
	status checkStatus
	detail string
}

// compatChecks is the recorded floor: the number of checks that MUST pass.
//
// History:
//   - 20/37 — round-3 audit baseline (v0.10.0). 13 fail, 2 degraded, 2 informational.
//   - 28/37 — after #2189 (entity structures), #2212 and #2190 (write statistics,
//     including MERGE created-vs-matched and the index/constraint counters) and #2191's
//     imp_user fix, which turned a DEGRADED silent-accept into a PASS by failing closed.
//
// The 8 remaining failures and 1 degraded result, each a known gap with a cause:
//
//   - temporal / Point2D / []byte PARAMETER round-trips — cypher.BindParams rejects
//     packstream.Struct and []uint8. Inbound temporal and spatial parameters are simply
//     not implemented; outbound temporals already work (#2189 left them untouched).
//   - EXPLAIN / PROFILE — not accepted as query prefixes. Engine.Explain() exists as a
//     Go method; there is no statement-level prefix, so ResultSummary.Plan()/Profile()
//     stay nil.
//   - ResultSummary.StatementType — the server never sends the `type` metadata field.
//   - SHOW TRANSACTIONS — the DDL parser has no SHOW TRANSACTIONS form (the operator
//     API added in #2176 is reachable another way).
//   - CALL dbms.components() — the dbms.* procedure namespace is not implemented.
//   - DEGRADED ResultSummary timing (t_first / t_last) — the server does not measure
//     them, so the driver reports -1ms. A feature, not a defect.
//
// Raise this whenever a check is fixed. Never lower it.
const compatPassFloor = 28

// runCheck evaluates one check, converting a panic into a FAIL so a driver-side panic
// (which the audit hit on ResultSummary.Database().Name()) is a recorded incompatibility
// rather than a lost test run.
func runCheck(name string, fn func() (checkStatus, string)) (res checkResult) {
	res = checkResult{name: name}
	defer func() {
		if p := recover(); p != nil {
			res.status = statusFail
			res.detail = fmt.Sprintf("panic: %v", p)
		}
	}()
	res.status, res.detail = fn()
	return res
}

func pass(format string, a ...any) (checkStatus, string) {
	return statusPass, fmt.Sprintf(format, a...)
}

func fail(format string, a ...any) (checkStatus, string) {
	return statusFail, fmt.Sprintf(format, a...)
}

func degraded(format string, a ...any) (checkStatus, string) {
	return statusDegraded, fmt.Sprintf(format, a...)
}

// TestDriverCompatibility runs every recorded check and ratchets the passing count.
func TestDriverCompatibility(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	// withSession runs fn against a fresh session and CLOSES it before returning, so the
	// connection goes back to the pool immediately. Deferring the closes to t.Cleanup
	// instead exhausts the driver's 5-connection pool part-way through the suite and
	// every later check fails with a connection timeout — which is a defect in the
	// harness, not in the server, and is exactly the kind of false red the ratchet must
	// not report.
	withSession := func(cfg neo4j.SessionConfig, fn func(neo4j.SessionWithContext) error) error {
		s := driver.NewSession(ctx, cfg)
		defer func() { _ = s.Close(ctx) }()
		return fn(s)
	}

	// readOne runs a query and returns its single record, or an error.
	readOne := func(query string) (rec *neo4j.Record, err error) {
		err = withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			res, rerr := s.Run(ctx, query, nil)
			if rerr != nil {
				return rerr
			}
			rec, rerr = res.Single(ctx)
			return rerr
		})
		return rec, err
	}

	// write runs a statement and returns its summary.
	write := func(query string) (sum neo4j.ResultSummary, err error) {
		err = withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			res, rerr := s.Run(ctx, query, nil)
			if rerr != nil {
				return rerr
			}
			sum, rerr = res.Consume(ctx)
			return rerr
		})
		return sum, err
	}

	// writeParams is write with parameters, for the round-trip checks.
	writeParams := func(query string, params map[string]any) error {
		return withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			res, rerr := s.Run(ctx, query, params)
			if rerr != nil {
				return rerr
			}
			_, rerr = res.Single(ctx)
			return rerr
		})
	}

	var results []checkResult
	add := func(name string, fn func() (checkStatus, string)) {
		results = append(results, runCheck(name, fn))
	}

	// ── connectivity and server identity ──

	add("driver.VerifyConnectivity", func() (checkStatus, string) {
		if err := driver.VerifyConnectivity(ctx); err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	add("driver.GetServerInfo", func() (checkStatus, string) {
		info, err := driver.GetServerInfo(ctx)
		if err != nil {
			return fail("%v", err)
		}
		return pass("agent=%q protocol=%v", info.Agent(), info.ProtocolVersion())
	})

	// ── entity structures (#2189) ──

	add("RETURN n -> neo4j.Node", func() (checkStatus, string) {
		if _, err := write(`CREATE (:Person:Admin {name: 'alice', age: 42})`); err != nil {
			return fail("seed: %v", err)
		}
		rec, err := readOne(`MATCH (n:Person) RETURN n`)
		if err != nil {
			return fail("%v", err)
		}
		v, _ := rec.Get("n")
		n, ok := v.(neo4j.Node)
		if !ok {
			return fail("got %T, want neo4j.Node", v)
		}
		return pass("labels=%v props=%v elementId=%q", n.Labels, n.Props, n.ElementId)
	})

	add("RETURN r -> neo4j.Relationship", func() (checkStatus, string) {
		if _, err := write(`CREATE (:RA)-[:KNOWS {since: 2020}]->(:RB)`); err != nil {
			return fail("seed: %v", err)
		}
		rec, err := readOne(`MATCH (:RA)-[r:KNOWS]->(:RB) RETURN r`)
		if err != nil {
			return fail("%v", err)
		}
		v, _ := rec.Get("r")
		r, ok := v.(neo4j.Relationship)
		if !ok {
			return fail("got %T, want neo4j.Relationship", v)
		}
		return pass("type=%q elementId=%q start=%q end=%q", r.Type, r.ElementId, r.StartElementId, r.EndElementId)
	})

	add("RETURN p -> neo4j.Path", func() (checkStatus, string) {
		if _, err := write(`CREATE (:PS)-[:HOP]->(:PM)-[:HOP]->(:PE)`); err != nil {
			return fail("seed: %v", err)
		}
		rec, err := readOne(`MATCH p=(:PS)-[:HOP*2..2]->(:PE) RETURN p`)
		if err != nil {
			return fail("%v", err)
		}
		v, _ := rec.Get("p")
		p, ok := v.(neo4j.Path)
		if !ok {
			return fail("got %T, want neo4j.Path", v)
		}
		if len(p.Nodes) != 3 || len(p.Relationships) != 2 {
			return fail("path has %d nodes and %d relationships, want 3 and 2",
				len(p.Nodes), len(p.Relationships))
		}
		return pass("nodes=%d rels=%d", len(p.Nodes), len(p.Relationships))
	})

	add("GetRecordValue[neo4j.Node]", func() (checkStatus, string) {
		if _, err := write(`CREATE (:Typed {k: 1})`); err != nil {
			return fail("seed: %v", err)
		}
		rec, err := readOne(`MATCH (n:Typed) RETURN n`)
		if err != nil {
			return fail("%v", err)
		}
		n, _, err := neo4j.GetRecordValue[neo4j.Node](rec, "n")
		if err != nil {
			return fail("%v", err)
		}
		return pass("elementId=%q", n.ElementId)
	})

	add("elementId(n) matches Node.ElementId", func() (checkStatus, string) {
		if _, err := write(`CREATE (:EID {k: 1})`); err != nil {
			return fail("seed: %v", err)
		}
		rec, err := readOne(`MATCH (n:EID) RETURN n, elementId(n) AS eid`)
		if err != nil {
			return fail("%v", err)
		}
		nv, _ := rec.Get("n")
		n, ok := nv.(neo4j.Node)
		if !ok {
			return fail("node came back as %T", nv)
		}
		eidv, _ := rec.Get("eid")
		eid, _ := eidv.(string)
		if n.ElementId != eid {
			return fail("Node.ElementId=%q but elementId()=%q — the wire and the function disagree",
				n.ElementId, eid)
		}
		return pass("both %q", eid)
	})

	// ── write statistics (#2212 / #2190) ──

	add("ResultSummary.Counters (create)", func() (checkStatus, string) {
		sum, err := write(`CREATE (:Counted {a: 1, b: 2})`)
		if err != nil {
			return fail("%v", err)
		}
		c := sum.Counters()
		if !c.ContainsUpdates() || c.NodesCreated() != 1 || c.PropertiesSet() != 2 || c.LabelsAdded() != 1 {
			return fail("ContainsUpdates=%v NodesCreated=%d PropertiesSet=%d LabelsAdded=%d",
				c.ContainsUpdates(), c.NodesCreated(), c.PropertiesSet(), c.LabelsAdded())
		}
		return pass("nodes=1 props=2 labels=1 containsUpdates=true")
	})

	add("MERGE created-vs-matched observable", func() (checkStatus, string) {
		first, err := write(`MERGE (n:MergeProbe {k: 1})`)
		if err != nil {
			return fail("%v", err)
		}
		second, err := write(`MERGE (n:MergeProbe {k: 1})`)
		if err != nil {
			return fail("%v", err)
		}
		if !first.Counters().ContainsUpdates() || second.Counters().ContainsUpdates() {
			return fail("created=%v matched=%v — must differ",
				first.Counters().ContainsUpdates(), second.Counters().ContainsUpdates())
		}
		return pass("created reports updates, matched does not")
	})

	add("DELETE counters", func() (checkStatus, string) {
		if _, err := write(`CREATE (:DelProbe)`); err != nil {
			return fail("seed: %v", err)
		}
		sum, err := write(`MATCH (n:DelProbe) DELETE n`)
		if err != nil {
			return fail("%v", err)
		}
		if got := sum.Counters().NodesDeleted(); got != 1 {
			return fail("NodesDeleted=%d, want 1", got)
		}
		return pass("NodesDeleted=1")
	})

	add("relationship counters", func() (checkStatus, string) {
		sum, err := write(`CREATE (:RelC)-[:R]->(:RelD)`)
		if err != nil {
			return fail("%v", err)
		}
		if got := sum.Counters().RelationshipsCreated(); got != 1 {
			return fail("RelationshipsCreated=%d, want 1", got)
		}
		return pass("RelationshipsCreated=1")
	})

	add("index counters", func() (checkStatus, string) {
		sum, err := write(`CREATE INDEX idx_compat FOR (n:IdxProbe) ON (n.k)`)
		if err != nil {
			return fail("%v", err)
		}
		if got := sum.Counters().IndexesAdded(); got != 1 {
			return fail("IndexesAdded=%d, want 1", got)
		}
		return pass("IndexesAdded=1")
	})

	add("constraint counters", func() (checkStatus, string) {
		sum, err := write(`CREATE CONSTRAINT c_compat FOR (n:ConProbe) REQUIRE n.k IS UNIQUE`)
		if err != nil {
			return fail("%v", err)
		}
		if got := sum.Counters().ConstraintsAdded(); got != 1 {
			return fail("ConstraintsAdded=%d, want 1", got)
		}
		return pass("ConstraintsAdded=1")
	})

	add("read-only reports no updates", func() (checkStatus, string) {
		sum, err := write(`MATCH (n:Person) RETURN n`)
		if err != nil {
			return fail("%v", err)
		}
		if sum.Counters().ContainsUpdates() {
			return fail("a read-only MATCH reported ContainsUpdates=true")
		}
		return pass("ok")
	})

	// ── result summary surface ──

	add("ResultSummary.Query().Text()", func() (checkStatus, string) {
		sum, err := write(`RETURN 1 AS x`)
		if err != nil {
			return fail("%v", err)
		}
		if sum.Query().Text() != `RETURN 1 AS x` {
			return fail("got %q", sum.Query().Text())
		}
		return pass("%q", sum.Query().Text())
	})

	add("ResultSummary.Database().Name()", func() (checkStatus, string) {
		sum, err := write(`RETURN 1 AS x`)
		if err != nil {
			return fail("%v", err)
		}
		db := sum.Database()
		if db == nil {
			return fail("Database() is nil — Name() would panic")
		}
		return pass("%q", db.Name())
	})

	add("ResultSummary.StatementType", func() (checkStatus, string) {
		sum, err := write(`RETURN 1 AS x`)
		if err != nil {
			return fail("%v", err)
		}
		if sum.StatementType() == neo4j.StatementTypeUnknown {
			return fail("StatementTypeUnknown — the server does not send the type field")
		}
		return pass("%v", sum.StatementType())
	})

	add("ResultSummary timing (t_first/t_last)", func() (checkStatus, string) {
		sum, err := write(`RETURN 1 AS x`)
		if err != nil {
			return fail("%v", err)
		}
		if sum.ResultAvailableAfter() < 0 || sum.ResultConsumedAfter() < 0 {
			return degraded("available=%v consumed=%v — the server does not measure them",
				sum.ResultAvailableAfter(), sum.ResultConsumedAfter())
		}
		return pass("available=%v consumed=%v", sum.ResultAvailableAfter(), sum.ResultConsumedAfter())
	})

	add("ResultSummary.Notifications (cartesian)", func() (checkStatus, string) {
		sum, err := write(`MATCH (a), (b) RETURN a, b LIMIT 1`)
		if err != nil {
			return fail("%v", err)
		}
		if len(sum.Notifications()) == 0 {
			return fail("no notification for a cartesian product")
		}
		return pass("%d: %s", len(sum.Notifications()), sum.Notifications()[0].Code())
	})

	add("ResultSummary.Plan (EXPLAIN)", func() (checkStatus, string) {
		sum, err := write(`EXPLAIN MATCH (n) RETURN n`)
		if err != nil {
			return fail("%v", err)
		}
		if sum.Plan() == nil {
			return fail("Plan() is nil after EXPLAIN")
		}
		return pass("%s", sum.Plan().Operator())
	})

	add("ResultSummary.Profile (PROFILE)", func() (checkStatus, string) {
		sum, err := write(`PROFILE MATCH (n) RETURN n`)
		if err != nil {
			return fail("%v", err)
		}
		if sum.Profile() == nil {
			return fail("Profile() is nil after PROFILE")
		}
		return pass("%s", sum.Profile().Operator())
	})

	// ── sessions, transactions, routing ──

	add("neo4j.ExecuteQuery + EagerResult", func() (checkStatus, string) {
		out, err := neo4j.ExecuteQuery(ctx, driver,
			`MATCH (n:Person) RETURN n.name AS name`, nil,
			neo4j.EagerResultTransformer)
		if err != nil {
			return fail("%v", err)
		}
		return pass("keys=%v rows=%d", out.Keys, len(out.Records))
	})

	add("SessionConfig{DatabaseName}", func() (checkStatus, string) {
		err := withSession(neo4j.SessionConfig{DatabaseName: "neo4j"}, func(s neo4j.SessionWithContext) error {
			res, rerr := s.Run(ctx, `RETURN 1 AS x`, nil)
			if rerr != nil {
				return rerr
			}
			_, rerr = res.Single(ctx)
			return rerr
		})
		if err != nil {
			return fail("%v", err)
		}
		return pass("accepted")
	})

	add("SessionConfig{ImpersonatedUser} fails closed", func() (checkStatus, string) {
		err := withSession(neo4j.SessionConfig{ImpersonatedUser: "someone-else"},
			func(s neo4j.SessionWithContext) error {
				res, rerr := s.Run(ctx, `RETURN 1 AS x`, nil)
				if rerr != nil {
					return rerr
				}
				_, rerr = res.Consume(ctx)
				return rerr
			})
		if err == nil {
			return fail("impersonation was SILENTLY ACCEPTED — a client believing it runs " +
				"as a restricted user gets full authority instead")
		}
		return pass("refused: %v", err)
	})

	add("ExecuteWrite managed transaction", func() (checkStatus, string) {
		err := withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			_, rerr := s.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
				_, terr := tx.Run(ctx, `CREATE (:Managed)`, nil)
				return nil, terr
			})
			return rerr
		})
		if err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	add("FetchSize batching (PULL n)", func() (checkStatus, string) {
		if _, err := write(`UNWIND range(1, 10) AS i CREATE (:Batched {i: i})`); err != nil {
			return fail("seed: %v", err)
		}
		var n int
		if err := withSession(neo4j.SessionConfig{FetchSize: 2}, func(s neo4j.SessionWithContext) error {
			res, rerr := s.Run(ctx, `MATCH (n:Batched) RETURN n.i AS i`, nil)
			if rerr != nil {
				return rerr
			}
			recs, rerr := res.Collect(ctx)
			n = len(recs)
			return rerr
		}); err != nil {
			return fail("%v", err)
		}
		if n != 10 {
			return fail("got %d rows, want 10", n)
		}
		return pass("10 rows with FetchSize=2")
	})

	add("bookmarks chain across sessions", func() (checkStatus, string) {
		var bm []string
		if err := withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			if _, rerr := s.Run(ctx, `CREATE (:Bookmarked)`, nil); rerr != nil {
				return rerr
			}
			if _, rerr := s.Run(ctx, `RETURN 1`, nil); rerr != nil {
				return rerr
			}
			bm = s.LastBookmarks()
			return nil
		}); err != nil {
			return fail("%v", err)
		}
		if err := withSession(neo4j.SessionConfig{Bookmarks: bm}, func(s neo4j.SessionWithContext) error {
			_, rerr := s.Run(ctx, `MATCH (n:Bookmarked) RETURN count(n)`, nil)
			return rerr
		}); err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	// ── parameter round-trips ──

	add("nested list/map parameter round-trip", func() (checkStatus, string) {
		if err := writeParams(`RETURN $p AS p`, map[string]any{
			"p": map[string]any{"xs": []any{int64(1), int64(2)}, "m": map[string]any{"k": "v"}},
		}); err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	add("temporal parameter round-trip", func() (checkStatus, string) {
		if err := writeParams(`RETURN $d AS d`, map[string]any{
			"d": neo4j.Date(time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)),
		}); err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	add("bytes parameter round-trip", func() (checkStatus, string) {
		if err := writeParams(`RETURN $b AS b`, map[string]any{"b": []byte{1, 2, 3}}); err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	add("Point2D parameter round-trip", func() (checkStatus, string) {
		if err := writeParams(`RETURN $pt AS pt`, map[string]any{
			"pt": neo4j.Point2D{X: 1, Y: 2, SpatialRefId: 7203},
		}); err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	})

	// ── errors and recovery ──

	add("syntax error -> Neo4jError", func() (checkStatus, string) {
		err := withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			_, rerr := s.Run(ctx, `THIS IS NOT CYPHER`, nil)
			return rerr
		})
		if err == nil {
			return fail("no error for invalid Cypher")
		}
		if !neo4j.IsNeo4jError(err) {
			return fail("got %T, want a Neo4jError", err)
		}
		return pass("%v", err)
	})

	add("FAILED-state recovery after error", func() (checkStatus, string) {
		var detail string
		err := withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			if _, rerr := s.Run(ctx, `THIS IS NOT CYPHER`, nil); rerr == nil {
				detail = "the invalid statement did not error"
				return nil
			}
			res, rerr := s.Run(ctx, `RETURN 1 AS x`, nil)
			if rerr != nil {
				return fmt.Errorf("session unusable after an error: %w", rerr)
			}
			_, rerr = res.Single(ctx)
			return rerr
		})
		if detail != "" {
			return fail("%s", detail)
		}
		if err != nil {
			return fail("%v", err)
		}
		return pass("session reusable")
	})

	// ── introspection ──

	// collects runs a query and drains it, which is all the introspection checks need.
	collects := func(query string) (checkStatus, string) {
		err := withSession(neo4j.SessionConfig{}, func(s neo4j.SessionWithContext) error {
			res, rerr := s.Run(ctx, query, nil)
			if rerr != nil {
				return rerr
			}
			_, rerr = res.Collect(ctx)
			return rerr
		})
		if err != nil {
			return fail("%v", err)
		}
		return pass("ok")
	}

	add("SHOW INDEXES", func() (checkStatus, string) { return collects(`SHOW INDEXES`) })

	add("db.labels()", func() (checkStatus, string) { return collects(`CALL db.labels()`) })

	add("SHOW TRANSACTIONS", func() (checkStatus, string) { return collects(`SHOW TRANSACTIONS`) })

	add("CALL dbms.components()", func() (checkStatus, string) { return collects(`CALL dbms.components()`) })

	// ── report and ratchet ──

	var passed, failed, degradedN, skipped int
	byStatus := map[checkStatus][]string{}
	for _, r := range results {
		switch r.status {
		case statusPass:
			passed++
		case statusFail:
			failed++
		case statusDegraded:
			degradedN++
		default:
			skipped++
		}
		byStatus[r.status] = append(byStatus[r.status],
			fmt.Sprintf("%-10s %-46s %s", r.status, r.name, r.detail))
	}

	t.Logf("driver compatibility: %d checks — PASS=%d FAIL=%d DEGRADED=%d SKIP=%d (floor=%d)",
		len(results), passed, failed, degradedN, skipped, compatPassFloor)
	for _, st := range []checkStatus{statusFail, statusDegraded, statusSkip, statusPass} {
		lines := byStatus[st]
		sort.Strings(lines)
		for _, l := range lines {
			t.Log("  " + l)
		}
	}

	if passed < compatPassFloor {
		t.Fatalf("driver compatibility REGRESSED to %d/%d passing, below the recorded floor "+
			"of %d; see the FAIL lines above", passed, len(results), compatPassFloor)
	}
	if passed > compatPassFloor {
		t.Logf("NOTE: %d checks now pass, above the floor of %d — raise compatPassFloor to %d "+
			"so the win is locked in", passed, compatPassFloor, passed)
	}
}
