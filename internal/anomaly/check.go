package anomaly

import (
	"fmt"
	"sort"
	"strings"
)

// Phenomenon is a named isolation anomaly.
//
// The names are Adya's (ICDE 2000) except where Elle's are now the common
// currency; both are given where they differ.
type Phenomenon int

const (
	// G1a is an ABORTED READ: a transaction read a version written by a
	// transaction that aborted.
	G1a Phenomenon = iota
	// G1b is an INTERMEDIATE READ: a transaction read a version of a key that
	// its writer superseded before committing, so no committed state ever held
	// it.
	G1b
	// G0 is a WRITE CYCLE: a cycle in the DSG made entirely of write-
	// dependencies. It means two transactions' writes to different keys were
	// applied in opposite orders.
	G0
	// G1c is CIRCULAR INFORMATION FLOW: a cycle made of write- and read-
	// dependencies. Each transaction in the cycle depends on information the
	// next one produced.
	G1c
	// GSingle (Elle's G-single; Adya's G-SIb, "missed effects") is a cycle with
	// EXACTLY ONE anti-dependency edge. Lost update has this shape.
	GSingle
	// GNonadjacent is a cycle whose anti-dependency edges are never adjacent to
	// one another. It generalises GSingle and is the boundary snapshot
	// isolation actually draws; see [SnapshotIsolation].
	GNonadjacent
	// G2Item is an ANTI-DEPENDENCY CYCLE over individual items: any cycle
	// carrying at least one anti-dependency edge. Write skew has this shape, and
	// snapshot isolation PERMITS it.
	G2Item
	// Unwritten is not one of Adya's phenomena. It reports a read of a version
	// no transaction in the history wrote, which means the history is
	// INCOMPLETE. It is surfaced rather than skipped because an incomplete
	// history silently produces a clean verdict, and a clean verdict from
	// missing data is the worst output this package could give.
	Unwritten
)

var phenomenonName = map[Phenomenon]string{
	G1a:          "G1a (aborted read)",
	G1b:          "G1b (intermediate read)",
	G0:           "G0 (write cycle)",
	G1c:          "G1c (circular information flow)",
	GSingle:      "G-single (cycle with exactly one anti-dependency)",
	GNonadjacent: "G-nonadjacent (cycle with non-adjacent anti-dependencies)",
	G2Item:       "G2-item (anti-dependency cycle)",
	Unwritten:    "unwritten-version-read (INCOMPLETE HISTORY, not an isolation phenomenon)",
}

func (p Phenomenon) String() string {
	if n, ok := phenomenonName[p]; ok {
		return n
	}
	return fmt.Sprintf("Phenomenon(%d)", int(p))
}

// Anomaly is one classified finding.
type Anomaly struct {
	// Detail explains the finding in the terms of the history: which
	// transactions, which keys, which versions.
	Detail string
	// Txns are the transactions involved, in cycle order where the anomaly is a
	// cycle.
	Txns []TxID
	// Cycle is the edge sequence, empty for the non-cycle phenomena.
	Cycle []Edge
	// Type names the phenomenon.
	Type Phenomenon
}

func (a Anomaly) String() string {
	var b strings.Builder
	b.WriteString(a.Type.String())
	if len(a.Cycle) > 0 {
		b.WriteString(": ")
		for i, e := range a.Cycle {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(e.String())
		}
	}
	if a.Detail != "" {
		b.WriteString("\n  ")
		b.WriteString(a.Detail)
	}
	return b.String()
}

// Level is an isolation level, named by what it FORBIDS.
type Level int

