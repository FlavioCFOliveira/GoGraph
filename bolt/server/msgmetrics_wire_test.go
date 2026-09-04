package server_test

// msgmetrics_wire_test.go — the per-message latency histograms, driven over a
// REAL Bolt socket and read back through a real Prometheus scrape (rmp #2715).
// Layer: short.
//
// The internal test (msgmetrics_internal_test.go) proves the label set is
// closed and that an emission allocates nothing. This one proves the other
// half: that driving message X over the wire actually lands an observation in
// series X, and that the series reaches an operator in the exposition format
// they scrape. Those are different claims — a table of correct names that
// nothing ever emits into is exactly the shape a metric surface fails in.
//
// The metrics backend is process-global, swapped via metrics.SetBackend, so
// this test does not call t.Parallel and restores the no-op default on cleanup,
// exactly as server_metrics_test.go does.

import (
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/bolt/server"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	promreg "github.com/FlavioCFOliveira/GoGraph/internal/metrics/prometheus"
)

// msgSeries is the exposition name of one per-message histogram, as an
// operator's scrape carries it: dots mapped to underscores by the backend's
// sanitize. Written out literally rather than derived from the production
// table, so a change to the naming convention has to be made deliberately here
// too rather than tracking itself silently.
func msgSeries(kind string) string {
	return "bolt_server_HandleMessage_message_" + kind
}

// TestBoltMessageHistograms_PopulateOverTheWire drives one connection through
// every message type reachable with the no-auth handler and asserts each one's
// own histogram recorded at least one observation.
func TestBoltMessageHistograms_PopulateOverTheWire(t *testing.T) {
	reg := promreg.New()
	cmetrics.SetBackend(reg)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	addr := startTestServer(t, server.Options{ConnTimeout: 10 * time.Second})
	c := newBoltTestClient(t, addr)
	defer c.close(t)

	c.negotiate(t)
	c.hello(t)

	// An autocommit read: RUN then PULL.
	c.run(t, "RETURN 1 AS n", nil)
	if recs, _ := c.pullAll(t); len(recs) != 1 {
		t.Fatalf("pullAll: got %d records, want 1", len(recs))
	}

	// An explicit transaction, committed.
	c.begin(t)
	c.run(t, "RETURN 1 AS n", nil)
	c.pullAll(t)
	c.commit(t)

	// An explicit transaction, rolled back.
	c.begin(t)
	c.rollback(t)

	// DISCARD: run a statement and throw the rows away.
	c.run(t, "RETURN 1 AS n", nil)
	c.sendRequest(t, &proto.Discard{N: -1, QID: -1})
	c.recvSuccess(t)

	// ROUTE and RESET, then GOODBYE last (it ends the session).
	c.route(t)
	c.sendRequest(t, &proto.Reset{})
	c.recvSuccess(t)
	c.goodbye(t)

	// GOODBYE gets no response, so the observation may still be in flight when
	// the assertion runs. Wait for it rather than sleeping a fixed amount.
	text := waitForSeries(t, reg, msgSeries("goodbye")+"_count")

	for _, kind := range []string{"hello", "run", "pull", "begin", "commit",
		"rollback", "discard", "route", "reset", "goodbye"} {
		name := msgSeries(kind)
		if !strings.Contains(text, "# TYPE "+name+" histogram\n") {
			t.Errorf("%s: no histogram declared in the exposition; the %s message "+
				"recorded no latency observation\n%s", name, kind, boltHistLines(text))
			continue
		}
		if n := seriesCount(t, text, name); n == 0 {
			t.Errorf("%s_count = 0: the series exists but the %s message recorded "+
				"no observation", name, kind)
		}
	}

	// The catch-all must stay empty: nothing driven over the wire may land in
	// it. An unknown struct tag is rejected by proto.DecodeRequest before
	// dispatch, so "other" appearing here would mean a real message type had
	// fallen out of the type switch and was being pooled into the catch-all.
	if strings.Contains(text, msgSeries("other")) {
		t.Errorf("%s appeared after a wire-driven session: a real message type is "+
			"falling through to the catch-all\n%s", msgSeries("other"), boltHistLines(text))
	}
}

