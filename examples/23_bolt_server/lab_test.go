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
	// The workload dimension must survive the fork too: a child that inherited
	// the DEFAULT shape would run a whole ladder against the wrong workload and
	// say nothing about it.
	parent.workload = workloadRecords
	parent.rows = 37
	parent.writeSlots = 2048

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
		{"server-log", child.serverLog, parent.serverLogMode()},
		{"workload", child.workload, parent.workloadKind()},
		{"rows", child.rows, parent.rowsPerQuery()},
		{"write-slots", child.writeSlots, parent.writeSlots},
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

// ─────────────────────────────────────────────────────────────────────────────
// Workload shapes
// ─────────────────────────────────────────────────────────────────────────────

// TestRunWorkloadShapes drives every shape end to end over real sockets and
// asserts what each one claims about the server, not merely that it completed.
//
// The three claims are the ones the round-3 campaign rests on, so a regression
// in any of them would silently invalidate a whole sweep rather than fail it:
// the RECORD-heavy shape must return EXACTLY the rows it asked for, the two
// explicit shapes must make bolt.server.tx.opened non-zero and balanced against
// tx.closed, and the write shape's committed effect must be readable back.
func TestRunWorkloadShapes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workload     string
		rows         int
		wantRecords  int   // total RECORDs the run must report, 0 = queries
		wantTxOpened bool  // bolt.server.tx.opened must be > 0
		wantCount    int64 // q.count_person, 0 = not reported
	}{
		{name: "count", workload: workloadCount, wantTxOpened: false, wantCount: 300},
		{name: "records", workload: workloadRecords, rows: 25, wantRecords: 240 * 25},
		{name: "txread", workload: workloadTxRead, wantTxOpened: true, wantCount: 300},
		{name: "txwrite", workload: workloadTxWrite, wantTxOpened: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := labConfig()
			cfg.workload = tc.workload
			cfg.rows = tc.rows
			var out bytes.Buffer
			if err := run(context.Background(), &out, cfg); err != nil {
				t.Fatalf("run %s: %v\n%s", tc.workload, err, out.String())
			}
			got := out.String()

			mustLine(t, got, "config.workload="+tc.workload)
			mustLine(t, got, "queries.ok="+strconv.Itoa(cfg.queries))

			if tc.wantCount > 0 {
				mustLine(t, got, "q.count_person="+strconv.FormatInt(tc.wantCount, 10))
			}
			if tc.wantRecords > 0 {
				mustLine(t, got, "records.ok="+strconv.Itoa(tc.wantRecords))
			}
			if tc.workload == workloadTxWrite {
				// The committed effect: one increment per successful unit,
				// read back through the engine and not over the wire.
				mustLine(t, got, "write.committed_delta="+strconv.Itoa(cfg.queries))
				mustLine(t, got, "nodes.counter="+strconv.Itoa(writeSlots))
			}

			// bolt.server.tx.opened is the evidence the transaction path ran.
			// A zero here on an explicit shape would make every claim about the
			// registry a non-observation dressed up as an exoneration.
			openedLine := "# counters.bolt.server.tx.opened=" + strconv.Itoa(cfg.queries)
			if tc.wantTxOpened {
				mustLine(t, got, openedLine)
				mustLine(t, got, "# counters.bolt.server.tx.closed="+strconv.Itoa(cfg.queries))
			} else if strings.Contains(got, "# counters.bolt.server.tx.opened=") {
				t.Errorf("auto-commit shape %s opened an explicit transaction:\n%s", tc.workload, got)
			}

			// Every shape must still leave the connection accounting balanced.
			mustLine(t, got, "# counters.bolt.server.conn.accepted="+strconv.Itoa(cfg.connections))
			mustLine(t, got, "# counters.bolt.server.conn.closed="+strconv.Itoa(cfg.connections))
			mustLine(t, got, "# counters.bolt.server.conn.panics=0")
			mustLine(t, got, "# server shut down cleanly")
		})
	}
}

