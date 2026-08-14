package sim

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// mvcc_isolation.go — the MVCC isolation checkers of the deterministic
// multi-session mode (rmp #2436). Three invariants, each adjudicated by the
// per-transaction oracle workspaces and probed through the SAME transaction
// handles the workload drives, so a probe observes exactly what a client
// would:
//
//   - SNAPSHOT STABILITY — every count read inside one explicit read-only
//     transaction must equal the committed counts captured at its BEGIN, even
//     when other sessions commit in between (the engine's whole-transaction
//     snapshot, rmp #2307). The StabilityInterleaved counter proves the
//     "commits landed in between" condition was actually exercised.
//   - READ-YOUR-OWN-WRITES — after every write statement of a write
//     transaction, the written object is probed through the same handle: a
//     created/merged node must be visible, a deleted node invisible, a SET
//     property readable at its new value; and at the session's next BEGIN a
//     name the session committed earlier must be visible to the new
//     transaction (the cypher.Session frontier contract).
//   - ATOMIC VISIBILITY — write transactions sometimes create an
//     invariant-bearing PAIR of nodes across two statements of one
//     transaction; pair members are never deleted, so every reader must
//     observe both members or neither, adjudicated exactly by whether the
//     reader's snapshot predates the pair's fold. Observing exactly ONE
//     member is a strict subset of a committed multi-object transaction — an
//     atomicity/isolation breach.
//
// All probes are pure observations: they draw no randomness and mutate
// nothing, so the run stays a pure function of the seed with or without a
// violation.

// maxPairProbes bounds how many of the most recent committed pairs each pair
// probe examines, keeping the per-statement probe cost O(1) as pairs
// accumulate over a run.
const maxPairProbes = 8

// Probe query templates (parameterised, like the workload's own).
const (
	tmplCountByName  = "MATCH (n:Person {name:$name}) RETURN count(n)"
	tmplAgeByName    = "MATCH (n:Person {name:$name}) RETURN n.age"
	tmplCountPair    = "MATCH (n:Person) WHERE n.name = $a OR n.name = $b RETURN count(n)"
	tmplCountKnowsAB = "MATCH (a:Person {name:$a})-[r:KNOWS]->(b:Person {name:$b}) RETURN count(r)"
)

// mvccPair is one committed invariant-bearing node pair, created by two
// statements of a single transaction. foldSeq is the harness fold counter at
// the instant the pair's transaction folded, so a reader's expectation is
// exact: a transaction whose beginFoldSeq >= foldSeq began after the pair
// committed and must see BOTH members; one whose beginFoldSeq < foldSeq must
// see NEITHER.
type mvccPair struct {
	a, b    string
	foldSeq int
}

// isPairName reports whether the name belongs to an atomic-visibility pair
// member. Pair names are minted as mv-s<id>-pa<k> / mv-s<id>-pb<k>, so the
// marker substring cannot collide with the workload's other names
// (mv-s<id>-n<k>, mv-s<id>-m<k>).
func isPairName(name string) bool {
	return strings.Contains(name, "-pa") || strings.Contains(name, "-pb")
}

// clearPairState drops the session's in-flight pair bookkeeping. Called on
// every path that discards the open transaction's pending writes (conflict
// concession, rollback, drain) and after a successful fold has registered the
// completed pairs.
func (s *mvccSessionState) clearPairState() {
	s.pairEmit, s.pairFirst, s.pairSecond = "", "", ""
	s.pairsDone = nil
}

// txCount runs a single-scalar count/value query inside the session's open
// transaction and returns the integer of the first row's first column. A
// typed serialization conflict on the probe finishes the transaction exactly
// as the workload's own statements do (concede: rollback + workspace abort);
// the second return reports that concession so the caller stops probing the
// now-finished transaction. Any other error is a hard fault.
func (h *mvccHarness) txCount(s *mvccSessionState, query string, params map[string]any) (int64, bool, error) {
	res, err := s.tx.ExecAny(query, params)
	var n int64
	if err == nil {
		if res.Next() {
			if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
				n = int64(iv)
			}
		}
		for res.Next() {
		}
		drainErr := res.Err()
		_ = res.Close()
		err = drainErr
	}
	if err != nil {
		if errors.Is(err, mvcc.ErrSerializationConflict) {
			return 0, true, h.concede(s)
		}
		return 0, false, fmt.Errorf("sim: session %d isolation probe %q: %w", s.id, query, err)
	}
	return n, false, nil
}