// TestBoltMessageHistograms_MalformedMessageMintsNoSeries is the cardinality
// claim against a hostile peer. A frame carrying an unknown struct tag is a
// message the server has no type for; if the metric name were derived from
// anything the peer sends, that is where an unbounded label would enter. It
// must instead produce NO new series at all — the decode fails before dispatch,
// so HandleMessage is never reached.
func TestBoltMessageHistograms_MalformedMessageMintsNoSeries(t *testing.T) {
	reg := promreg.New()
	cmetrics.SetBackend(reg)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })

	addr := startTestServer(t, server.Options{ConnTimeout: 10 * time.Second})
	c := newBoltTestClient(t, addr)
	defer c.close(t)
	c.negotiate(t)
	c.hello(t)

	before := boltSeriesNames(t, reg)

	// A PackStream struct with a tag no Bolt request uses (0x5A), zero fields.
	// The server answers Neo.ClientError.Request.Invalid and keeps the
	// connection, which is the path that must not touch the metric table.
	c.sendRawStruct(t, 0x5A)
	f := c.recvFailure(t)
	if f.Code != "Neo.ClientError.Request.Invalid" {
		t.Fatalf("unknown tag: got code %q, want Neo.ClientError.Request.Invalid", f.Code)
	}

	after := boltSeriesNames(t, reg)
	for name := range after {
		if !before[name] {
			t.Errorf("the malformed message minted a new series %q; the metric name "+
				"must never derive from the wire", name)
		}
	}
	// The connection must still be usable, and the next real message must still
	// be attributed correctly — the failure path leaving the table untouched is
	// only interesting if the server is still serving.
	c.run(t, "RETURN 1 AS n", nil)
	c.pullAll(t)
	if n := seriesCount(t, msgScrapeText(t, reg), msgSeries("run")); n == 0 {
		t.Error("after a rejected malformed message, a valid RUN recorded no observation")
	}
}

// boltSeriesNames returns the set of bolt_server_HandleMessage_* histogram
// names currently present in reg's exposition.
func boltSeriesNames(t *testing.T, reg *promreg.Registry) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, line := range strings.Split(msgScrapeText(t, reg), "\n") {
		rest, ok := strings.CutPrefix(line, "# TYPE ")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, " ")
		if strings.HasPrefix(name, "bolt_server_HandleMessage_") {
			names[name] = true
		}
	}
	return names
}

// boltHistLines returns just the Bolt per-message lines of an exposition, for a
// failure message that would otherwise dump the module's whole metric surface.
func boltHistLines(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "bolt_server_HandleMessage_") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return "(no bolt_server_HandleMessage_* series in the exposition at all)\n"
	}
	return b.String()
}

// msgScrapeText renders reg's exposition exactly as reg.Handler() would.
func msgScrapeText(t *testing.T, reg *promreg.Registry) string {
	t.Helper()
	var sb strings.Builder
	if err := reg.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return sb.String()
}

// seriesCount reads the "<name>_count N" sample out of an exposition.
func seriesCount(t *testing.T, text, name string) int {
	t.Helper()
	prefix := name + "_count "
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			n := 0
			for _, r := range v {
				if r < '0' || r > '9' {
					t.Fatalf("%s: unparseable count %q", name, v)
				}
				n = n*10 + int(r-'0')
			}
			return n
		}
	}
	return 0
}

// waitForSeries polls the exposition until want appears, and returns the
// exposition text that contained it. GOODBYE is answered with no response, so
// the server-side observation can lag the client's send; polling makes the test
// wait for the event rather than for a fixed duration that would be either
// flaky or slow.
func waitForSeries(t *testing.T, reg *promreg.Registry, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var text string
	for {
		text = msgScrapeText(t, reg)
		if strings.Contains(text, want) {
			return text
		}
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %q in the exposition\n%s", want, boltHistLines(text))
			return text
		}
		time.Sleep(2 * time.Millisecond)
	}
}
