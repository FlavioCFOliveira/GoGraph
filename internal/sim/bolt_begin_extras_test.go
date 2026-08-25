package sim

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// TestBoltBeginExtras_Clean drives the whole BEGIN extras surface against a real
// server and asserts both the contract and the non-vacuity gate are satisfied.
func TestBoltBeginExtras_Clean(t *testing.T) {
	defer goleak.VerifyNone(t)

	ev, err := RunBoltBeginExtras(context.Background(), boltBeginExtrasDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltBeginExtras: %v", err)
	}
	for _, v := range checkBoltBeginExtras(ev) {
		t.Errorf("contract violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	for _, v := range checkBoltBeginExtrasNonVacuity(ev) {
		t.Errorf("non-vacuity violation: %s %s: %s", v.Kind, v.Op, v.Message)
	}
	if t.Failed() {
		t.Log(ev.String())
	}
}

// TestBoltBeginExtras_Deterministic asserts the same seed produces byte-identical
// evidence, which is what makes a failure replayable.
//
// It compares the RENDERING rather than the struct, which is only sound because the
// rendering was swept for everything not reachable from the seed: the real-time
// durations, the process-global bookmark text, and the harness error text. See
// [BoltBeginExtrasEvidence.String]. The struct is spot-checked afterwards on the two
// excluded classes, so the sweep is asserted rather than trusted.
func TestBoltBeginExtras_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)

	const seed = 0x2485_D0_D0
	first, err := RunBoltBeginExtras(context.Background(), seed)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := RunBoltBeginExtras(context.Background(), seed)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("two runs of seed %#x rendered differently:\n--- first ---\n%s\n--- second ---\n%s",
			seed, first.String(), second.String())
	}
	// The excluded classes must be excluded for the RIGHT reason: the bookmark text
	// really does differ between two runs in the same process (the counter is global),
	// which is the evidence that rendering it would have made this test flaky rather
	// than merely redundant.
	if len(first.IssuedBookmarks) > 0 && len(second.IssuedBookmarks) > 0 {
		if first.IssuedBookmarks[0] == second.IssuedBookmarks[0] {
			t.Errorf("two runs issued the SAME first bookmark %q. bookmarkCounter is process-global "+
				"(bolt/server/bookmark.go:13), so this should be impossible; if it is now per-server, the "+
				"rendering may include the literal text and this exclusion should be revisited",
				first.IssuedBookmarks[0])
		}
	}
}

// TestBoltBeginExtras_ScenarioPasses drives the catalogue scenario end to end, so the
// registry wiring — not just the collector — is covered.
func TestBoltBeginExtras_ScenarioPasses(t *testing.T) {
	defer goleak.VerifyNone(t)

	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioBoltBeginExtras)
	if !ok {
		t.Fatalf("scenario %q not in catalogue", ScenarioBoltBeginExtras)
	}
	rep, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("scenario run: %v", err)
	}
	if rep != nil {
		t.Errorf("scenario failed:\n%s", rep.String())
	}
}

// cleanBeginExtrasEvidence returns a hand-built evidence value that BOTH checkers
// pass, so a perturbation test can attribute any violation to the field it changed.
//
// It is built from the constants the collector uses rather than from literals, so a
// change to a roster or a bound cannot leave the fixture healthy while the real run
// diverges from it.
func cleanBeginExtrasEvidence() BoltBeginExtrasEvidence {
	const committed = 5
	ev := BoltBeginExtrasEvidence{
		Seed:                          1,
		CommittedNodes:                committed,
		IssuedBookmarks:               []string{"FB:k00000001", "FB:k00000002"},
		AutocommitBookmarkFresh:       "",
		AutocommitBookmarkAfterCommit: "FB:k00000002",
		AutocommitAfterCommitExpected: "FB:k00000002",
		AutocommitStatsSeen:           true,
	}
	for _, name := range beginBookmarkArmNames {
		a := BoltBookmarkArm{Name: name, Accepted: true, Observed: committed, BeginElapsed: time.Millisecond}
		switch name {
		case "no-bookmark-baseline":
			// The key is omitted; the server sees no tokens.
		case "real-issued-bookmark":
			a.SentKey, a.SentToken, a.ServerSawTokens = true, beginIssuedTag, 1
		case "fabricated-far-future":
			a.SentKey, a.SentToken, a.ServerSawTokens = true, beginFabricatedFarFuture, 1
		case "fabricated-unparseable":
			a.SentKey, a.SentToken, a.ServerSawTokens = true, beginFabricatedUnparseable, 1
		case "fabricated-wrong-type":
			a.SentKey, a.SentToken, a.ServerSawTokens = true, "<int64>", 0
		}
		ev.Bookmarks = append(ev.Bookmarks, a)
	}
	for i := range beginTimeoutPlans {
		p := &beginTimeoutPlans[i]
		a := BoltTimeoutArm{
			Name: p.name, SentKey: p.sendKey, SentTimeoutMS: p.timeoutMS,
			IdleBound: p.idleBound, DefaultTotalBound: p.defTotal,
			// Cloned for the same reason the collector clones it: these subtests run in
			// parallel and one that perturbs Advanced must not rewrite the shared plan.
			Advanced: slices.Clone(p.advances), ReapedAfter: -1, TimersArmed: 1,
		}
		switch p.name {
		case "no-tx-timeout-control":
			a.Committed = true
		case "overflow-tx-timeout":
			a.ReapedAfter = 1
		default:
			a.ReapedAfter = 0
		}
		if a.reaped() {
			a.GotCode, a.GotMessage = txReapFailureCode, txReapFailureMessage
			a.SecondReplyKind, a.ReplyElapsed = "IGNORED", time.Millisecond
		}
		ev.Timeouts = append(ev.Timeouts, a)
	}
	for _, name := range beginModeArmNames {
		a := BoltModeArm{Name: "mode=" + name, RegistryMode: beginModeWrite, WriteAccepted: true}
		if name != beginModeAbsentTag {
			a.SentKey, a.SentMode = true, name
		}
		if name == beginModeRead {
			a.RegistryMode, a.WriteAccepted = beginModeRead, false
			a.WriteCode = beginReadOnlyRefusalCode
			a.WriteMessage = "cypher: write or DDL statement not allowed in a read-only transaction"
		}
		ev.Modes = append(ev.Modes, a)
	}
	for _, name := range beginDBArmNames {
		reported := server.DefaultDatabaseName
		a := BoltDBArm{Name: "db=" + name, CommitMetaKeys: []string{metaKeyBookmark}}
		if name != beginDBAbsentTag {
			a.SentKey, a.SentDB, reported = true, name, name
		}
		a.ReportedDB, a.ReportedOnRun = reported, reported
		ev.DBs = append(ev.DBs, a)
	}
	addr := "sim-listener"
	ev.Route = BoltRouteObs{
		ListenerAddr: addr, SentDB: beginDBForeign, SentBookmark: beginFabricatedFarFuture,
		Roles: slices.Clone(beginExpectedRoles), RoleCount: len(beginExpectedRoles),
		AddressesByRole: map[string][]string{"WRITE": {addr}, "READ": {addr}, "ROUTE": {addr}},
		TTL:             beginRoutingTableTTL, TTLPresent: true, TableDB: "",
		PopulatedRender: "table", ZeroMessageRender: "table", Accepted: true,
	}
	ev.Metadata = BoltMetadataObs{
		SentKeys: []string{beginTxMetadataKey, "gograph.dst.seed"}, Accepted: true,
		BeginMetaKeys:    []string{},
		TerminalMetaKeys: []string{metaKeyBookmark, metaKeyDB, metaKeyHasMore},
		CommitMetaKeys:   []string{metaKeyBookmark},
	}
	return ev
}

