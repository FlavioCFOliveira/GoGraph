package server

// plan_meta.go — Bolt `plan` / `profile` SUCCESS metadata (rmp #2721).
//
// GoGraph's plan surfaces were Go methods only, so a driver asking a GoGraph
// server for a plan got nothing: `ResultSummary.Plan()` and `Profile()` were
// always nil, and the driver-compat inventory recorded that as a known gap.
// Accepting EXPLAIN and PROFILE as statement prefixes (cypher/plan_prefix.go) is
// half the fix; publishing the captured plan in the terminal SUCCESS is the
// other half, because that SUCCESS is the message the driver turns into the
// ResultSummary.
//
// # The wire contract, read from the decoder
//
// The key names and value types below are transcribed from the driver that must
// read them (neo4j-go-driver v5.28.4, neo4j/internal/bolt/hydrator.go), not
// recalled:
//
//   - parsePlanOpIdArgsChildren reads `operatorType` as a STRING, `identifiers`
//     as a LIST of strings, `args` as a MAP, and `children` as a LIST of maps.
//     Each child is parsed by the same function, so the structure is uniform all
//     the way down.
//   - parseProfile additionally reads `dbHits` and `rows`, both asserted to
//     int64 with a comma-ok assertion, so an ABSENT key yields 0 rather than a
//     panic. The hydrator decodes every packed integer with unp.Int(), which
//     returns int64, so an int64 here arrives as an int64 there. `dbHits` is
//     omitted for an operator whose accesses nobody counted (rmp #2760), on the
//     same reasoning as the page-cache keys below.
//   - parseProfile reads `time` with an UNCHECKED type assertion to int64
//     (`childPlan.Time = planTime.(int64)`). It is emitted as an int64 for that
//     reason and for no other; a float would panic the driver.
//   - `pageCacheHits`, `pageCacheMisses` and `pageCacheHitRatio` are read the
//     same unchecked way. GoGraph measures none of them and therefore emits
//     none: a fabricated zero would read as a measurement.
//
// # Which key, and never both
//
// An EXPLAIN publishes `plan` and a PROFILE publishes `profile`, never both.
// That is what lets a driver tell the planner's ESTIMATES apart from
// measurements of a run that happened — and it is the same split
// [cypher.Result.Plan] / [cypher.Result.Profile] make on the Go side.

