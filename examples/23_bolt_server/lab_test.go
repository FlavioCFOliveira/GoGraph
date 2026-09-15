package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// labConfig is the connection-laboratory counterpart of testConfig: the same
// model and the same code path, sized so that a rung completes well inside the
// short-layer package budget while still opening real, independent sockets.
func labConfig() config {
	return config{
		nodes:          300,
		knowsMin:       2,
		knowsMax:       4,
		queries:        240,
		sessions:       1,
		seed:           42,
		connections:    8,
		maxConnections: 16,
		repetitions:    1,
		connectTimeout: 5 * time.Second,
		mutexFraction:  1,
		blockRate:      1,
	}
}

// TestRunConnectionMode drives the independent-connection path and asserts the
// dimension does what it claims: every offered connection becomes a live
// server-side connection, and the server's accepted counter agrees with the
// client's own count.
func TestRunConnectionMode(t *testing.T) {
	var buf bytes.Buffer
	cfg := labConfig()
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	facts := parseFacts(t, out)

	if got := facts["config.connections"]; got != int64(cfg.connections) {
		t.Errorf("config.connections = %d, want %d", got, cfg.connections)
	}
	if got := facts["config.max_connections"]; got != int64(cfg.maxConnections) {
		t.Errorf("config.max_connections = %d, want %d", got, cfg.maxConnections)
	}
	// Every query must have succeeded: nothing is being refused at this rung.
	if got := facts["queries.ok"]; got != int64(cfg.queries) {
		t.Errorf("queries.ok = %d, want %d", got, cfg.queries)
	}
	if got := telemetryInt(t, out, "conn.established"); got != int64(cfg.connections) {
		t.Errorf("conn.established = %d, want %d", got, cfg.connections)
	}
	if got := telemetryInt(t, out, "conn.failed"); got != 0 {
		t.Errorf("conn.failed = %d, want 0", got)
	}
	// The server's own view must account for at least the offered connections.
	if got := telemetryInt(t, out, "counters.bolt.server.conn.accepted"); got < int64(cfg.connections) {
		t.Errorf("bolt.server.conn.accepted = %d, want >= %d", got, cfg.connections)
	}
	if got := telemetryInt(t, out, "counters.bolt.server.conn.rejected"); got != 0 {
		t.Errorf("bolt.server.conn.rejected = %d, want 0 below the semaphore", got)
	}
}

// TestRunSaturationRejects is the reject-branch experiment in miniature: offer
// twice the connections the semaphore admits, and require that the run
// completes, that no more than the semaphore's worth are admitted, and that the
// refusal is visible in bolt.server.conn.rejected.
//
// The counter is the only evidence there is. A rejected connection never
// becomes live, so neither the accepted/closed derivation nor anything the
// client can see distinguishes a refusal from a network failure.
func TestRunSaturationRejects(t *testing.T) {
	cfg := labConfig()
	cfg.connections = 16
	cfg.maxConnections = 8
	if !cfg.expectRejections() {
		t.Fatalf("config offers %d connections to a semaphore of %d but does not expect rejections",
			cfg.connections, cfg.effectiveMaxConnections())
	}

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()

	rejected := telemetryInt(t, out, "counters.bolt.server.conn.rejected")
	if rejected <= 0 {
		t.Errorf("bolt.server.conn.rejected = %d, want > 0:\n%s", rejected, out)
	}
	established := telemetryInt(t, out, "conn.established")
	if established <= 0 || established > int64(cfg.maxConnections) {
		t.Errorf("conn.established = %d, want in (0, %d]", established, cfg.maxConnections)
	}
	if failed := telemetryInt(t, out, "conn.failed"); failed != int64(cfg.connections)-established {
		t.Errorf("conn.failed = %d, want %d (offered %d - established %d)",
			failed, int64(cfg.connections)-established, cfg.connections, established)
	}
	if accepted := telemetryInt(t, out, "counters.bolt.server.conn.accepted"); accepted > int64(cfg.maxConnections) {
		t.Errorf("bolt.server.conn.accepted = %d, want <= the semaphore %d", accepted, cfg.maxConnections)
	}
}