// bookmarkArmIndex returns the index of the named causal-read arm. It PANICS when the
// arm is absent rather than calling t.Fatalf: every caller is a mutate closure running
// inside a t.Run subtest, and testing requires Fatalf to be called on the goroutine of
// the test it belongs to. A missing arm is a defect in the hand-built fixture, not a
// test outcome, so a panic is the honest signal and it names the arm.
func bookmarkArmIndex(e *BoltBeginExtrasEvidence, name string) int {
	for i := range e.Bookmarks {
		if e.Bookmarks[i].Name == name {
			return i
		}
	}
	panic("sim: test fixture has no causal-read arm named " + name)
}

// timeoutArmIndex, modeArmIndex and dbArmIndex are the same accessor for the other
// three families. Each panics for the same reason.
func timeoutArmIndex(e *BoltBeginExtrasEvidence, name string) int {
	for i := range e.Timeouts {
		if e.Timeouts[i].Name == name {
			return i
		}
	}
	panic("sim: test fixture has no timeout arm named " + name)
}

func modeArmIndex(e *BoltBeginExtrasEvidence, sent string) int {
	for i := range e.Modes {
		if e.Modes[i].Name == "mode="+sent {
			return i
		}
	}
	panic("sim: test fixture has no mode arm for " + sent)
}

func dbArmIndex(e *BoltBeginExtrasEvidence, sent string) int {
	for i := range e.DBs {
		if e.DBs[i].Name == "db="+sent {
			return i
		}
	}
	panic("sim: test fixture has no db arm for " + sent)
}

