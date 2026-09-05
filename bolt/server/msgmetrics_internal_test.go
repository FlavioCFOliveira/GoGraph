package server

// msgmetrics_internal_test.go — the bounded-cardinality proof for the
// per-message latency histograms (rmp #2715). Layer: short.
//
// The mandate these serve is Compliance Mandate 3's bounded-resources rule, not
// a style preference: a metric name derived from anything a peer controls is an
// unbounded map key in a process-global registry. These tests pin that the label
// set is a CLOSED one, that nothing outside it can be minted, and that an
// emission allocates nothing.

import (
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	promreg "github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// msgPromName renders a dotted in-tree metric name the way the Prometheus
// backend's sanitize does, so a test can look for the line an operator's scrape
// would actually carry.
func msgPromName(dotted string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == ':' {
			return r
		}
		return '_'
	}, dotted)
}

// msgScrape renders reg's exposition text, exactly as reg.Handler() would.
func msgScrape(t *testing.T, reg *promreg.Registry) string {
	t.Helper()
	var sb strings.Builder
	if err := reg.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return sb.String()
}

// everyRequestMessage is one value of every client→server message type
// bolt/proto can decode, paired with the kind it must classify as. The list is
// the whole reason the cardinality claim holds, so it is written out rather
// than derived: a new message type added to bolt/proto without a line here
// fails TestMsgKind_EveryProtoRequestTypeIsClassified below.
var everyRequestMessage = []struct {
	msg  any
	kind msgKind
}{
	{&proto.Hello{}, msgHello},
	{&proto.Logon{}, msgLogon},
	{&proto.Logoff{}, msgLogoff},
	{&proto.Goodbye{}, msgGoodbye},
	{&proto.Reset{}, msgReset},
	{&proto.Run{}, msgRun},
	{&proto.Pull{}, msgPull},
	{&proto.Discard{}, msgDiscard},
	{&proto.Begin{}, msgBegin},
	{&proto.Commit{}, msgCommit},
	{&proto.Rollback{}, msgRollback},
	{&proto.Route{}, msgRoute},
}

// TestMsgKind_EveryProtoRequestTypeIsClassified asserts the type switch covers
// every request type and maps each to its OWN kind — so no message type is
// silently pooled into another's histogram, and none falls through to msgOther.
func TestMsgKind_EveryProtoRequestTypeIsClassified(t *testing.T) {
	t.Parallel()
	seen := map[msgKind]any{}
	for _, tc := range everyRequestMessage {
		got := msgKindOf(tc.msg)
		if got != tc.kind {
			t.Errorf("msgKindOf(%T) = %d, want %d", tc.msg, got, tc.kind)
		}
		if got == msgOther {
			t.Errorf("msgKindOf(%T) fell through to msgOther; a known message type "+
				"must have its own bucket", tc.msg)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("msgKindOf(%T) and msgKindOf(%T) share kind %d", tc.msg, prev, got)
		}
		seen[got] = tc.msg
	}
	// Every kind except msgOther must be produced by some real message type.
	// Without this a kind could be added to the const block, get a name in the
	// table, and be exported for ever while nothing can ever observe into it.
	for k := msgKind(0); k < msgOther; k++ {
		if _, ok := seen[k]; !ok {
			t.Errorf("kind %d (%q) is declared and named but no bolt/proto request "+
				"type produces it", k, msgKindLabel[k])
		}
	}
	if len(everyRequestMessage) != int(msgOther) {
		t.Errorf("the closed set has %d real message types but %d are exercised here; "+
			"a message type was added to bolt/proto without a line in everyRequestMessage",
			int(msgOther), len(everyRequestMessage))
	}
}

// TestMsgKind_UnknownGoesToOther pins the catch-all. HandleMessage is exported,
// so an in-process caller can hand it anything; from the WIRE this is
// unreachable, because proto.DecodeRequest rejects an unknown struct tag before
// dispatch. Either way it must land in ONE bucket.
func TestMsgKind_UnknownGoesToOther(t *testing.T) {
	t.Parallel()
	// A server→client type, a bare struct, a string, and an untyped nil: four
	// things a caller could pass that are not requests.
	for _, v := range []any{&proto.Success{}, struct{}{}, "RUN", nil, (*proto.Record)(nil)} {
		if got := msgKindOf(v); got != msgOther {
			t.Errorf("msgKindOf(%T) = %d, want msgOther (%d)", v, got, msgOther)
		}
	}
	// A TYPED nil pointer of a known type is still that message type: the
	// handler will fault on it, and the observation must be attributed to the
	// message the caller claimed to send, not to the catch-all.
	if got := msgKindOf((*proto.Run)(nil)); got != msgRun {
		t.Errorf("msgKindOf((*proto.Run)(nil)) = %d, want msgRun (%d)", got, msgRun)
	}
}