// TestRunWritesArtefacts pins the artefact contract: an instrumented run writes
// every profile, the trace, and both machine-readable records, and the
// benchmark file carries exactly one record per unprofiled repetition.
func TestRunWritesArtefacts(t *testing.T) {
	dir := t.TempDir()
	cfg := labConfig()
	cfg.connections = 4
	cfg.maxConnections = 8
	cfg.repetitions = 2
	cfg.artifactDir = dir
	cfg.label = "unit"

	var buf bytes.Buffer
	if err := run(context.Background(), &buf, cfg); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, name := range []string{
		"cpu.pprof", "heap.pprof",
		mutexProfileName, blockProfileName, goroutineProfileName,
		traceName, metricsFileName, hostFileName, benchFileName,
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("artefact %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("artefact %s is empty", name)
		}
	}

	m, err := readRunMetrics(dir)
	if err != nil {
		t.Fatalf("read %s: %v", metricsFileName, err)
	}
	if m.Schema != metricsSchema {
		t.Errorf("schema = %q, want %q", m.Schema, metricsSchema)
	}
	if m.Label != "unit" {
		t.Errorf("label = %q, want %q", m.Label, "unit")
	}
	if len(m.Effect) != cfg.repetitions {
		t.Errorf("effect windows = %d, want %d", len(m.Effect), cfg.repetitions)
	}
	if m.Probe == nil {
		t.Error("probe window missing: an instrumented run must record the profiled window it profiled")
	}
	if m.Config.Connections != cfg.connections {
		t.Errorf("recorded connections = %d, want %d", m.Config.Connections, cfg.connections)
	}
	// The host record must never claim an idle host it did not measure.
	if !m.Host.LoadAvgReadable && m.Host.Idle {
		t.Error("host reported idle although its load average was never read")
	}
	if m.Host.IdleNote == "" {
		t.Error("host idle verdict carries no justification")
	}

	//nolint:gosec // G304: the path is this test's own t.TempDir joined with a fixed basename.
	bench, err := os.ReadFile(filepath.Join(dir, benchFileName))
	if err != nil {
		t.Fatalf("read %s: %v", benchFileName, err)
	}
	records := benchmarkLines(string(bench))
	if len(records) != cfg.repetitions {
		t.Errorf("benchmark records = %d, want one per unprofiled repetition (%d):\n%s",
			len(records), cfg.repetitions, bench)
	}
	for _, line := range records {
		assertBenchmarkLine(t, line)
	}
}

// TestParseLevels covers the ladder specification: ordering, de-duplication,
// whitespace, and the errors that must stop a sweep before it spends an hour
// writing artefacts under the wrong name.
func TestParseLevels(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		got, err := parseLevels(defaultLadderLevels)
		if err != nil {
			t.Fatalf("parseLevels(%q): %v", defaultLadderLevels, err)
		}
		want := []int{1, 8, 64, 256, 1024}
		if len(got) != len(want) {
			t.Fatalf("levels = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("levels = %v, want %v", got, want)
			}
		}
	})
	t.Run("sorted and deduplicated", func(t *testing.T) {
		got, err := parseLevels(" 64, 1 ,64,8 ")
		if err != nil {
			t.Fatalf("parseLevels: %v", err)
		}
		if len(got) != 3 || got[0] != 1 || got[1] != 8 || got[2] != 64 {
			t.Fatalf("levels = %v, want [1 8 64]", got)
		}
	})
	for _, bad := range []string{"", ",", "0", "-4", "eight", "8,x"} {
		t.Run("rejects "+strconv.Quote(bad), func(t *testing.T) {
			if got, err := parseLevels(bad); err == nil {
				t.Fatalf("parseLevels(%q) = %v, want an error", bad, got)
			}
		})
	}
}