// TestBoltBeginExtras_OracleCanFail is the falsifiability proof for the CONTRACT
// checker: the hand-built evidence passes, and every single-field perturbation of it is
// caught by the clause it stands for. An oracle that cannot fail proves nothing, so each
// mutation below names the defect it represents.
func TestBoltBeginExtras_OracleCanFail(t *testing.T) {
	t.Parallel()

	base := cleanBeginExtrasEvidence()
	if v := checkBoltBeginExtras(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass the contract, got %d violation(s): %v", len(v), v)
	}
	if v := checkBoltBeginExtrasNonVacuity(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass non-vacuity, got %d violation(s): %v", len(v), v)
	}

	cases := []struct {
		name   string
		mutate func(*BoltBeginExtrasEvidence)
		wantOp string
	}{
		// --- the causal-read family ---
		{
			name: "a fabricated bookmark was REFUSED (the server started honouring bookmarks)",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Bookmarks[bookmarkArmIndex(e, "fabricated-far-future")]
				a.Accepted, a.GotCode = false, "Neo.ClientError.Transaction.InvalidBookmark"
			},
			wantOp: "bookmark-accepted",
		},
		{
			name: "the causal read could not be completed",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Bookmarks[bookmarkArmIndex(e, "real-issued-bookmark")].ReadFailed = "census PULL: broken pipe"
			},
			wantOp: "bookmark-read-completed",
		},
		{
			name: "the causal read missed a committed node",
			mutate: func(e *BoltBeginExtrasEvidence) {
				for i := range e.Bookmarks {
					e.Bookmarks[i].Observed--
				}
			},
			wantOp: "bookmark-causal-read",
		},
		{
			name: "BEGIN WAITED on an unknown bookmark",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Bookmarks[bookmarkArmIndex(e, "fabricated-far-future")].BeginElapsed = beginReplyBound + time.Second
			},
			wantOp: "bookmark-does-not-wait",
		},
		{
			name: "the bookmark token CHANGED what the reader saw",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Bookmarks[bookmarkArmIndex(e, "fabricated-unparseable")].Observed = 0
			},
			wantOp: "bookmark-token-changes-nothing",
		},
		{
			name: "the server issued a counter at or above the 'far future' token",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.IssuedBookmarks = append(e.IssuedBookmarks, beginFabricatedFarFuture)
			},
			wantOp: "bookmark-fabricated-is-ahead",
		},
		// --- bookmark delivery ---
		{
			name:   "a COMMIT returned a malformed bookmark",
			mutate: func(e *BoltBeginExtrasEvidence) { e.IssuedBookmarks[1] = "bookmark-2" },
			wantOp: "bookmark-shape",
		},
		{
			name:   "two successive COMMITs minted the same counter",
			mutate: func(e *BoltBeginExtrasEvidence) { e.IssuedBookmarks[1] = e.IssuedBookmarks[0] },
			wantOp: "bookmark-strictly-advances",
		},
		{
			name:   "an autocommit statement minted its own bookmark",
			mutate: func(e *BoltBeginExtrasEvidence) { e.AutocommitBookmarkFresh = "FB:k00000009" },
			wantOp: "autocommit-bookmark-is-empty",
		},
		{
			name:   "the autocommit bookmark stopped echoing the prior COMMIT's",
			mutate: func(e *BoltBeginExtrasEvidence) { e.AutocommitBookmarkAfterCommit = "FB:k0000000a" },
			wantOp: "autocommit-bookmark-is-stale",
		},
		{
			name:   "the autocommit statement wrote nothing, so the empty bookmark is unattributable",
			mutate: func(e *BoltBeginExtrasEvidence) { e.AutocommitStatsSeen = false },
			wantOp: "autocommit-did-write",
		},
		// --- tx_timeout ---
		{
			name: "the client's tx_timeout did not reap the transaction",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")]
				a.ReapedAfter, a.GotCode, a.GotMessage, a.SecondReplyKind = -1, "", "", ""
				a.Committed = true
			},
			wantOp: "txtimeout-reaps",
		},
		{
			name: "THE CONTROL was reaped, so the subject's reap attributes nothing",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Timeouts[timeoutArmIndex(e, "no-tx-timeout-control")]
				a.ReapedAfter, a.Committed = 0, false
				a.GotCode, a.GotMessage, a.SecondReplyKind = txReapFailureCode, txReapFailureMessage, "IGNORED"
			},
			wantOp: "txtimeout-control-survives",
		},
		{
			name: "the surviving control's COMMIT was refused",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Timeouts[timeoutArmIndex(e, "no-tx-timeout-control")]
				a.Committed, a.GotCode = false, "Neo.DatabaseError.General.UnknownError"
			},
			wantOp: "txtimeout-control-commits",
		},
		{
			name: "the abort carried the wrong code",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")].GotCode = "Neo.ClientError.Request.Invalid"
			},
			wantOp: "txtimeout-abort-typed",
		},
		{
			name: "the abort carried the wrong message",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")].GotMessage = "transaction timed out"
			},
			wantOp: "txtimeout-abort-typed",
		},
		{
			name: "the termination was delivered twice instead of once",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")].SecondReplyKind = "FAILURE"
			},
			wantOp: "txtimeout-abort-delivered-once",
		},
		{
			name: "the transaction STALLED instead of aborting",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")].ReplyElapsed = beginReplyBound + time.Second
			},
			wantOp: "txtimeout-aborts-not-stalls",
		},
		{
			name: "a reaped arm's reaper was never armed on the injected clock",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")].TimersArmed = 0
			},
			wantOp: "txtimeout-timer-was-armed",
		},
		{
			name: "the idle collision control was not reaped",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Timeouts[timeoutArmIndex(e, "idle-bound-control")]
				a.ReapedAfter, a.GotCode, a.GotMessage, a.SecondReplyKind = -1, "", "", ""
			},
			wantOp: "txtimeout-idle-control-reaps",
		},
		{
			name: "a total-bound and an idle-bound abort became distinguishable",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "idle-bound-control")].GotMessage =
					"the transaction has been terminated because it was idle too long"
			},
			wantOp: "txtimeout-shares-one-failure",
		},
		{
			name: "an overflowing tx_timeout disarmed the reaper entirely",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Timeouts[timeoutArmIndex(e, "overflow-tx-timeout")]
				a.ReapedAfter, a.GotCode, a.GotMessage, a.SecondReplyKind = -1, "", "", ""
				a.Committed = true
			},
			wantOp: "txtimeout-overflow-keeps-default",
		},
		{
			name: "an overflowing tx_timeout was honoured as a SHORTER bound",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "overflow-tx-timeout")].ReapedAfter = 0
			},
			wantOp: "txtimeout-overflow-keeps-default",
		},
		// --- mode ---
		{
			name: "the registry recorded a write transaction as read-only",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Modes[modeArmIndex(e, beginModeUpperR)].RegistryMode = beginModeRead
			},
			wantOp: "mode-registry-agrees",
		},
		{
			name: "a read-only transaction ACCEPTED a write",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Modes[modeArmIndex(e, beginModeRead)]
				a.WriteAccepted, a.WriteCode, a.WriteMessage = true, "", ""
			},
			wantOp: "mode-read-refuses-write",
		},
		{
			name: "the read-only refusal message stopped naming the access mode",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Modes[modeArmIndex(e, beginModeRead)].WriteMessage = "cypher: statement not allowed"
			},
			wantOp: "mode-read-refuses-write",
		},
		{
			name: "an unknown mode stopped failing open",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Modes[modeArmIndex(e, beginModeNonsense)]
				a.WriteAccepted, a.WriteCode = false, beginReadOnlyRefusalCode
			},
			wantOp: "mode-unknown-fails-open",
		},
		// --- db ---
		{
			name: "a foreign database name stopped being echoed",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.DBs[dbArmIndex(e, beginDBForeign)].ReportedDB = server.DefaultDatabaseName
			},
			wantOp: "db-echoed-unvalidated",
		},
		{
			name: "the RUN SUCCESS and the terminal PULL SUCCESS disagree on the name",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.DBs[dbArmIndex(e, beginDBSystem)].ReportedOnRun = server.DefaultDatabaseName
			},
			wantOp: "db-run-and-terminal-agree",
		},
		{
			name: "the reported database name is EMPTY (the rmp #2172 driver panic)",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.DBs[dbArmIndex(e, beginDBAbsentTag)]
				a.ReportedDB, a.ReportedOnRun = "", ""
			},
			wantOp: "db-never-empty",
		},
		{
			name: "the COMMIT reply widened beyond the bookmark",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.DBs[dbArmIndex(e, beginDBForeign)]
				a.CommitMetaKeys = []string{metaKeyBookmark, metaKeyDB}
			},
			wantOp: "db-absent-from-commit",
		},
		// --- ROUTE ---
		{
			name: "ROUTE was refused",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Route.Accepted, e.Route.GotCode = false, "Neo.ClientError.Request.Invalid"
			},
			wantOp: "route-accepted",
		},
		{
			name: "the routing table dropped a role",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Route.Roles = []string{"WRITE", "READ"}
				e.Route.RoleCount = 2
				delete(e.Route.AddressesByRole, "ROUTE")
			},
			wantOp: "route-roles",
		},
		{
			name: "the routing table advertises a host that is not this server",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Route.AddressesByRole["WRITE"] = []string{"localhost:7687"}
			},
			wantOp: "route-names-this-server",
		},
		{
			name:   "the routing table's TTL changed",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Route.TTL = 30 },
			wantOp: "route-ttl",
		},
		{
			name:   "the routing table carried no TTL at all",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Route.TTLPresent = false },
			wantOp: "route-ttl",
		},
		{
			name:   "ROUTE started honouring the database it was asked for",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Route.TableDB = beginDBForeign },
			wantOp: "route-db-is-dropped",
		},
		{
			name:   "ROUTE's routing context or bookmarks changed the answer",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Route.ZeroMessageRender = "different-table" },
			wantOp: "route-ignores-context-and-bookmarks",
		},
		// --- tx_metadata ---
		{
			name: "BEGIN carrying tx_metadata was refused",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Metadata.Accepted, e.Metadata.GotCode = false, "Neo.ClientError.Request.Invalid"
			},
			wantOp: "metadata-accepted",
		},
		{
			name: "the terminal PULL SUCCESS echoed a tx_metadata key",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Metadata.TerminalMetaKeys = append(e.Metadata.TerminalMetaKeys, beginTxMetadataKey)
			},
			wantOp: "metadata-echoed-nowhere",
		},
		{
			name: "the COMMIT SUCCESS echoed a tx_metadata key",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Metadata.CommitMetaKeys = append(e.Metadata.CommitMetaKeys, beginTxMetadataKey)
			},
			wantOp: "metadata-echoed-nowhere",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := cleanBeginExtrasEvidence()
			tc.mutate(&ev)
			v := checkBoltBeginExtras(&ev)
			if len(v) == 0 {
				t.Fatalf("the contract checker did NOT fire for %q: the clause %s cannot fail",
					tc.name, boltBeginOp(tc.wantOp))
			}
			wantOp := boltBeginOp(tc.wantOp)
			for _, got := range v {
				if got.Op == wantOp {
					return
				}
			}
			t.Errorf("the contract checker fired, but not on %s. Got:\n%s", wantOp, renderViolations(v))
		})
	}
}