// mustLine fails the test unless got contains want as a whole line.
func mustLine(t *testing.T, got, want string) {
	t.Helper()
	for _, line := range strings.Split(got, "\n") {
		if line == want {
			return
		}
	}
	t.Errorf("missing line %q in:\n%s", want, got)
}

// TestCheckWindowRejectsAWindowThatDidNotRun pins the per-shape correctness
// gate itself. It is the check that turns "the transaction path was never
// exercised" from a silent non-observation into a failed run, so a regression
// in it would not fail anything else.
func TestCheckWindowRejectsAWindowThatDidNotRun(t *testing.T) {
	opened := func(n uint64) map[string]uint64 {
		return map[string]uint64{metricNameTxOpened: n, metricNameTxClosed: n}
	}
	for _, tc := range []struct {
		name    string
		cfg     config
		st      repStats
		wantErr string
	}{
		{
			name:    "no query completed",
			cfg:     config{workload: workloadCount},
			st:      repStats{QueriesOK: 0},
			wantErr: "no query",
		},
		{
			name:    "record shortfall",
			cfg:     config{workload: workloadRecords, nodes: 100, rows: 10},
			st:      repStats{QueriesOK: 5, RecordsOK: 49},
			wantErr: "records: consumed 49, want 50",
		},
		{
			name:    "write reported success but committed nothing",
			cfg:     config{workload: workloadTxWrite},
			st:      repStats{QueriesOK: 7, WriteDelta: 0, WriteExpected: 7, Counters: opened(7)},
			wantErr: "committed effect",
		},
		{
			name:    "explicit shape opened no transaction",
			cfg:     config{workload: workloadTxRead},
			st:      repStats{QueriesOK: 7, Counters: map[string]uint64{}},
			wantErr: "opened no explicit transaction",
		},
		{
			name:    "one BEGIN per unit was expected",
			cfg:     config{workload: workloadTxRead},
			st:      repStats{QueriesOK: 7, Counters: opened(6)},
			wantErr: "for 7 successful units",
		},
		{
			name: "a transaction was left open",
			cfg:  config{workload: workloadTxRead},
			st: repStats{QueriesOK: 7, Counters: map[string]uint64{
				metricNameTxOpened: 7, metricNameTxClosed: 6}},
			wantErr: "left open",
		},
		{
			name: "a transaction was abandoned",
			cfg:  config{workload: workloadTxRead},
			st: repStats{QueriesOK: 7, Counters: map[string]uint64{
				metricNameTxOpened: 7, metricNameTxClosed: 7, metricNameTxAbandoned: 1}},
			wantErr: "still open when its connection tore down",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWindow(&tc.cfg, &tc.st)
			if err == nil {
				t.Fatalf("checkWindow accepted a window it should reject")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("checkWindow error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	// And the happy paths must pass, or the gate would reject every run.
	for _, ok := range []struct {
		name string
		cfg  config
		st   repStats
	}{
		{"auto-commit read", config{workload: workloadCount}, repStats{QueriesOK: 7}},
		{"records exact", config{workload: workloadRecords, nodes: 100, rows: 10},
			repStats{QueriesOK: 5, RecordsOK: 50}},
		{"explicit read", config{workload: workloadTxRead}, repStats{QueriesOK: 7, Counters: opened(7)}},
		{"explicit write", config{workload: workloadTxWrite},
			repStats{QueriesOK: 7, WriteDelta: 7, WriteExpected: 7, Counters: opened(7)}},
	} {
		if err := checkWindow(&ok.cfg, &ok.st); err != nil {
			t.Errorf("checkWindow(%s) = %v, want nil", ok.name, err)
		}
	}
}

// TestWorkloadSpecDerivations pins the shape resolution: which statement each
// shape runs, which access mode it opens its session with, and whether it is
// explicit. The access mode is load-bearing rather than cosmetic — the server
// treats BEGIN's mode field as a capability restriction and refuses a write
// inside a read-only transaction.
func TestWorkloadSpecDerivations(t *testing.T) {
	base := func(kind string) config {
		c := defaultConfig()
		c.workload = kind
		return c
	}
	for _, tc := range []struct {
		kind         string
		wantExplicit bool
		wantWrites   bool
		wantRows     int
		wantQuery    string
	}{
		{workloadCount, false, false, 1, countPersonQuery},
		{workloadRecords, false, false, defaultRows, ""},
		{workloadTxRead, true, false, 1, countPersonQuery},
		{workloadTxWrite, true, true, 1, incCounterQuery},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c := base(tc.kind)
			w := c.workloadSpec()
			if w.kind != tc.kind {
				t.Errorf("kind = %q, want %q", w.kind, tc.kind)
			}
			if w.explicit != tc.wantExplicit {
				t.Errorf("explicit = %v, want %v", w.explicit, tc.wantExplicit)
			}
			if c.explicitTx() != tc.wantExplicit {
				t.Errorf("config.explicitTx() = %v, want %v", c.explicitTx(), tc.wantExplicit)
			}
			if c.writesGraph() != tc.wantWrites {
				t.Errorf("config.writesGraph() = %v, want %v", c.writesGraph(), tc.wantWrites)
			}
			if w.rows != tc.wantRows {
				t.Errorf("rows = %d, want %d", w.rows, tc.wantRows)
			}
			if tc.wantQuery != "" && w.query != tc.wantQuery {
				t.Errorf("query = %q, want %q", w.query, tc.wantQuery)
			}
			// Only the write shape seeds counters, so every read shape's graph
			// stays byte-identical to the one earlier rungs measured.
			if got, want := c.writeSlotCount(), 0; !tc.wantWrites && got != want {
				t.Errorf("writeSlotCount() = %d, want %d for a read shape", got, want)
			}
			if tc.wantWrites {
				if got := c.writeSlotCount(); got != writeSlots {
					t.Errorf("writeSlotCount() = %d, want %d", got, writeSlots)
				}
				// Distinct slots must map to distinct counters, or two
				// connections would collide on one node and the shape would
				// measure conflict resolution instead of the transaction path.
				a := w.params(0)[paramSlot]
				b := w.params(1)[paramSlot]
				if a == b {
					t.Errorf("slots 0 and 1 both map to counter %v", a)
				}
				if got := w.params(writeSlots)[paramSlot]; got != a {
					t.Errorf("slot %d maps to %v, want it to wrap to %v", writeSlots, got, a)
				}
			} else if w.params(3) != nil {
				t.Errorf("read shape bound parameters: %v", w.params(3))
			}
		})
	}

	// The RECORD-heavy shape's row count reaches the statement, and a zero
	// resolves to the documented default rather than to a failure.
	c := base(workloadRecords)
	c.rows = 7
	if got := c.workloadSpec().query; !strings.HasSuffix(got, "LIMIT 7") {
		t.Errorf("records query = %q, want it to end in LIMIT 7", got)
	}
	c.rows = 0
	if got := c.rowsPerQuery(); got != defaultRows {
		t.Errorf("rowsPerQuery() with rows unset = %d, want %d", got, defaultRows)
	}
}

// TestValidateRejectsWorkloadConfigs pins the configurations that would produce
// a whole wasted sweep rather than an error.
func TestValidateRejectsWorkloadConfigs(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*config)
		want string
	}{
		{"unknown shape", func(c *config) { c.workload = "nosuch" }, "workload"},
		{"negative rows", func(c *config) { c.rows = -1 }, "rows"},
		{"more rows than nodes", func(c *config) {
			c.workload = workloadRecords
			c.nodes = 50
			c.rows = 51
		}, "exceeds nodes"},
		{"negative write slots", func(c *config) { c.writeSlots = -1 }, "write-slots"},
		{"fewer counters than connections", func(c *config) {
			c.workload = workloadTxWrite
			c.connections = 64
			c.writeSlots = 8
		}, "share a counter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			tc.mut(&cfg)
			err := cfg.validate()
			if err == nil {
				t.Fatalf("validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validate error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