// TestLadderSpecs asserts the shape of a sweep: every ladder rung is given a
// semaphore wide enough to admit everything it offers (so a rejection on the
// ladder proper is a defect, not the design), and the saturation rung is the
// one — and the only one — that offers more than it admits.
func TestLadderSpecs(t *testing.T) {
	levels := []int{1, 8, 64, 256, 1024}
	specs := ladderSpecs(levels, 256, 128)
	if len(specs) != len(levels)+1 {
		t.Fatalf("specs = %d, want %d ladder rungs plus saturation", len(specs), len(levels))
	}
	for i, lvl := range levels {
		s := specs[i]
		if s.name != "conn="+strconv.Itoa(lvl) {
			t.Errorf("spec[%d].name = %q, want conn=%d", i, s.name, lvl)
		}
		if s.connections != lvl {
			t.Errorf("spec[%d].connections = %d, want %d", i, s.connections, lvl)
		}
		if s.maxConnections <= s.connections {
			t.Errorf("spec[%d] offers %d connections to a semaphore of %d: the ladder must not refuse",
				i, s.connections, s.maxConnections)
		}
	}
	sat := specs[len(specs)-1]
	if sat.name != "saturation" {
		t.Errorf("last spec = %q, want saturation", sat.name)
	}
	if sat.connections <= sat.maxConnections {
		t.Errorf("saturation offers %d to a semaphore of %d: nothing would be refused",
			sat.connections, sat.maxConnections)
	}

	if got := ladderSpecs(levels, 0, 128); len(got) != len(levels) {
		t.Errorf("saturation offer 0 should drop the rung, got %d specs", len(got))
	}
}

// TestChildArgsRoundTrip parses a rung's child-process arguments back through
// the very flag set they were rendered for. A flag renamed on one side only
// would otherwise leave the sweep silently running every rung at the default —
// the sweep would still produce five directories, and every one of them would
// describe the same measurement.
func TestChildArgsRoundTrip(t *testing.T) {
	parent := labConfig()
	parent.nodes = 1234
	parent.queries = 4321
	parent.sessions = 3
	parent.seed = 99
	parent.repetitions = 7
	parent.connectTimeout = 3 * time.Second
	parent.artifactDir = "/tmp/parent"

	spec := rungSpec{name: "conn=64", connections: 64, maxConnections: 68}
	args := childArgs(&parent, spec, "/tmp/parent/conn=64")

	child := defaultConfig()
	fs := flag.NewFlagSet("child", flag.ContinueOnError)
	fs.SetOutput(new(bytes.Buffer))
	bindFlags(fs, &child)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse child args %v: %v", args, err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"nodes", child.nodes, parent.nodes},
		{"knows-min", child.knowsMin, parent.knowsMin},
		{"knows-max", child.knowsMax, parent.knowsMax},
		{"queries", child.queries, parent.queries},
		{"sessions", child.sessions, parent.sessions},
		{"seed", child.seed, parent.seed},
		{"connections", child.connections, spec.connections},
		{"max-connections", child.maxConnections, spec.maxConnections},
		{"repetitions", child.repetitions, parent.reps()},
		{"connect-timeout", child.connectTimeout, parent.dialTimeout()},
		{"mutex-fraction", child.mutexFraction, parent.mutexFraction},
		{"block-rate", child.blockRate, parent.blockRate},
		{"label", child.label, spec.name},
		{"artifact-dir", child.artifactDir, "/tmp/parent/conn=64"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("child %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// A child must never sweep: that is the parent's job, and a child that
	// inherited -ladder would fork for ever.
	if child.ladder {
		t.Error("child inherited -ladder")
	}
	if err := child.validate(); err != nil {
		t.Errorf("child config is invalid: %v", err)
	}
}

// TestConfigDerivations pins the accessors that turn a zero value into the
// documented default, and the semaphore derivation that preserves the value
// this example used before the connection dimension existed.
func TestConfigDerivations(t *testing.T) {
	var zero config
	if got := zero.reps(); got != 1 {
		t.Errorf("zero.reps() = %d, want 1", got)
	}
	if got := zero.dialTimeout(); got != 5*time.Second {
		t.Errorf("zero.dialTimeout() = %s, want 5s", got)
	}
	if got := zero.sessionsPerConn(); got != 1 {
		t.Errorf("zero.sessionsPerConn() = %d, want 1", got)
	}

	pooled := config{sessions: 4}
	if got := pooled.effectiveMaxConnections(); got != 4+connHeadroom {
		t.Errorf("pooled semaphore = %d, want %d", got, 4+connHeadroom)
	}
	if pooled.expectRejections() {
		t.Error("pooled mode cannot expect rejections: it does not control the socket count")
	}
	if got := pooled.rungLabel(); got != "pooled" {
		t.Errorf("pooled label = %q, want %q", got, "pooled")
	}

	conn := config{sessions: 4, connections: 64}
	if got := conn.effectiveMaxConnections(); got != 64+connHeadroom {
		t.Errorf("connection semaphore = %d, want %d", got, 64+connHeadroom)
	}
	if conn.expectRejections() {
		t.Error("a derived semaphore admits every connection offered")
	}
	if got := conn.rungLabel(); got != "conn=64" {
		t.Errorf("connection label = %q, want conn=64", got)
	}

	sat := config{sessions: 4, connections: 256, maxConnections: 128}
	if got := sat.effectiveMaxConnections(); got != 128 {
		t.Errorf("explicit semaphore = %d, want 128", got)
	}
	if !sat.expectRejections() {
		t.Error("256 connections offered to a semaphore of 128 must expect rejections")
	}
	explicit := config{label: "x", connections: 8}
	if got := explicit.rungLabel(); got != "x" {
		t.Errorf("explicit label = %q, want x", got)
	}
}

// TestValidateRejectsLabConfigs covers the boundary checks the laboratory added.
func TestValidateRejectsLabConfigs(t *testing.T) {
	base := labConfig()
	cases := map[string]func(*config){
		"negative connections":     func(c *config) { c.connections = -1 },
		"negative max-connections": func(c *config) { c.maxConnections = -1 },
		"negative repetitions":     func(c *config) { c.repetitions = -1 },
		"negative connect-timeout": func(c *config) { c.connectTimeout = -time.Second },
		"negative mutex-fraction":  func(c *config) { c.mutexFraction = -1 },
		"negative block-rate":      func(c *config) { c.blockRate = -1 },
		"ladder without artefacts": func(c *config) { c.ladder = true; c.artifactDir = "" },
		"saturation that refuses nothing": func(c *config) {
			c.ladder = true
			c.artifactDir = "/tmp/x"
			c.satOffer, c.satAdmit = 64, 64
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatalf("validate accepted %s", name)
			}
		})
	}
}