// TestBoltBeginExtras_NonVacuityCanFail is the falsifiability proof for the GATE. It is
// a separate table from the contract's because the two checkers answer different
// questions — did the server misbehave, and was the run in a position to notice — and a
// gate clause that cannot fire is a gate that certifies nothing.
func TestBoltBeginExtras_NonVacuityCanFail(t *testing.T) {
	t.Parallel()

	base := cleanBeginExtrasEvidence()
	if v := checkBoltBeginExtrasNonVacuity(&base); len(v) != 0 {
		t.Fatalf("clean evidence must pass the gate, got %d violation(s):\n%s", len(v), renderViolations(v))
	}

	cases := []struct {
		name   string
		mutate func(*BoltBeginExtrasEvidence)
		wantOp string
	}{
		{
			name:   "a causal-read arm was dropped from the roster",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Bookmarks = e.Bookmarks[:len(e.Bookmarks)-1] },
			wantOp: "nv-bookmark-roster",
		},
		{
			name: "the writer committed nothing, so every causal read trivially passes",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.CommittedNodes = 0
				for i := range e.Bookmarks {
					e.Bookmarks[i].Observed = 0
				}
			},
			wantOp: "nv-bookmark-write-nonempty",
		},
		{
			name: "no fabricated bookmark was presented, so the token proves nothing",
			mutate: func(e *BoltBeginExtrasEvidence) {
				for i := range e.Bookmarks {
					if e.Bookmarks[i].SentKey && e.Bookmarks[i].SentToken != beginIssuedTag {
						e.Bookmarks[i].SentKey, e.Bookmarks[i].SentToken = false, ""
					}
				}
			},
			wantOp: "nv-bookmark-contrast",
		},
		{
			name: "only one arm completed its read, so the cross-arm comparison is self-comparison",
			mutate: func(e *BoltBeginExtrasEvidence) {
				for i := range e.Bookmarks[1:] {
					e.Bookmarks[i+1].ReadFailed = "census PULL: broken pipe"
				}
			},
			wantOp: "nv-bookmark-arms-completed",
		},
		{
			name:   "one bookmark cannot falsify a strict-advance clause",
			mutate: func(e *BoltBeginExtrasEvidence) { e.IssuedBookmarks = e.IssuedBookmarks[:1] },
			wantOp: "nv-bookmark-sequence",
		},
		{
			name: "the stale-bookmark reference is empty, so that clause compares two empty strings",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.AutocommitAfterCommitExpected, e.AutocommitBookmarkAfterCommit = "", ""
			},
			wantOp: "nv-bookmark-stale-reference",
		},
		{
			name:   "a timeout arm was dropped from the roster",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Timeouts = e.Timeouts[:2] },
			wantOp: "nv-timeout-roster",
		},
		{
			name: "every timeout arm was reaped, so the family cannot tell a bound from a runaway reaper",
			mutate: func(e *BoltBeginExtrasEvidence) {
				a := &e.Timeouts[timeoutArmIndex(e, "no-tx-timeout-control")]
				a.ReapedAfter, a.Committed = 0, false
				a.GotCode, a.GotMessage, a.SecondReplyKind = txReapFailureCode, txReapFailureMessage, "IGNORED"
			},
			wantOp: "nv-timeout-both-outcomes",
		},
		{
			name: "THE CONTROL survived with no timer armed, so 'not reaped' attributes nothing",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "no-tx-timeout-control")].TimersArmed = 0
			},
			wantOp: "nv-timeout-reaper-armed",
		},
		{
			name: "an arm advanced no virtual time at all",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "no-tx-timeout-control")].Advanced = nil
			},
			wantOp: "nv-timeout-advance-nonzero",
		},
		{
			name: "the subject arm's idle bound fell WITHIN its advance, so it measures the wrong reaper",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Timeouts[timeoutArmIndex(e, "client-tx-timeout")].IdleBound = beginTxTimeoutSmall / 2
			},
			wantOp: "nv-timeout-bounds-separated",
		},
		{
			name:   "a mode arm was dropped from the roster",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Modes = e.Modes[:len(e.Modes)-1] },
			wantOp: "nv-mode-roster",
		},
		{
			name: "no transaction was recorded read-only, so the fail-open pin is one-sided",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Modes[modeArmIndex(e, beginModeRead)].RegistryMode = beginModeWrite
			},
			wantOp: "nv-mode-both-sides",
		},
		{
			name: "no write was accepted anywhere, so the read-only refusal is unattributable",
			mutate: func(e *BoltBeginExtrasEvidence) {
				for i := range e.Modes {
					e.Modes[i].WriteAccepted = false
				}
			},
			wantOp: "nv-mode-write-observed",
		},
		{
			name:   "a database arm was dropped from the roster",
			mutate: func(e *BoltBeginExtrasEvidence) { e.DBs = e.DBs[:len(e.DBs)-1] },
			wantOp: "nv-db-roster",
		},
		{
			name: "every arm named the server's own database, so an echo and a fallback are the same reading",
			mutate: func(e *BoltBeginExtrasEvidence) {
				for i := range e.DBs {
					e.DBs[i].SentKey, e.DBs[i].SentDB = true, server.DefaultDatabaseName
					e.DBs[i].ReportedDB, e.DBs[i].ReportedOnRun = server.DefaultDatabaseName, server.DefaultDatabaseName
				}
			},
			wantOp: "nv-db-contrast",
		},
		{
			name:   "the listener reported no address, so every advertised address is compared against \"\"",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Route.ListenerAddr = "" },
			wantOp: "nv-route-reference",
		},
		{
			name:   "ROUTE named no database, so an empty table label is equally consistent with an echo",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Route.SentDB = "" },
			wantOp: "nv-route-asked-for-a-db",
		},
		{
			name: "the routing table did not decode, so its clauses adjudicate zero values",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Route.RoleCount, e.Route.AddressesByRole = 0, nil
			},
			wantOp: "nv-route-table-decoded",
		},
		{
			name: "both routing-table renderings are empty, so the identical-answer clause compares nothing",
			mutate: func(e *BoltBeginExtrasEvidence) {
				e.Route.PopulatedRender, e.Route.ZeroMessageRender = "", ""
			},
			wantOp: "nv-route-renders-nonempty",
		},
		{
			name:   "no tx_metadata key was sent, so 'nothing echoed it' is vacuously true",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Metadata.SentKeys = nil },
			wantOp: "nv-metadata-sent",
		},
		{
			name:   "the replies were never decoded, so the echo search ran over empty lists",
			mutate: func(e *BoltBeginExtrasEvidence) { e.Metadata.TerminalMetaKeys = nil },
			wantOp: "nv-metadata-replies-read",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := cleanBeginExtrasEvidence()
			tc.mutate(&ev)
			v := checkBoltBeginExtrasNonVacuity(&ev)
			if len(v) == 0 {
				t.Fatalf("the non-vacuity gate did NOT fire for %q: the clause %s cannot fail",
					tc.name, boltBeginOp(tc.wantOp))
			}
			wantOp := boltBeginOp(tc.wantOp)
			for _, got := range v {
				if got.Op == wantOp {
					return
				}
			}
			t.Errorf("the gate fired, but not on %s. Got:\n%s", wantOp, renderViolations(v))
		})
	}
}

