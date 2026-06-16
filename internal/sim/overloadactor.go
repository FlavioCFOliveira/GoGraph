package sim

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
)

// OverloadFamily identifies one class of legitimately-heavy operation the
// [OverloadActor] issues. Each is well-formed Cypher that pushes a resource
// dimension (transaction size, list size, traversal breadth/depth, result-set
// size) toward or past the engine's declared bound.
type OverloadFamily int

// Overload families.
const (
	// OverloadHugeUnwind unwinds a very large literal range, producing many rows.
	OverloadHugeUnwind OverloadFamily = iota
	// OverloadLargeCreateTx creates many nodes in a single autocommit transaction,
	// pushing the per-transaction op count toward DefaultMaxTxnOps.
	OverloadLargeCreateTx
	// OverloadLargeResultSet matches a Cartesian product to materialise a large
	// result set, exercising the engine's MaxResultRows cap.
	OverloadLargeResultSet
	// OverloadDeepVLE runs a deep/wide variable-length expansion over the seeded
	// graph, exercising traversal bounds.
	OverloadDeepVLE
)

// overloadFamilyCount is the number of overload families; the unit test asserts
// every family is reachable.
const overloadFamilyCount = 4

// String renders an OverloadFamily for reports.
func (f OverloadFamily) String() string {
	switch f {
	case OverloadHugeUnwind:
		return "HugeUnwind"
	case OverloadLargeCreateTx:
		return "LargeCreateTx"
	case OverloadLargeResultSet:
		return "LargeResultSet"
	case OverloadDeepVLE:
		return "DeepVLE"
	default:
		return fmt.Sprintf("OverloadFamily(%d)", int(f))
	}
}

// OverloadOutcome records the result of one heavy operation. Exactly one of
// Succeeded or BoundedError is the acceptable result: the engine either served
// the work within its limits or refused it with a typed limit/bound error. A
// hang, panic, OOM, or dropped acknowledged write is a violation (a hang is
// caught by the caller's deadline; the others by goleak/no-panic and the
// durability re-read).
type OverloadOutcome struct {
	Family       OverloadFamily
	Succeeded    bool   // the op completed and drained cleanly
	BoundedError bool   // the engine refused it with a typed FAILURE (a declared bound)
	Rows         int    // rows actually streamed back (always bounded)
	FailureMsg   string // populated when BoundedError
}

// Acceptable reports whether the outcome honours the graceful-degradation
// contract: success within limits OR a typed bound error.
func (o OverloadOutcome) Acceptable() bool { return o.Succeeded || o.BoundedError }

// overloadUnwindSize is the literal range an [OverloadHugeUnwind] unwinds. It is
// large enough to exceed the SimServer engine's result-row cap
// ([defaultSimResultRowCap]) so the cap is exercised, yet bounded so the actor
// itself stays bounded.
const overloadUnwindSize = defaultSimResultRowCap + 10_000

// overloadCreateBatch is the number of nodes an [OverloadLargeCreateTx] creates
// in one statement (one autocommit transaction). It is large enough to be a
// genuinely heavy write but well within DefaultMaxTxnOps so it commits; the
// point is to prove a large legitimate transaction is served and durable, not to
// trip the op cap (which the malformed/abuse paths cover differently).
const overloadCreateBatch = 5_000

// overloadProductSide is the per-side size of the Cartesian product an
// [OverloadLargeResultSet] forms; the product (side²) exceeds the result-row cap.
const overloadProductSide = 500

// OverloadActor issues legitimately heavy work over the real Bolt wire and
// classifies the engine's response. It asserts the engine enforces its declared
// bounds (a typed FAILURE or a bounded, fully-streamed success) and degrades
// gracefully — never OOM, panic, deadlock, or drop an acknowledged write.
//
// # Concurrency contract
//
// OverloadActor is stateless; each [OverloadActor.Run] call drives one
// connection it owns. It is safe to call from many goroutines (the concurrent
// harness does), each with its own connection.
type OverloadActor struct{}

// Name returns the actor's identifier.
func (OverloadActor) Name() string { return "OverloadActor" }

// PickFamily chooses an overload family from the seed (one int draw).
func (OverloadActor) PickFamily(seed *Seed) OverloadFamily {
	return OverloadFamily(seed.IntN(overloadFamilyCount))
}

// Run executes one heavy operation of the given family over c and returns the
// classified outcome. The connection must already be Connected. It never returns
// an error for an expected engine bound (that is a BoundedError outcome); it
// returns an error only for a harness/transport failure.
func (a OverloadActor) Run(c *WireClient, family OverloadFamily) (OverloadOutcome, error) {
	out := OverloadOutcome{Family: family}
	query, params := a.statement(family)

	runResp, err := c.Run(query, params)
	if err != nil {
		return out, fmt.Errorf("sim: overload RUN(%s): %w", family, err)
	}
	if f, ok := runResp.(*proto.Failure); ok {
		out.BoundedError = true
		out.FailureMsg = f.Code + ": " + f.Message
		return out, nil
	}

	records, term, err := c.PullAll()
	if err != nil {
		return out, fmt.Errorf("sim: overload PULL(%s): %w", family, err)
	}
	out.Rows = len(records)
	switch m := term.(type) {
	case *proto.Failure:
		// The engine streamed up to its cap, then surfaced the bound through the
		// result error — a graceful, typed refusal of the over-large request.
		out.BoundedError = true
		out.FailureMsg = m.Code + ": " + m.Message
	case *proto.Success:
		out.Succeeded = true
	}
	return out, nil
}

// statement returns the Cypher and parameters for an overload family.
func (OverloadActor) statement(family OverloadFamily) (string, map[string]any) {
	switch family {
	case OverloadHugeUnwind:
		// UNWIND a large range and return each element: row count == range size,
		// which exceeds the result-row cap.
		return fmt.Sprintf("UNWIND range(1, %d) AS x RETURN x", overloadUnwindSize), nil
	case OverloadLargeCreateTx:
		// Create many nodes in one statement (one autocommit transaction).
		return fmt.Sprintf("UNWIND range(1, %d) AS i CREATE (:Bulk {i: i})", overloadCreateBatch), nil
	case OverloadLargeResultSet:
		// A self-Cartesian product over a bounded UNWIND: side² rows, exceeding
		// the result-row cap without touching the graph.
		return fmt.Sprintf(
			"UNWIND range(1, %d) AS a UNWIND range(1, %d) AS b RETURN a, b",
			overloadProductSide, overloadProductSide), nil
	case OverloadDeepVLE:
		// A deep variable-length expansion. Bounded with LIMIT so the actor stays
		// bounded while still exercising the VLE plan over whatever graph exists.
		return "MATCH p=(a)-[*1..6]->(b) RETURN length(p) LIMIT 1000", nil
	default:
		return "RETURN 1", nil
	}
}