const (
	// ReadUncommitted is Adya's PL-1: forbids G0 only.
	ReadUncommitted Level = iota
	// ReadCommitted is Adya's PL-2: forbids G0 and G1 (= G1a ∪ G1b ∪ G1c).
	ReadCommitted
	// SnapshotIsolation is Adya's PL-SI, and it is the level GoGraph targets.
	//
	// It forbids G0, G1 and G-nonadjacent — and PERMITS G2-item cycles whose
	// anti-dependency edges are adjacent, which is exactly write skew.
	//
	// THE BOUNDARY IS G-NONADJACENT, NOT G-SINGLE, and getting that right is the
	// substance of this checker. Adya's PL-SI forbids G-SIb, "a cycle with
	// exactly one anti-dependency edge" — Elle's G-single. Cerone, Bernardi &
	// Gotsman's characterisation of GENERALIZED snapshot isolation is stronger:
	// it forbids any cycle whose anti-dependency edges are pairwise
	// non-adjacent, of which a single anti-dependency is the degenerate case.
	// Elle adopts the stronger form and says why, in
	// src/elle/consistency_model.clj (read 2026-08-08): "Chatting with Alexey
	// Gotsman about this confirms my suspicion: generalized SI forbids *any*
	// history where all rw edges are nonadjacent, not just G-single."
	//
	// The two classic anomalies fall on opposite sides of it, and that is the
	// check that matters:
	//
	//   - LOST UPDATE (P4): T1 -rw-> T2 -ww-> T1. One anti-dependency, so the
	//     cycle is G-single ⊂ G-nonadjacent ⇒ FORBIDDEN. Snapshot isolation
	//     prevents it by first-committer-wins on the shared key.
	//
	//   - WRITE SKEW (A5B): T1 -rw-> T2 -rw-> T1. Two anti-dependencies, and
	//     they are ADJACENT — each transaction is both entered and left by one —
	//     so the cycle is G2-item but NOT G-nonadjacent ⇒ PERMITTED. The two
	//     transactions write different keys, so nothing conflicts.
	//
	// A checker that flagged legal write skew would be worse than none, because
	// every clean run would carry a false violation and the real ones would stop
	// being read.
	SnapshotIsolation
	// Serializable is Adya's PL-3: forbids G0, G1 and G2 — any cycle at all.
	Serializable
)

func (l Level) String() string {
	switch l {
	case ReadUncommitted:
		return "read uncommitted (PL-1)"
	case ReadCommitted:
		return "read committed (PL-2)"
	case SnapshotIsolation:
		return "snapshot isolation (PL-SI)"
	default:
		return "serializable (PL-3)"
	}
}

// Report is the outcome of checking a history.
type Report struct {
	// Level is the level the history was judged against.
	Level Level
	// Violations are the anomalies that level forbids.
	Violations []Anomaly
	// Permitted are anomalies that were present but are LEGAL at this level —
	// write skew under snapshot isolation, above all. They are reported, not
	// hidden, because "your engine exhibits write skew and that is allowed here"
	// is information, and because silently discarding them would make it
	// impossible to tell a checker that found nothing from one that found
	// something and swallowed it.
	Permitted []Anomaly
	// Truncated records that the cycle search hit its bound.
	//
	// IT INVALIDATES A CLEAN VERDICT, NOT A VIOLATION. "No violation found"
	// under truncation means "none found within the bound" and is worth nothing;
	// the violations that WERE found are real, because each one is a cycle that
	// exists in the graph. A caller expecting a violation may therefore trust
	// what is reported and ignore this flag; a caller concluding cleanliness
	// must not. [Report.Clean] encodes that asymmetry.
	//
	// Never silently true: the rendered report leads with INCONCLUSIVE.
	Truncated bool
	// Txns is how many transactions were examined.
	Txns int
	// Edges is how many dependency edges the DSG carried.
	Edges int
}

// Clean reports whether the history is known to satisfy the level.
//
// A truncated search is never clean, however few violations it found: absence of
// evidence under a bound is not evidence of absence. See [Report.Truncated] for
// why the converse does not hold.
func (r *Report) Clean() bool { return len(r.Violations) == 0 && !r.Truncated }

// String renders the report for a test failure or a log line.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "history: %d transactions, %d dependency edges, checked against %s\n",
		r.Txns, r.Edges, r.Level)
	switch {
	case r.Truncated:
		b.WriteString("VERDICT: INCONCLUSIVE — the cycle search hit its bound; " +
			"absence of a violation below is not evidence of absence\n")
	case len(r.Violations) == 0:
		b.WriteString("VERDICT: CLEAN\n")
	default:
		fmt.Fprintf(&b, "VERDICT: %d VIOLATION(S)\n", len(r.Violations))
	}
	for _, a := range r.Violations {
		fmt.Fprintf(&b, "  VIOLATION %s\n", a.String())
	}
	for _, a := range r.Permitted {
		fmt.Fprintf(&b, "  permitted at this level: %s\n", a.String())
	}
	return b.String()
}

// maxCyclesExamined bounds the cycle enumeration. A history whose dependency
// graph is one large strongly connected component has combinatorially many
// simple cycles, and enumerating them all would turn a checker into a hang.
// Hitting the bound sets [Report.Truncated] — the bound is never silent.
const maxCyclesExamined = 20000

// maxCycleLength bounds how long a reported cycle may be. A ten-transaction
// cycle is not a diagnosis anyone can act on; the short ones are.
const maxCycleLength = 8