// TestBoltBeginExtras_ServerWouldHaveKeptTheFabricatedToken is the control that makes
// the whole bookmark pin mean something.
//
// "The server accepted a fabricated bookmark" is only evidence that the token was SEEN
// AND IGNORED if the server's own extractor would have kept it. Were
// [server.ExtractBookmarks] filtering it out first, acceptance would prove nothing
// about honouring bookmarks — the server would simply never have had a token to honour.
// So the harness's own ServerSawTokens bookkeeping is checked against the exported
// extractor the two BEGIN and RUN call sites actually use, rather than being asserted
// from the token's appearance.
func TestBoltBeginExtras_ServerWouldHaveKeptTheFabricatedToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		token packstream.Value
		want  []string
	}{
		{"far-future", beginFabricatedFarFuture, []string{beginFabricatedFarFuture}},
		{"unparseable", beginFabricatedUnparseable, []string{beginFabricatedUnparseable}},
		// The wrong-type element is filtered by the extractor, which is exactly why that
		// arm records ServerSawTokens = 0: it probes a DIFFERENT branch from a well-typed
		// unknown token, and a clause that treated the two alike would be wrong.
		{"wrong-type", beginBookmarkWrongTypeValue, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := server.ExtractBookmarks(map[string]packstream.Value{
				beginKeyBookmarks: []packstream.Value{tc.token},
			})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ExtractBookmarks(%#v) = %v, want %v: the harness's ServerSawTokens bookkeeping and the "+
					"server's own extractor disagree, so the causal-read arms are not probing what they claim",
					tc.token, got, tc.want)
			}
		})
	}

	// And the bookkeeping in the collector must match, arm for arm.
	ev, err := RunBoltBeginExtras(context.Background(), boltBeginExtrasDefaultSeed)
	if err != nil {
		t.Fatalf("RunBoltBeginExtras: %v", err)
	}
	for i := range ev.Bookmarks {
		a := &ev.Bookmarks[i]
		want := 0
		if a.SentKey && a.SentToken != "<int64>" {
			want = 1
		}
		if a.ServerSawTokens != want {
			t.Errorf("arm %q recorded ServerSawTokens=%d, want %d", a.Name, a.ServerSawTokens, want)
		}
	}
}

