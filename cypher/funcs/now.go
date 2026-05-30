package funcs

import (
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Statement-frozen `now` time
// ─────────────────────────────────────────────────────────────────────────────
//
// openCypher specifies that all calls to the temporal "now" constructors —
// localtime(), time(), date(), localdatetime(), datetime() — within a
// single statement must observe the same instant. Without that guarantee
// Temporal10 [12] (`RETURN duration.inSeconds(localtime(), localtime())`)
// produces a non-zero duration because each call to time.Now() advances by
// at least one tick.
//
// The production engine (cypher.Engine) satisfies this requirement without
// touching statementNow: it wraps its FunctionRegistry in a per-query
// nowAwareRegistry (cypher/stmt_now_reg.go) that captures time.Now() once at
// the start of each Engine.Run / Engine.RunInTx call and overrides the
// zero-argument forms of the five temporal constructors to return values
// derived from that frozen instant. Concurrent queries therefore each observe
// their own independent timestamp with no shared mutable state.
//
// statementNow is retained exclusively for the TCK runner
// (cypher/tck/runner_test.go) and for standalone unit tests that call
// temporal functions directly outside an Engine: those callers use
// [SetStatementNow] to pin a deterministic instant before running the
// scenario and [ClearStatementNow] to restore the fall-through behaviour
// afterwards.
//
// # Concurrency
//
// statementNow is process-global. Callers outside the engine (e.g. the TCK
// runner) that use [SetStatementNow] must serialise their own calls if they
// run in parallel, as the last writer wins. The engine itself never calls
// SetStatementNow.

//nolint:gochecknoglobals // process-global statement-now; replaceable via Set/Clear
var statementNow atomic.Pointer[time.Time]

// SetStatementNow installs t as the statement-frozen "now" for every
// subsequent temporal "now" constructor in this process until
// [ClearStatementNow] is called.
func SetStatementNow(t time.Time) {
	statementNow.Store(&t)
}

// ClearStatementNow removes the frozen "now" so subsequent calls to
// [StatementNow] fall back to time.Now().
func ClearStatementNow() {
	statementNow.Store(nil)
}

// StatementNow returns the statement-frozen instant when one has been
// installed via [SetStatementNow], otherwise time.Now().UTC(). The
// temporal "now" constructors call this in place of time.Now() so
// repeated calls within the same statement observe the same instant.
func StatementNow() time.Time {
	if p := statementNow.Load(); p != nil {
		return p.UTC()
	}
	return time.Now().UTC()
}