// violate appends one isolation violation at the current tick.
func (h *mvccHarness) violate(op, format string, args ...any) {
	h.res.Violations = append(h.res.Violations, Violation{
		Kind:    ViolationACIDIsolation,
		Tick:    h.tick,
		Op:      op,
		Message: fmt.Sprintf(format, args...),
	})
}

// adjudicateCountRead adjudicates a count read the workload itself just ran
// inside the open transaction (no re-execution). Inside a READ-ONLY
// transaction both templates must return the counts captured at BEGIN —
// snapshot stability. Inside a WRITE transaction the Person count must equal
// the workspace's visible count (begin-snapshot plus own pending writes):
// a shortfall means an own write is invisible (a read-your-own-writes
// breach), an excess means a foreign commit leaked into the snapshot.
func (h *mvccHarness) adjudicateCountRead(s *mvccSessionState, query string, got int64, have bool) {
	if query != mvccReadTemplates[0] && query != mvccReadTemplates[1] {
		return
	}
	if !have {
		h.violate("count read", "session %d: count query %q returned no integer row", s.id, query)
		return
	}
	if s.readOnly {
		want, what := s.expectNodes, "Person count"
		if query == mvccReadTemplates[1] {
			want, what = s.expectEdges, "KNOWS count"
		}
		h.res.StabilityProbes++
		if h.foldSeq > s.beginFoldSeq {
			h.res.StabilityInterleaved++
		}
		if got != want {
			h.violate("snapshot stability",
				"session %d read-only tx: %s read %d, want %d (captured at BEGIN; %d commit(s) folded since) — the pinned snapshot moved; nodes:%s edges:%s",
				s.id, what, got, want, h.foldSeq-s.beginFoldSeq,
				h.stabilityNameDiff(s), h.stabilityEdgeDiff(s))
		}
		return
	}
	if query == mvccReadTemplates[0] && s.doomSuspect == "" {
		want := int64(s.otx.NodeCount())
		h.res.RYOWProbes++
		if got != want {
			// Not a violation yet: a refused-void write (doomed-tx contract,
			// rmp #2354) makes the workspace and the engine's view diverge
			// legitimately. The transaction must now end in a conflict; a
			// clean COMMIT converts this suspicion into a violation (finish).
			s.doomSuspect = fmt.Sprintf(
				"Person count read %d, want %d (begin-snapshot + own writes)", got, want)
		}
	}
}

// oracleEdgeNames renders the committed KNOWS edge set as sorted "a->b"
// strings, the form the stability checker captures at a read transaction's
// BEGIN and diffs on a violation.
func (h *mvccHarness) oracleEdgeNames() []string {
	states := h.oracle.edgeStates()
	out := make([]string, 0, len(states))
	for _, e := range states {
		out = append(out, h.oracle.nameOf(e.SrcID)+"->"+h.oracle.nameOf(e.DstID))
	}
	slices.Sort(out)
	return out
}