// TestBoltBeginExtras_SeedMixDoesNotCancelTheDefaultSeed guards the one value
// [beginSeedMix] may not take.
//
// XOR is self-annihilating, so a mix equal to a seed makes the mix a NO-OP at exactly
// that seed. When the seed in question is the catalogue default, the decorrelation the
// constant exists to provide is absent from the one run every report, sweep and
// reproduction starts from — and absent silently, because nothing else observes it.
//
// It is not a theoretical trap: the constant shipped as 0x2485_B0_0C against a default
// of 0x2485_B00C, which Go reads as the SAME number, digit separators being cosmetic and
// implying no grouping. Comparing the two constants is the cheapest guard there is, and
// it doubles as the documentation of a mistake that is invisible on inspection.
func TestBoltBeginExtras_SeedMixDoesNotCancelTheDefaultSeed(t *testing.T) {
	t.Parallel()

	if effective := uint64(boltBeginExtrasDefaultSeed) ^ uint64(beginSeedMix); effective == 0 {
		t.Fatalf("beginSeedMix (%#x) equals boltBeginExtrasDefaultSeed (%#x), so the catalogue default draws "+
			"from NewSeed(0) and the mix decorrelates nothing on the one run every report starts from; "+
			"pick a mix that differs from the default seed",
			uint64(beginSeedMix), uint64(boltBeginExtrasDefaultSeed))
	}
}

