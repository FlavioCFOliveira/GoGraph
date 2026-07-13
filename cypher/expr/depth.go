package expr

import "fmt"

// MaxValueDepth bounds how deeply a Cypher value (nested List/Map, or a
// property map reached through a Node/Relationship/Path) may nest before the
// evaluator rejects it. Cypher values are walked one Go stack frame per nesting
// level by several recursive helpers — result-size accounting
// (estimateValueSize), PackStream encoding (Encoder.WriteValue), and
// ListValue/MapValue String/Hash/Equal. An unbounded nesting depth therefore
// lets a short query build a value deep enough to overflow the goroutine stack,
// a fatal and unrecoverable crash (fatal error: stack overflow cannot be
// recovered), taking down every other connection on a shared server.
//
// The amplification vector is iterative construction: reduce() (and, to a
// lesser degree, list comprehensions) can add one nesting level per iteration
// while charging only one element against the element budget
// ([DefaultMaxListElements]), so ~5,000,000 depth fits inside the 10,000,000
// element ceiling. The parser already caps query-text nesting
// (maxNestingDepth = 256) and the PackStream decoder caps read depth
// (maxValueDepth = 128); MaxValueDepth is the symmetric bound on the
// construction/write side that those two guards leave open.
//
// 1000 is far above any legitimate need — the openCypher TCK and realistic
// application values nest only a handful of levels — while 1000 Go stack frames
// (~a few hundred KiB) is orders of magnitude below the goroutine stack limit,
// so a value at the cap still walks safely.
const MaxValueDepth = 1000

// maxDepthProbeVisits bounds the total number of value nodes [ExceedsValueDepth]
// will visit (and the transient work-stack it will hold) on a single call. Cypher
// values may be aliased directed acyclic graphs — reduce(acc=[0], … | [acc, acc])
// shares one accumulator under two slots, so the logical node count doubles each
// iteration while the distinct allocation count grows only linearly. A naive walk
// would then be exponential in time. Capping visits keeps the probe O(visits) and
// treats an over-large logical structure the same as an over-deep one: rejected.
// The cap is twice the element budget so any structure the element budget admits
// as a flat value is still walkable in full, while a doubling DAG trips it fast.
const maxDepthProbeVisits = 2 * DefaultMaxListElements

// ExceedsValueDepth reports whether v nests deeper than [MaxValueDepth] or is
// structurally too large to walk safely (more than [maxDepthProbeVisits] logical
// nodes, which an aliased DAG can reach with a linear allocation count). It is
// the guard the evaluator applies to values produced by the depth-amplifying
// constructs (reduce, list comprehension) so an over-deep or over-large value is
// never returned into the rest of the query, protecting every downstream
// recursive walker (result accounting, PackStream encoding, String/Hash/Equal).
//
// The walk is iterative (an explicit work stack, never the Go call stack) with
// early exit, so probing a pathological value cannot itself overflow the stack or
// run unbounded: an over-deep chain trips the depth cap after ~MaxValueDepth
// visits along one path, and an over-wide/aliased value trips the visit cap.
func ExceedsValueDepth(v Value) bool {
	type frame struct {
		v     Value
		depth int
	}
	stack := []frame{{v, 0}}
	visits := 0
	for len(stack) > 0 {
		n := len(stack) - 1
		f := stack[n]
		stack = stack[:n]

		if f.depth > MaxValueDepth {
			return true
		}
		visits++
		if visits > maxDepthProbeVisits || len(stack) > maxDepthProbeVisits {
			return true
		}

		switch x := f.v.(type) {
		case ListValue:
			for _, e := range x {
				stack = append(stack, frame{e, f.depth + 1})
			}
		case MapValue:
			for _, e := range x {
				stack = append(stack, frame{e, f.depth + 1})
			}
		case NodeValue:
			for _, e := range x.Properties {
				stack = append(stack, frame{e, f.depth + 1})
			}
		case RelationshipValue:
			for _, e := range x.Properties {
				stack = append(stack, frame{e, f.depth + 1})
			}
		case PathValue:
			for i := range x.Nodes {
				stack = append(stack, frame{x.Nodes[i], f.depth + 1})
			}
			for i := range x.Relationships {
				stack = append(stack, frame{x.Relationships[i], f.depth + 1})
			}
		}
		// Scalars (Integer/Float/String/Boolean/Null, temporal/point) are
		// leaves: nothing to push.
	}
	return false
}

// errValueTooDeep is the typed error returned when an expression would
// materialise a value nested deeper than [MaxValueDepth] (or too large to walk).
// Its message shape mirrors errListTooLarge/errStringTooLarge so callers map it
// to a query error, never a panic or an out-of-memory / stack-overflow crash.
func errValueTooDeep() error {
	return &EvalError{Msg: fmt.Sprintf(
		"ArgumentError: NumberOutOfRange: expression would materialise a value nested deeper than the maximum of %d levels",
		MaxValueDepth)}
}