// Check classifies a history against an isolation level.
//
// The classification is DETERMINISTIC: the graph's nodes and each node's edges
// are sorted at construction, and the search visits them in that order, so the
// same history always yields the same report. That matters because the report is
// the evidence a future sighting is compared against.
func Check(h *History, level Level) (*Report, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	g, direct := BuildDSG(h)
	rep := &Report{Level: level, Txns: len(h.Txns)}
	for _, es := range g.Out {
		rep.Edges += len(es)
	}

	cycles, truncated := findCycles(g)
	rep.Truncated = truncated

	all := make([]Anomaly, 0, len(direct)+len(cycles))
	all = append(all, direct...)
	for _, c := range cycles {
		all = append(all, classifyCycle(c))
	}
	for _, a := range all {
		if forbids(level, a.Type) {
			rep.Violations = append(rep.Violations, a)
		} else {
			rep.Permitted = append(rep.Permitted, a)
		}
	}
	sortAnomalies(rep.Violations)
	sortAnomalies(rep.Permitted)
	return rep, nil
}

// forbids answers whether a level forbids a phenomenon. This function IS the
// level boundary; see [SnapshotIsolation] for the sources it was verified
// against.
func forbids(l Level, p Phenomenon) bool {
	switch p {
	case Unwritten:
		// Not a phenomenon: an incomplete history is a defect in the
		// OBSERVATION, and it invalidates the verdict at every level.
		return true
	case G0:
		return true // forbidden from PL-1 up
	case G1a, G1b, G1c:
		return l >= ReadCommitted
	case GSingle, GNonadjacent:
		return l >= SnapshotIsolation
	case G2Item:
		// The one that must NOT fire under snapshot isolation: a G2-item cycle
		// whose anti-dependencies are adjacent is write skew, which PL-SI
		// permits. Only serializability forbids it.
		return l >= Serializable
	default:
		return false
	}
}

// classifyCycle names a cycle by the most specific phenomenon it satisfies.
//
// Most specific first, because the lattice is nested — every G0 cycle is also
// G1c, every G-single is also G-nonadjacent and G2-item — and reporting the
// weakest name would tell a reader "there is a cycle somewhere" when the checker
// knows precisely which one.
func classifyCycle(c []Edge) Anomaly {
	var ww, wr, rw int
	for _, e := range c {
		switch e.Dep {
		case WW:
			ww++
		case WR:
			wr++
		default:
			rw++
		}
	}
	txns := make([]TxID, len(c))
	for i, e := range c {
		txns[i] = e.From
	}
	a := Anomaly{Cycle: c, Txns: txns}
	switch {
	case rw == 0 && wr == 0:
		a.Type = G0
		a.Detail = fmt.Sprintf("%d transactions wrote overlapping keys in mutually inconsistent orders", len(c))
	case rw == 0:
		a.Type = G1c
		a.Detail = fmt.Sprintf("%d transactions each read information the next one produced", len(c))
	case rw == 1:
		a.Type = GSingle
		a.Detail = fmt.Sprintf("one anti-dependency closes a cycle of %d transactions; "+
			"this is the shape of a LOST UPDATE and snapshot isolation must prevent it", len(c))
	case nonAdjacentRW(c):
		a.Type = GNonadjacent
		a.Detail = fmt.Sprintf("%d anti-dependencies in a cycle of %d, none adjacent to another; "+
			"generalized snapshot isolation forbids this", rw, len(c))
	default:
		a.Type = G2Item
		a.Detail = fmt.Sprintf("%d anti-dependencies in a cycle of %d, at least two adjacent; "+
			"this is the shape of WRITE SKEW, which snapshot isolation permits", rw, len(c))
	}
	return a
}

// nonAdjacentRW reports whether every anti-dependency edge in the cycle is
// followed by a non-anti-dependency edge.
//
// The cycle is circular, so the successor of the last edge is the first. Two
// adjacent rw edges mean one transaction is both entered and left by an
// anti-dependency — the write-skew shape, where the two transactions read an
// overlapping set and wrote disjoint parts of it. Anything else is a cycle
// generalized snapshot isolation forbids.
func nonAdjacentRW(c []Edge) bool {
	for i, e := range c {
		if e.Dep != RW {
			continue
		}
		if c[(i+1)%len(c)].Dep == RW {
			return false
		}
	}
	return true
}

func sortAnomalies(as []Anomaly) {
	sort.SliceStable(as, func(i, j int) bool {
		if as[i].Type != as[j].Type {
			return as[i].Type < as[j].Type
		}
		if len(as[i].Cycle) != len(as[j].Cycle) {
			return len(as[i].Cycle) < len(as[j].Cycle)
		}
		return as[i].Detail < as[j].Detail
	})
}