// TestBoltBeginExtras_IssuedRenderingIgnoresCounterValues pins the property that
// [BoltBeginExtrasEvidence.renderIssued] must hold for the evidence rendering to be a
// function of the seed: it depends on neither the ABSOLUTE bookmark counters nor the
// GAPS between them.
//
// Two evidence values differing ONLY in IssuedBookmarks must render identically — one
// with consecutive counters, one with large, unevenly spaced ones whose implied advances
// differ from each other and from 1. The malformed slot is present in both so the
// <malformed> path is covered by the same equality rather than by a separate case.
//
// # Why this shape of test, and not the one that found the defect
//
// The defect was real and is recorded in docs/dst-feature-coverage.md: renderIssued used
// to emit the advance (#1=<issued,+1>) on the reasoning that a gap, unlike an absolute
// value, is seed-determined. It is not — bookmarkCounter is process-global
// (bolt/server/bookmark.go:13) and `sim -swarm -workers N` runs N scenarios concurrently
// in ONE process (internal/sim/swarm.go:271-278). MEASURED over six fixed seeds, the
// advance read +1 at workers=1 and +5, +6 or +7 at workers=6: six of six seeds rendered
// differently at the two worker counts.
//
// That concurrent probe is deliberately NOT the permanent guard. It reproduces the fault
// through the SCHEDULER, so it is slow, load-dependent, and would go quiet on a fast or
// lightly loaded machine — a test that stops failing when the box gets quicker is worse
// than no test, because it reports green for a reason unrelated to the property. This
// one asserts the property directly, with no concurrency and no server, in microseconds,
// and it cannot fall silent. [TestBoltBeginExtras_Deterministic] cannot substitute for
// it either: that test compares two SERIAL runs, which is exactly the condition under
// which the old rendering was stable.
func TestBoltBeginExtras_IssuedRenderingIgnoresCounterValues(t *testing.T) {
	t.Parallel()

	// Same slot count, same malformed slot, same everything but the counters. The
	// consecutive set implies advances of +1 throughout; the spaced set implies +1192
	// and +122222, so a rendering that leaked either the values or the gaps must differ.
	consecutive := BoltBeginExtrasEvidence{
		IssuedBookmarks: []string{"FB:k00000001", "FB:k00000002", "FB:k00000003", "not-a-bookmark"},
	}
	spaced := BoltBeginExtrasEvidence{
		IssuedBookmarks: []string{"FB:k0000002a", "FB:k000004d2", "FB:k0001e240", "not-a-bookmark"},
	}

	got, other := consecutive.renderIssued(), spaced.renderIssued()
	if got != other {
		t.Fatalf("renderIssued depends on the bookmark counters: consecutive counters render %q but "+
			"unevenly spaced ones render %q. The counter is process-global (bolt/server/bookmark.go:13), so "+
			"anything derived from its values — the absolutes OR the gaps — makes the evidence rendering "+
			"non-deterministic under any concurrent run", got, other)
	}
	// Non-vacuity: equality alone would also hold if renderIssued returned a constant, so
	// pin the shape it must produce — every slot present, by ordinal, with the malformed
	// one marked as such.
	const want = "#0=<issued> #1=<issued> #2=<issued> #3=<malformed>"
	if got != want {
		t.Fatalf("renderIssued produced %q, want %q", got, want)
	}
}

// TestWireClientBeginExtras_NilExtrasNeedsNoNormalisation guards the assumption
// [WireClient.BeginExtras] rests on when it hands the caller's map to [proto.Begin]
// unchanged: a nil extras map and an empty one encode to the SAME bytes, so
// [WireClient.Begin] stays byte-identical to what it sent before BeginExtras existed
// without the method normalising anything.
//
// That equality is a measured property of the encoder, not a tautology, and this guard
// can fail. It holds only because a typed nil map[string]packstream.Value boxed into an
// interface misses the encoder's `case nil` arm (bolt/packstream/value.go:73) and lands
// on `case map[string]Value` (bolt/packstream/value.go:99-110), where len and range over
// a nil map yield 0 and no iterations. An encoder that grew a nil-map arm — emitting
// NULL (c0) where the empty map emits the empty-map header (a0) — would split the two
// spellings apart and fail here, and that is precisely the change that would force the
// normalisation back into BeginExtras. The concrete byte string is pinned next to the
// equality so that two silently empty encodings cannot pass for agreement.
func TestWireClientBeginExtras_NilExtrasNeedsNoNormalisation(t *testing.T) {
	// NOT t.Parallel(): the LIVE half below defers goleak.VerifyNone, and goleak's
	// default filters ignore a parent parked in testing.(*T).Run but not one parked in
	// testing.tRunner.func1 waiting for parallel subtests to signal
	// (go.uber.org/goleak@v1.3.0 options.go:174-175). A parallel test that defers
	// VerifyNone therefore reports its own parent as a leak, every run.
	defer goleak.VerifyNone(t)

	encode := func(extra map[string]packstream.Value) []byte {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		if err := proto.EncodeRequest(enc, &proto.Begin{Extra: extra}); err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		return buf.Bytes()
	}
	nilBytes := encode(nil)
	emptyBytes := encode(map[string]packstream.Value{})
	t.Logf("BEGIN encodes to %x with a nil extras map and to %x with an empty one", nilBytes, emptyBytes)
	if !bytes.Equal(nilBytes, emptyBytes) {
		t.Fatalf("BEGIN with a nil extras map encodes to %x but with an empty one to %x. Passing the caller's map "+
			"through unchanged is then wrong: a nil map no longer reaches the wire as the empty map every "+
			"pre-existing call site sent, so BeginExtras must normalise nil to an empty map again", nilBytes, emptyBytes)
	}
	// Struct header for one field, TagBegin 0x11 (bolt/proto/messages.go:26), empty-map
	// header 0xa0. Pinned so the equality above cannot be satisfied by two encodings that
	// both produced nothing.
	if want := []byte{0xb1, proto.TagBegin, 0xa0}; !bytes.Equal(nilBytes, want) {
		t.Fatalf("BEGIN with a nil extras map encodes to %x, want %x", nilBytes, want)
	}

	// The LIVE half: the argument above is made against the encoder alone, so drive all
	// three spellings over the genuine wire and require each to open exactly one WRITE
	// transaction in the server's registry.
	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()
	for _, tc := range []struct {
		name string
		open func(*WireClient) (any, error)
	}{
		{"Begin()", func(c *WireClient) (any, error) { return c.Begin() }},
		{"BeginExtras(nil)", func(c *WireClient) (any, error) { return c.BeginExtras(nil) }},
		{"BeginMode(\"\")", func(c *WireClient) (any, error) { return c.BeginMode("") }},
	} {
		c, err := srv.Dial()
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		resp, err := tc.open(c)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !isSuccess(resp) {
			t.Errorf("%s was refused: %#v", tc.name, resp)
		}
		txs := srv.Server().Transactions()
		if len(txs) != 1 || txs[0].Mode != beginModeWrite {
			t.Errorf("%s: registry reports %d transaction(s) %v, want exactly one in mode %q",
				tc.name, len(txs), txs, beginModeWrite)
		}
		if _, err := c.Rollback(); err != nil {
			t.Fatalf("%s: ROLLBACK: %v", tc.name, err)
		}
		_ = c.Close()
		// The next iteration asserts len == 1, so this one's transaction must be gone.
		deadline := time.Now().Add(beginReplyBound)
		for len(srv.Server().Transactions()) != 0 && time.Now().Before(deadline) {
			time.Sleep(beginPollInterval)
		}
	}
}