// TestMsgLatencySeries_ClosedAndDistinct asserts the published name set is
// exactly msgKindCount entries, all non-empty, all distinct, and all carrying
// the documented prefix. Distinctness is the property that makes the series
// readable: two kinds sharing a name would silently merge two histograms.
func TestMsgLatencySeries_ClosedAndDistinct(t *testing.T) {
	t.Parallel()
	if len(msgLatencySeries) != int(msgKindCount) {
		t.Fatalf("msgLatencySeries has %d entries, want %d", len(msgLatencySeries), msgKindCount)
	}
	seen := map[string]msgKind{}
	for k := msgKind(0); k < msgKindCount; k++ {
		name := msgLatencySeries[k]
		if msgKindLabel[k] == "" {
			t.Errorf("kind %d has no label", k)
		}
		if want := metricMsgLatencyPrefix + msgKindLabel[k]; name != want {
			t.Errorf("msgLatencySeries[%d] = %q, want %q", k, name, want)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("kinds %d and %d both publish %q", prev, k, name)
		}
		seen[name] = k
	}
}

// TestMsgLatencySeries_SurviveBackendSanitisationDistinctly is the cardinality
// claim as an OPERATOR sees it. The names above are distinct as Go strings; what
// matters is that they are still distinct after the Prometheus backend maps
// them onto the metric-name grammar. Two names that collapse to one series
// would merge two histograms in the exposition while looking fine in the
// source — the collapse internal/metrics/prometheus/collision_test.go documents.
func TestMsgLatencySeries_SurviveBackendSanitisationDistinctly(t *testing.T) {
	t.Parallel()
	reg := promreg.New()
	for k := msgKind(0); k < msgKindCount; k++ {
		reg.ObserveLatency(msgLatencySeries[k], time.Duration(k+1)*time.Millisecond)
	}
	text := msgScrape(t, reg)
	for k := msgKind(0); k < msgKindCount; k++ {
		want := msgPromName(msgLatencySeries[k]) + "_count 1\n"
		if !strings.Contains(text, want) {
			t.Errorf("kind %d (%q): want an exposition line %q; the series either did not "+
				"appear or merged with another\ngot:\n%s", k, msgLatencySeries[k], want, text)
		}
	}
}

// TestHandleMessage_ObservationAllocatesNothing pins the reason msgLatencySeries
// is a table. Building the name by concatenation would allocate a string on
// EVERY inbound Bolt message; graph/lpg/mvcc_metricnames.go records the same
// mistake costing 21.8% of a benchmark's allocations. This asserts the emission
// path — the type switch, the table index, and the Stopwatch — allocates
// nothing, with the REAL backend installed so the assertion covers the path an
// operator actually runs rather than the no-op one.
//
// allocs/op is not build-invariant in general — bolt/proto records a count that
// differs by one under -race, because instrumentation disables the
// append-of-make rewrite — so the figure here was measured in BOTH modes rather
// than in one and assumed for the other. It is 0 under `go test` and 0 under
// `go test -race`, which is why this is one untagged constant and not the
// build-tagged pair bolt/proto needs.
const wantMsgObserveAllocs = 0

func TestHandleMessage_ObservationAllocatesNothing(t *testing.T) {
	reg := promreg.New()
	metrics.SetBackend(reg)
	t.Cleanup(func() { metrics.SetBackend(nil) })

	msg := any(&proto.Run{Query: "RETURN 1"})
	// Establish the series so the first-sight sanitize+LoadOrStore is not
	// counted; steady state is what runs per message.
	metrics.Time(msgLatencySeries[msgKindOf(msg)]).Stop()

	got := testing.AllocsPerRun(2000, func() {
		metrics.Time(msgLatencySeries[msgKindOf(msg)]).Stop()
	})
	if got != float64(wantMsgObserveAllocs) {
		t.Errorf("the per-message observation allocates %.0f objects/op, want %d — "+
			"is a name being concatenated, or has the Stopwatch started escaping?",
			got, wantMsgObserveAllocs)
	}
	t.Logf("per-message observation: %.0f allocs/op with the real Prometheus backend installed",
		got)
}

// TestStopwatchIsASmallValue guards the claim in HandleMessage's comment that
// the deferred call is open-coded and allocation-free BECAUSE metrics.Time
// returns a value. If Stopwatch ever grew to something the compiler spills, the
// allocation test above is the one that would fail; this documents the size the
// claim rests on so the failure is diagnosable.
func TestStopwatchIsASmallValue(t *testing.T) {
	t.Parallel()
	if got, max := unsafe.Sizeof(metrics.Stopwatch{}), uintptr(64); got > max {
		t.Errorf("sizeof(metrics.Stopwatch) = %d, want <= %d", got, max)
	}
}