// TestCounterSink asserts the sink records the server counters it carries,
// ignores everything else, and reports a window as the difference between two
// snapshots — the only way to attribute a counter that can never be reset.
func TestCounterSink(t *testing.T) {
	s := newCounterSink()
	before := s.snapshot()
	if len(before) != len(serverCounters) {
		t.Fatalf("snapshot carries %d counters, want %d", len(before), len(serverCounters))
	}

	s.IncCounter("bolt.server.conn.accepted", 3)
	s.IncCounter("bolt.server.conn.rejected", 5)
	s.IncCounter("some.other.counter", 99)
	s.ObserveLatency("bolt.server.HandleMessage.message.run", time.Second)
	s.SetGauge("whatever", 1)

	after := s.snapshot()
	if _, ok := after["some.other.counter"]; ok {
		t.Error("sink recorded a counter outside the set it declares")
	}
	d := diffCounters(before, after)
	if d["bolt.server.conn.accepted"] != 3 {
		t.Errorf("accepted delta = %d, want 3", d["bolt.server.conn.accepted"])
	}
	if d["bolt.server.conn.rejected"] != 5 {
		t.Errorf("rejected delta = %d, want 5", d["bolt.server.conn.rejected"])
	}
	if d["bolt.server.conn.closed"] != 0 {
		t.Errorf("closed delta = %d, want 0", d["bolt.server.conn.closed"])
	}
	if got := liveConnections(after); got != 3 {
		t.Errorf("liveConnections = %d, want 3 (accepted 3 - closed 0)", got)
	}
}

// TestHostInfoNeverClaimsUnreadIdle is the guard on the rule that a run is
// never called idle on a load average nobody read.
func TestHostInfoNeverClaimsUnreadIdle(t *testing.T) {
	h := hostInfo{NumCPU: 8, IdleThreshold: 0.8, LoadAvgReadable: false}
	h.finish()
	if h.Idle {
		t.Error("host called idle although its load average was never read")
	}
	if !strings.Contains(h.IdleNote, "NOT certified idle") {
		t.Errorf("idle note = %q, want it to say the host is not certified idle", h.IdleNote)
	}

	busy := hostInfo{NumCPU: 8, IdleThreshold: 0.8, LoadAvgReadable: true, LoadAvgBefore: []float64{4, 3, 2}}
	busy.finish()
	if busy.Idle {
		t.Error("host called idle at loadavg1 4.0 against a threshold of 0.8")
	}
	if !strings.Contains(busy.IdleNote, "NOT IDLE") {
		t.Errorf("idle note = %q, want it to report NOT IDLE", busy.IdleNote)
	}

	quiet := hostInfo{NumCPU: 8, IdleThreshold: 0.8, LoadAvgReadable: true, LoadAvgBefore: []float64{0.1, 0.2, 0.3}}
	quiet.finish()
	if !quiet.Idle {
		t.Errorf("host not called idle at loadavg1 0.1 against a threshold of 0.8: %s", quiet.IdleNote)
	}
}

