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
// The engine calls [SetStatementNow] at the start of every query and
// [ClearStatementNow] when the query result is closed. Within a query the
// temporal "now" constructors call [StatementNow], which returns the
// frozen time when set and time.Now().UTC() otherwise (the engine never
// installs a value when the registry is used standalone, e.g. in unit
// tests that call a function directly).
//
// # Concurrency
//
// statementNow is process-global. Two queries running concurrently in
// the same process will race; the last writer wins. This is acceptable
// for the TCK runner (one query at a time per worker) but a future
// improvement is to thread the timestamp through the per-query
// FunctionRegistry instead, so each query owns its own instant.

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