import (
	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// Bolt plan/profile key names, from neo4j/internal/bolt/hydrator.go.
const (
	planKeyOperatorType = "operatorType"
	planKeyArgs         = "args"
	planKeyChildren     = "children"
	planKeyDbHits       = "dbHits"
	planKeyRows         = "rows"
	planKeyTime         = "time"
	// planArgDetails carries the operator's own inline detail — the scanned
	// label, the expanded pattern — under the key Neo4j uses for the same thing.
	planArgDetails = "Details"
	// planArgRowsRemoved carries the candidate rows an operator read and then
	// discarded, under PostgreSQL's own name for the concept (rmp #2764).
	//
	// It goes in `args` and NOT at the top level, which is a wire-protocol fact
	// rather than a preference: the driver's parseProfile reads a FIXED set of
	// top-level keys — dbHits, rows, time, and the three pageCache ones — and
	// discards every other, so a new top-level key would reach no caller. `args`
	// is read wholesale into ProfiledPlan.Arguments (neo4j-go-driver v5.28.4,
	// neo4j/internal/bolt/hydrator.go, parsePlanOpIdArgsChildren), so a figure put
	// there is surfaced to the driver's user verbatim.
	//
	// The name is PostgreSQL's `Rows Removed by Filter` in the CamelCase spelling
	// the args map uses for every other key (Neo4j's own arguments are `DbHits`,
	// `EstimatedRows`, `Details`). Neo4j has no equivalent argument, so there is no
	// incumbent spelling to match and the recognisable name is the right one.
	planArgRowsRemoved = "RowsRemovedByFilter"
)

// resultPlanMetadata renders the plan a statement captured, as the pair of
// SUCCESS metadata fields the drivers read: (plan, profile). At most one is
// non-nil, and both are nil for an ordinary statement written with no prefix.
//
// r may be nil — the caller reads it from a cursor that a concurrent failure may
// already have dropped — in which case there is nothing to publish.
func resultPlanMetadata(r *cypher.Result) (plan, profile map[string]packstream.Value) {
	if r == nil {
		return nil, nil
	}
	if n := r.Plan(); n != nil {
		return planNodeMetadata(n, false), nil
	}
	if n := r.Profile(); n != nil {
		return nil, planNodeMetadata(n, true)
	}
	return nil, nil
}

// planNodeMetadata renders one captured plan node and its subtree.
//
// profiled selects whether the measured fields are written. They are omitted —
// not zeroed — for an EXPLAIN, because an EXPLAIN executed nothing and a `rows`
// of 0 on the wire would read as an operator that produced no rows rather than
// as one that never ran.
//
// The recursion is bounded by the depth of the plan, which is bounded in turn by
// the parser's nesting guard (cypher/parser/guard.go): a query deep enough to
// overflow this walk is rejected before it is ever planned.
func planNodeMetadata(n *exec.PlanNode, profiled bool) map[string]packstream.Value {
	m := map[string]packstream.Value{
		planKeyOperatorType: n.Name,
	}
	args := map[string]packstream.Value{}
	if n.Detail != "" {
		args[planArgDetails] = n.Detail
	}
	// The rejection figure is an ARG rather than a top-level key, and it is written
	// only when the operator reports one. An operator that removes no rows sends
	// nothing, on the same reasoning that omits dbHits below and the page-cache keys
	// above: an absent key is "no figure", a 0 would be a measurement claim
	// (rmp #2764). Unlike dbHits this is also written for an EXPLAIN-shaped node —
	// it cannot be, in practice, because an EXPLAIN never runs an operator and so no
	// operator ever claims the figure, but the guard is the flag rather than the
	// mode, so the two cannot disagree.
	if profiled && n.RowsRemovedByFilterKnown {
		args[planArgRowsRemoved] = n.RowsRemovedByFilter
	}
	if len(args) > 0 {
		m[planKeyArgs] = args
	}
	if profiled {
		// rows and time are emitted for every node, including one the
		// instrumentation did not reach: its counters are genuinely zero and the
		// honest report of that is a zero, not an absent key that a driver would
		// render as "no data".
		m[planKeyRows] = n.Rows
		m[planKeyTime] = n.Time.Nanoseconds()
		// dbHits is NOT, and that is the whole of rmp #2760 on the wire. An
		// operator whose storage accesses nobody counted has no figure to send, and
		// a 0 on this key is a measurement claim — the same reasoning that already
		// omits the page-cache keys above. The omission is safe for the driver this
		// file transcribes: parseProfile reads it as
		// `plan.DbHits, _ = profilex["dbHits"].(int64)` (neo4j-go-driver v5.28.4,
		// neo4j/internal/bolt/hydrator.go), a comma-ok assertion that yields 0 for
		// an absent key and cannot panic.
		//
		// A driver therefore still surfaces 0 for such an operator, because Bolt's
		// ProfiledPlan has no tri-state to carry. What the omission buys is that
		// GoGraph does not ASSERT the zero: the Go surfaces
		// ([cypher.Result.Profile], [cypher.Engine.Profile] and
		// [cypher.Engine.ProfileTable]) render "?" and remain the accurate report.
		if n.DbHitsKnown {
			m[planKeyDbHits] = n.DbHits
		}
	}
	if len(n.Children) > 0 {
		kids := make([]packstream.Value, len(n.Children))
		for i := range n.Children {
			kids[i] = planNodeMetadata(&n.Children[i], profiled)
		}
		m[planKeyChildren] = kids
	}
	return m
}