// TestNewHostInfoReadsThisHost checks the platform readers against the machine
// the test runs on, rather than only against synthetic values.
func TestNewHostInfoReadsThisHost(t *testing.T) {
	h := newHostInfo()
	if h.NumCPU <= 0 || h.GOMAXPROCS <= 0 {
		t.Errorf("cores = %d, gomaxprocs = %d, want both > 0", h.NumCPU, h.GOMAXPROCS)
	}
	if h.LoadAvgReadable {
		if len(h.LoadAvgBefore) != 3 {
			t.Fatalf("loadavg = %v, want three values", h.LoadAvgBefore)
		}
		for i, v := range h.LoadAvgBefore {
			if v < 0 || v > 4096 {
				t.Errorf("loadavg[%d] = %v, outside any plausible range", i, v)
			}
		}
	}
	if h.FDLimitReadable && h.FDLimitSoft == 0 {
		t.Error("descriptor soft limit read as 0")
	}
	if n, ok := openFDs(); ok && n <= 0 {
		t.Errorf("openFDs = %d, want > 0 when readable", n)
	}
}

// TestPercentilesIncludesP999 pins the four-percentile contract, p999 included:
// it is the percentile a connection-saturation run actually moves.
func TestPercentilesIncludesP999(t *testing.T) {
	if p50, p95, p99, p999 := percentiles(nil); p50|p95|p99|p999 != 0 {
		t.Errorf("percentiles(nil) = %d %d %d %d, want all zero", p50, p95, p99, p999)
	}
	latencies := make([]time.Duration, 1000)
	for i := range latencies {
		latencies[i] = time.Duration(i+1) * time.Millisecond
	}
	p50, p95, p99, p999 := percentiles(latencies)
	for _, c := range []struct {
		name string
		got  int64
		want time.Duration
	}{
		{"p50", p50, 500 * time.Millisecond},
		{"p95", p95, 950 * time.Millisecond},
		{"p99", p99, 990 * time.Millisecond},
		{"p999", p999, 999 * time.Millisecond},
	} {
		if time.Duration(c.got) != c.want {
			t.Errorf("%s = %s, want %s", c.name, time.Duration(c.got), c.want)
		}
	}
}

// TestBenchRecordShape asserts the benchmark file is something benchstat can
// read: a name, an iteration count, and value/unit pairs.
func TestBenchRecordShape(t *testing.T) {
	m := &runMetrics{
		Label: "conn=64",
		Host:  hostInfo{GOMAXPROCS: 10},
		Effect: []repStats{
			{QueriesOK: 2000, LoadNS: int64(2 * time.Second), Throughput: 1000, P99NS: 5, P999NS: 9, ConnectNS: 11, PeakGoroutines: 700},
			{QueriesOK: 2000, LoadNS: int64(time.Second), Throughput: 2000, P99NS: 4, P999NS: 8, ConnectNS: 12, PeakGoroutines: 701},
		},
		Probe: &repStats{QueriesOK: 2000, LoadNS: int64(9 * time.Second), Throughput: 222},
	}
	out := benchRecord(m)
	if !strings.HasPrefix(out, "goos: ") {
		t.Errorf("record does not start with the benchstat preamble:\n%s", out)
	}
	lines := benchmarkLines(out)
	if len(lines) != len(m.Effect) {
		t.Fatalf("records = %d, want %d — the profiled window must never appear:\n%s", len(lines), len(m.Effect), out)
	}
	for _, line := range lines {
		assertBenchmarkLine(t, line)
		if !strings.Contains(line, "/conn=64-10\t") {
			t.Errorf("record %q does not carry the rung sub-name and GOMAXPROCS suffix", line)
		}
	}
	if strings.Contains(out, "222") {
		t.Errorf("the profiled window's throughput leaked into the benchmark file:\n%s", out)
	}
}

