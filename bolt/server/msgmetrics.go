package server

// msgmetrics.go — the per-message latency histograms (rmp #2715).
//
// # What this closes
//
// CLAUDE.md's observability mandate requires a latency histogram on every
// public blocking API. bolt/server serves the module's entire network surface
// and published none: the only per-message emissions on the Bolt path were
// bolt.pool.decoder.get / .put, which are pool bookkeeping and say nothing
// about how long a RUN, a PULL, a BEGIN or a COMMIT took. A Bolt latency
// regression in a deployed GoGraph was invisible to the module's own
// observability; the p50/p99 quoted for the Bolt surface came from the
// EXAMPLE's instrumentation (examples/23_bolt_server), which no operator runs.
//
// # Why the message type is in the NAME and not in a label
//
// [github.com/FlavioCFOliveira/GoGraph/internal/metrics.Backend] takes a name
// and a value; it has no label dimension. docs/metrics.md settles what a
// dimensioned series looks like in this module: the dimension goes in the name
// as "<base>.<dimension>.<value>", drawn from a closed set and built once at
// init, exactly as graph/lpg/mvcc_metricnames.go builds
// "lpg.mvcc.conflicts.store.<store>". This file follows that rule rather than
// inventing a second one — the base is the module-wide
// "<package-path>.<ExportedSymbol>" latency name, bolt.server.HandleMessage,
// the dimension is "message", and the value is one of the thirteen labels in
// [msgKindLabel].
//
// # Why the names are a table and not a concatenation
//
// The same reason graph/lpg/mvcc_metricnames.go is a table: a concatenation at
// the call site allocates a string on EVERY message. There is no justification
// available for allocating a constant, and this call site runs once per inbound
// Bolt message. The array is package-level, filled once at init, and never
// mutated, so an emission indexes it and allocates nothing.
//
// # Cardinality is bounded by construction
//
// [msgKindOf] is a type switch over the twelve client→server message types
// bolt/proto defines, plus msgOther. It cannot mint a name: the label never
// comes from the wire, only from which Go type the decoder produced. An unknown
// struct tag never reaches here at all — proto.DecodeRequest rejects it with an
// error (bolt/proto/messages.go, the default arm of its tag switch) and the
// serve loop answers Neo.ClientError.Request.Invalid without dispatching. So
// msgOther is reachable only by an in-process caller handing the exported
// [Session.HandleMessage] something that is not a bolt/proto request, and even
// that lands in ONE bucket, the way mvcc.StoreOther does: "not a store; the
// bucket that keeps the cardinality bounded without losing the count".

import "github.com/FlavioCFOliveira/GoGraph/bolt/proto"

// msgKind indexes the closed set of Bolt request message types for metric
// purposes. It exists only to select a pre-built series name; it is not a
// protocol concept and nothing outside this file depends on its values.
type msgKind uint8

const (
	msgHello msgKind = iota
	msgLogon
	msgLogoff
	msgGoodbye
	msgReset
	msgRun
	msgPull
	msgDiscard
	msgBegin
	msgCommit
	msgRollback
	msgRoute
	// msgOther is where anything that is not one of the twelve above is
	// counted. It is not a message type; it is the bucket that keeps the
	// cardinality bounded without losing the observation.
	msgOther
	// msgKindCount is the size of the closed set, and the length of every
	// table indexed by a msgKind.
	msgKindCount
)

// msgKindLabel is the metric-name suffix of each kind: the Bolt message name
// lower-cased, carrying no character a Prometheus metric name may not hold, so
// the published series name is decided here rather than by whatever sanitising
// a backend happens to apply.
var msgKindLabel = [msgKindCount]string{
	msgHello:    "hello",
	msgLogon:    "logon",
	msgLogoff:   "logoff",
	msgGoodbye:  "goodbye",
	msgReset:    "reset",
	msgRun:      "run",
	msgPull:     "pull",
	msgDiscard:  "discard",
	msgBegin:    "begin",
	msgCommit:   "commit",
	msgRollback: "rollback",
	msgRoute:    "route",
	msgOther:    "other",
}

// metricMsgLatencyPrefix is the base of the per-message latency series: the
// module-wide "<package-path>.<ExportedSymbol>" latency name for the public
// blocking API being instrumented, plus the ".message" dimension key.
const metricMsgLatencyPrefix = "bolt.server.HandleMessage.message."

// msgLatencySeries[k] is the latency histogram name for message kind k, built
// once at init so an emission never concatenates.
var msgLatencySeries = func() [msgKindCount]string {
	var names [msgKindCount]string
	for i := range names {
		names[i] = metricMsgLatencyPrefix + msgKindLabel[i]
	}
	return names
}()

// msgKindOf classifies one inbound message. The switch is ordered by the
// frequency the message appears on a live connection — RUN and PULL are sent
// once per query, the session and transaction messages far less often — so the
// common case settles in the fewest comparisons.
func msgKindOf(msg any) msgKind {
	switch msg.(type) {
	case *proto.Run:
		return msgRun
	case *proto.Pull:
		return msgPull
	case *proto.Begin:
		return msgBegin
	case *proto.Commit:
		return msgCommit
	case *proto.Rollback:
		return msgRollback
	case *proto.Discard:
		return msgDiscard
	case *proto.Reset:
		return msgReset
	case *proto.Route:
		return msgRoute
	case *proto.Hello:
		return msgHello
	case *proto.Logon:
		return msgLogon
	case *proto.Logoff:
		return msgLogoff
	case *proto.Goodbye:
		return msgGoodbye
	default:
		return msgOther
	}
}