// TestBoltBeginExtras_RouteNamesTheSimListenerNotADefault is the LIVE control on the
// independent reference.
//
// The routing-table clause compares the advertised addresses against
// [SimServer.ListenerAddr]. That comparison is only meaningful if the address is
// SPECIFIC to this server: were RoutingTable hardcoding a production default, an
// advertised "localhost:7687" would have to be caught. So this asserts the reply names
// the SIM listener — an address no production default could be — and that it is not any
// of the defaults the bolt/server tests use.
func TestBoltBeginExtras_RouteNamesTheSimListenerNotADefault(t *testing.T) {
	defer goleak.VerifyNone(t)

	srv, err := NewSimServer(SimEngineForServer(), clock.Real())
	if err != nil {
		t.Fatalf("NewSimServer: %v", err)
	}
	defer func() { _ = srv.Close() }()

	addr := srv.ListenerAddr()
	if addr == "" {
		t.Fatal("SimServer.ListenerAddr is empty; every address clause would compare against \"\"")
	}
	for _, notThis := range []string{"localhost:7687", "testhost:7687", "127.0.0.1:7687"} {
		if addr == notThis {
			t.Fatalf("the sim listener reports %q, which is also a production/test default: a hardcoded routing "+
				"table would satisfy the address clause by coincidence", addr)
		}
	}

	c, err := srv.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	resp, err := c.Request(&proto.Route{})
	if err != nil {
		t.Fatalf("ROUTE: %v", err)
	}
	var obs BoltRouteObs
	obs.ListenerAddr = addr
	beginFillRouteTable(&obs, resp)
	if obs.RoleCount != len(beginExpectedRoles) {
		t.Fatalf("the routing table carried %d entries, want %d", obs.RoleCount, len(beginExpectedRoles))
	}
	for _, role := range beginExpectedRoles {
		if !slices.Equal(obs.AddressesByRole[role], []string{addr}) {
			t.Errorf("role %s advertises %v, want [%s]", role, obs.AddressesByRole[role], addr)
		}
	}
}

// TestBoltBeginExtras_LargerTxTimeoutStopsTheReap is the ALTERNATIVE-CONFIGURATION
// control for the timeout family: a real configuration change, not a doctored value.
//
// The in-scenario control removes the `tx_timeout` key. This one KEEPS it and changes
// only its value, so the pair isolates the extra's magnitude rather than its presence: an
// advance that reaps a transaction asking for a small bound must leave one asking for a
// large bound alive. Both arms run the identical script against identical servers.
func TestBoltBeginExtras_LargerTxTimeoutStopsTheReap(t *testing.T) {
	defer goleak.VerifyNone(t)

	small := beginTimeoutPlan{
		name: "small-bound", sendKey: true, timeoutMS: beginTxTimeoutSmall.Milliseconds(),
		idleBound: beginTxTimeoutClear, defTotal: beginTxTimeoutClear,
		advances: []time.Duration{beginTxTimeoutSmall},
	}
	large := small
	large.name = "large-bound"
	// The ONLY difference: a bound ten times the advance.
	large.timeoutMS = (10 * beginTxTimeoutSmall).Milliseconds()

	// Stored by POINTER: reaped() has a pointer receiver because a BoltTimeoutArm is well
	// over gocritic's hugeParam threshold.
	got := map[string]*BoltTimeoutArm{}
	for _, p := range []beginTimeoutPlan{small, large} {
		arm, err := driveBoltTimeoutArm(context.Background(), &p)
		if err != nil {
			t.Fatalf("driveBoltTimeoutArm(%s): %v", p.name, err)
		}
		got[p.name] = &arm
	}
	if !got["small-bound"].reaped() {
		t.Errorf("the arm asking for tx_timeout=%dms was NOT reaped by an advance of %s",
			small.timeoutMS, beginTxTimeoutSmall)
	}
	if got["large-bound"].reaped() {
		t.Errorf("the arm asking for tx_timeout=%dms WAS reaped by an advance of only %s: the client's bound is "+
			"not what the reaper is honouring, so the scenario's timeout family attributes its reap to the wrong "+
			"thing", large.timeoutMS, beginTxTimeoutSmall)
	}
	if !got["large-bound"].Committed {
		t.Errorf("the large-bound arm survived the advance but its COMMIT was answered %q / %q",
			got["large-bound"].GotCode, got["large-bound"].GotMessage)
	}
	// Both must have armed a timer, or "not reaped" would mean "no reaper".
	for name, arm := range got {
		if arm.TimersArmed < 1 {
			t.Errorf("arm %s armed %d timer(s) on the injected clock", name, arm.TimersArmed)
		}
	}
}