// TestRunMetricsRoundTrip confirms the machine-readable record survives the
// write/read cycle the ladder depends on to summarise a child.
func TestRunMetricsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	labCfg := labConfig()
	want := &runMetrics{
		Schema: metricsSchema,
		Label:  "conn=8",
		Config: labCfg.asJSON(),
		Host:   newHostInfo(),
		Effect: []repStats{{ConnOffered: 8, ConnEstablished: 8, QueriesOK: 240, Counters: map[string]uint64{"bolt.server.conn.accepted": 8}}},
	}
	if err := writeRunArtefacts(dir, want); err != nil {
		t.Fatalf("writeRunArtefacts: %v", err)
	}
	got, err := readRunMetrics(dir)
	if err != nil {
		t.Fatalf("readRunMetrics: %v", err)
	}
	if got.Label != want.Label || got.Schema != want.Schema {
		t.Errorf("round trip = %q/%q, want %q/%q", got.Schema, got.Label, want.Schema, want.Label)
	}
	if len(got.Effect) != 1 || got.Effect[0].Counters["bolt.server.conn.accepted"] != 8 {
		t.Errorf("effect window did not survive the round trip: %+v", got.Effect)
	}

	// host.json must be readable on its own: it is what a later cycle reads to
	// decide whether a number may be compared with another run's.
	//nolint:gosec // G304: the path is this test's own t.TempDir joined with a fixed basename.
	b, err := os.ReadFile(filepath.Join(dir, hostFileName))
	if err != nil {
		t.Fatalf("read %s: %v", hostFileName, err)
	}
	var h hostInfo
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("decode %s: %v", hostFileName, err)
	}
	if h.NumCPU != want.Host.NumCPU {
		t.Errorf("host.json cores = %d, want %d", h.NumCPU, want.Host.NumCPU)
	}
}

// TestSplitWorkConserves asserts the query split loses nothing, at every
// division the ladder performs.
func TestSplitWorkConserves(t *testing.T) {
	for _, total := range []int{0, 1, 19, 240, 20000} {
		for _, parts := range []int{1, 8, 64, 256, 1024} {
			got := splitWork(total, parts)
			if len(got) != parts {
				t.Fatalf("splitWork(%d,%d) returned %d buckets", total, parts, len(got))
			}
			sum, minB, maxB := 0, got[0], got[0]
			for _, n := range got {
				sum += n
				minB = min(minB, n)
				maxB = max(maxB, n)
			}
			if sum != total {
				t.Errorf("splitWork(%d,%d) sums to %d", total, parts, sum)
			}
			if maxB-minB > 1 {
				t.Errorf("splitWork(%d,%d) is uneven: [%d,%d]", total, parts, minB, maxB)
			}
		}
	}
	if got := splitWork(10, 0); got != nil {
		t.Errorf("splitWork(10,0) = %v, want nil", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// telemetryInt reads the integer value of a "# key=value" telemetry line.
func telemetryInt(t *testing.T, out, key string) int64 {
	t.Helper()
	prefix := "# " + key + "="
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v := strings.TrimPrefix(line, prefix)
		if i := strings.IndexByte(v, ' '); i >= 0 {
			v = v[:i]
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("telemetry %s: %q does not parse as an integer", key, v)
		}
		return n
	}
	t.Fatalf("telemetry line %q missing from output:\n%s", key, out)
	return 0
}

// benchmarkLines returns the benchmark records of a benchstat file, dropping
// the environment preamble.
func benchmarkLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, benchmarkName) {
			out = append(out, line)
		}
	}
	return out
}

// assertBenchmarkLine checks one record against the Go benchmark format
// benchstat parses: a name, an iteration count, then value/unit pairs.
func assertBenchmarkLine(t *testing.T, line string) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 4 {
		t.Errorf("benchmark record %q has %d fields, want a name, an iteration count and at least one value/unit pair",
			line, len(fields))
		return
	}
	if _, err := strconv.Atoi(fields[1]); err != nil {
		t.Errorf("benchmark record %q: iteration count %q is not an integer", line, fields[1])
	}
	for i := 2; i+1 < len(fields); i += 2 {
		if _, err := strconv.ParseFloat(fields[i], 64); err != nil {
			t.Errorf("benchmark record %q: value %q is not a number", line, fields[i])
		}
		if fields[i+1] == "" {
			t.Errorf("benchmark record %q: value %q has no unit", line, fields[i])
		}
	}
	if len(fields)%2 != 0 {
		t.Errorf("benchmark record %q has a dangling field: values and units must pair up", line)
	}
}