// stabilityEdgeDiff enumerates the KNOWS edges visible to the read-only
// transaction and renders the symmetric difference against the committed edge
// set captured at its BEGIN. Best-effort, like [mvccHarness.stabilityNameDiff].
func (h *mvccHarness) stabilityEdgeDiff(s *mvccSessionState) string {
	res, err := s.tx.ExecAny("MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a.name, b.name", nil)
	if err != nil {
		return fmt.Sprintf(" [edge diff unavailable: %v]", err)
	}
	seen := make(map[string]bool, len(s.expectEdgeNames))
	for res.Next() {
		a, okA := res.ValueAt(0).(expr.StringValue)
		b, okB := res.ValueAt(1).(expr.StringValue)
		if okA && okB {
			seen[string(a)+"->"+string(b)] = true
		}
	}
	_ = res.Close()
	var missing, extra []string
	expect := make(map[string]bool, len(s.expectEdgeNames))
	for _, e := range s.expectEdgeNames {
		expect[e] = true
		if !seen[e] {
			missing = append(missing, e)
		}
	}
	for e := range seen {
		if !expect[e] {
			extra = append(extra, e)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return fmt.Sprintf(" [vanished=%v appeared=%v]", missing, extra)
}

// stabilityNameDiff enumerates the Person names visible to the read-only
// transaction and renders the symmetric difference against the committed name
// set captured at its BEGIN, so a stability violation names the exact node
// that appeared or vanished. Best-effort: an enumeration failure only loses
// detail on a violation the caller is already raising.
func (h *mvccHarness) stabilityNameDiff(s *mvccSessionState) string {
	res, err := s.tx.ExecAny("MATCH (n:Person) RETURN n.name", nil)
	if err != nil {
		return fmt.Sprintf(" [name diff unavailable: %v]", err)
	}
	seen := make(map[string]bool, len(s.expectNames))
	for res.Next() {
		if sv, ok := res.ValueAt(0).(expr.StringValue); ok {
			seen[string(sv)] = true
		}
	}
	_ = res.Close()
	var missing, extra []string
	expect := make(map[string]bool, len(s.expectNames))
	for _, n := range s.expectNames {
		expect[n] = true
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	for n := range seen {
		if !expect[n] {
			extra = append(extra, n)
		}
	}
	// Sorted so the message — which lands in the determinism-compared result —
	// is a pure function of the state, not of map iteration order.
	slices.Sort(missing)
	slices.Sort(extra)
	return fmt.Sprintf(" [vanished=%v appeared=%v]", missing, extra)
}

// probeRYOW probes, through the transaction's own handle, the object the
// write statement that just succeeded targeted: read-your-own-writes at
// statement granularity. The expectation comes from the oracle workspace,
// which already mirrored the statement. Returns a hard fault only; a typed
// conflict on the probe concedes the transaction (see txCount).
func (h *mvccHarness) probeRYOW(s *mvccSessionState, kind OpKind, cypherText string, params map[string]any) error {
	if s.otx == nil || !s.open() || s.doomSuspect != "" {
		return nil
	}
	// suspect records a divergence between an own write and its read-back.
	// Under the doomed-tx contract (rmp #2354) a conflict hit by a void
	// primitive refuses the write WITHOUT failing the statement, so the
	// divergence is legitimate exactly when the transaction goes on to fail:
	// finish() converts a suspect that COMMITs cleanly into the violation.
	suspect := func(format string, args ...any) {
		s.doomSuspect = fmt.Sprintf(format, args...)
	}
	if cypherText == tmplCreateKnows {
		a, _ := params["a"].(string)
		b, _ := params["b"].(string)
		want := int64(0)
		if s.otx.PendingKnows(a, b) {
			want = 1
		}
		got, conceded, err := h.txCount(s, tmplCountKnowsAB, map[string]any{"a": a, "b": b})
		if err != nil || conceded {
			return err
		}
		h.res.RYOWProbes++
		if got != want {
			suspect("own KNOWS edge (%q)->(%q) reads count %d inside its own tx, want %d", a, b, got, want)
		}
		return nil
	}
	name, ok := params["name"].(string)
	if !ok {
		return nil
	}
	want := int64(0)
	if s.otx.HasPerson(name) {
		want = 1
	}
	got, conceded, err := h.txCount(s, tmplCountByName, map[string]any{"name": name})
	if err != nil || conceded {
		return err
	}
	h.res.RYOWProbes++
	if got != want {
		suspect("after own %v of %q the tx reads count %d, want %d", kind, name, got, want)
		return nil
	}
	if kind != OpUpdate || want == 0 {
		return nil
	}
	// A SET the transaction itself issued must read back at its new value.
	wantAge, visible := s.otx.AgeOf(name)
	wantInt, isInt := wantAge.(int64)
	if !visible || !isInt {
		return nil
	}
	gotAge, conceded, err := h.txCount(s, tmplAgeByName, map[string]any{"name": name})
	if err != nil || conceded {
		return err
	}
	h.res.RYOWProbes++
	if gotAge != wantInt {
		suspect("own SET n.age=%d on %q reads back %d inside the same tx", wantInt, name, gotAge)
	}
	return nil
}

// probeCrossTxRYOW probes, at the BEGIN of a session's new transaction, a
// name the SAME session committed in an earlier transaction: the
// cypher.Session frontier contract promises the session's next transaction
// begins at-or-after its own commit, so the name must be visible — unless a
// later committed transaction (any session's) deleted it, which the committed
// oracle adjudicates exactly.
func (h *mvccHarness) probeCrossTxRYOW(s *mvccSessionState) error {
	if s.lastCommitted == "" || !h.oracle.HasPersonName(s.lastCommitted) {
		return nil
	}
	got, conceded, err := h.txCount(s, tmplCountByName, map[string]any{"name": s.lastCommitted})
	if err != nil || conceded {
		return err
	}
	h.res.RYOWCrossTx++
	if got != 1 {
		h.violate("cross-tx read-your-own-writes",
			"session %d: name %q committed by this session's earlier tx (still committed per the oracle) is invisible to its next tx (count=%d)",
			s.id, s.lastCommitted, got)
	}
	return nil
}

// probePairs probes the most recent committed pairs through the open
// transaction's handle: a pair folded before the transaction began must show
// BOTH members, one folded after must show NEITHER. A count of exactly 1 is a
// strict subset of a committed multi-object transaction — the atomic
// visibility breach this checker exists to catch.
func (h *mvccHarness) probePairs(s *mvccSessionState) error {
	start := len(h.pairs) - maxPairProbes
	if start < 0 {
		start = 0
	}
	for _, p := range h.pairs[start:] {
		if !s.open() {
			return nil // a prior probe conceded the transaction
		}
		want := int64(0)
		if p.foldSeq <= s.beginFoldSeq {
			want = 2
		}
		got, conceded, err := h.txCount(s, tmplCountPair, map[string]any{"a": p.a, "b": p.b})
		if err != nil || conceded {
			return err
		}
		h.res.PairProbes++
		if got != want {
			detail := "the pair is not atomically visible"
			if got == 1 {
				detail = "STRICT SUBSET of a committed multi-object transaction observed"
			}
			h.violate("atomic visibility",
				"session %d: pair (%q,%q) folded at seq %d, tx began at seq %d: reads %d members, want %d — %s",
				s.id, p.a, p.b, p.foldSeq, s.beginFoldSeq, got, want, detail)
		}
	}
	return nil
}

// checkCommittedPairs sweeps the most recent committed pairs through the
// engine's PRESENT state (autocommit read, like the periodic parity check).
// Pair members are never deleted — the workload excludes them from DETACH
// DELETE — so the present state must always hold exactly both members of
// every committed pair; a count of 1 is a torn pair.
func (h *mvccHarness) checkCommittedPairs(tick int64) []Violation {
	var out []Violation
	start := len(h.pairs) - maxPairProbes
	if start < 0 {
		start = 0
	}
	for _, p := range h.pairs[start:] {
		got, err := h.checker.countQuery(h.adapter, tmplCountPair, map[string]any{"a": p.a, "b": p.b})
		if err != nil {
			out = append(out, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: "atomic visibility",
				Message: fmt.Sprintf("pair sweep (%q,%q) probe failed: %v", p.a, p.b, err)})
			continue
		}
		if got != 2 {
			detail := "committed pair not fully present"
			if got == 1 {
				detail = "STRICT SUBSET of a committed multi-object transaction visible in the present state"
			}
			out = append(out, Violation{Kind: ViolationACIDIsolation, Tick: tick, Op: "atomic visibility",
				Message: fmt.Sprintf("committed pair (%q,%q) counts %d members in the present state, want 2 — %s",
					p.a, p.b, got, detail)})
		}
	}
	return out
}
