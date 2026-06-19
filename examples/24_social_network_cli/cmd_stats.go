package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// cmdStats counts nodes by label and edges by relationship type and
// writes a single JSON object with the alphabetical keys
// authored / comments / follows / likes / on / posts / replies /
// users to stdout. The counts are computed through the same
// WAL-backed Cypher engine used by `query` so they reflect the
// committed state at invocation time.
//
//	{"authored":N,"comments":N,"follows":N,"likes":N,
//	 "on":N,"posts":N,"replies":N,"users":N}
//
// With the opt-in -evidence flag the JSON fact line is followed by
// "# "-prefixed telemetry: the graph order and size, the live Go heap,
// and the per-query wall-clock latency of each count. Telemetry is off by
// default so the deterministic output is unchanged.
func cmdStats(args []string) error {
	dir, evidence, err := parseStatsArgs(args)
	if err != nil {
		return err
	}
	return runStatsWithEvidence(context.Background(), dir, os.Stdout, evidence)
}

// parseStatsArgs parses the stats subcommand's flags: the shared -d <dir>
// and the opt-in -evidence toggle. A flag-parse failure or a missing -d is
// mapped to a *usageError (exit code 2).
func parseStatsArgs(args []string) (dir string, evidence bool, err error) {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dir, "d", "", "data directory (required)")
	fs.BoolVar(&evidence, "evidence", false, "print \"# \" telemetry (graph order/size, live heap, per-query latency)")
	if perr := fs.Parse(args); perr != nil {
		return "", false, newUsageError("stats: flag parse: %v", perr)
	}
	if dir == "" {
		return "", false, newUsageError("stats: missing required flag -d <dir>")
	}
	return dir, evidence, nil
}

// runStats is the entry point used by both cmdStats and the round-trip
// test in T9. It is kept at its original signature and emits only the
// deterministic JSON fact line (no telemetry), so the golden tests see
// byte-for-byte the same output. It delegates to runStatsWithEvidence with
// evidence off.
func runStats(ctx context.Context, dir string, out io.Writer) error {
	return runStatsWithEvidence(ctx, dir, out, false)
}

// runStatsWithEvidence computes the eight counts and writes them as one
// JSON fact line; when evidence is set it then writes "# "-prefixed
// telemetry. The counts are always the deterministic facts; only the
// latency, order/size, and heap lines vary per run.
func runStatsWithEvidence(ctx context.Context, dir string, out io.Writer, evidence bool) (retErr error) {
	o, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := o.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("stats: close store: %w", cerr)
		}
	}()

	counts := make(map[string]any, len(statQueries))
	latencies := make(map[string]time.Duration, len(statQueries))
	for _, q := range statQueries {
		start := time.Now()
		n, err := countOne(ctx, o.engine, q.query)
		if err != nil {
			return fmt.Errorf("stats: %s: %w", q.key, err)
		}
		latencies[q.key] = time.Since(start)
		counts[q.key] = n
	}
	if err := writeJSONObject(out, counts); err != nil {
		return err
	}
	if evidence {
		writeStatsTelemetry(out, o.graph.AdjList().Order(), o.graph.AdjList().Size(), latencies)
	}
	return nil
}

// writeStatsTelemetry emits the volatile stats evidence as "# " lines:
// the graph order (node slots) and size (edges) read straight from the
// adjacency list, the live Go heap, and the per-query latency for each of
// the eight count queries (in the same fixed statQueries order so the
// telemetry block is itself stable in shape, only its values vary).
func writeStatsTelemetry(out io.Writer, order, size uint64, latencies map[string]time.Duration) {
	writeTelemetry(out, "graph.order", fmt.Sprintf("%d", order))
	writeTelemetry(out, "graph.size", fmt.Sprintf("%d", size))
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeTelemetry(out, "mem.heap_alloc", humanBytes(m.HeapAlloc))
	for _, q := range statQueries {
		writeTelemetry(out, "q."+q.key+".latency", latencies[q.key].Round(time.Microsecond).String())
	}
}

// statQueries lists the eight Cypher count queries that produce the
// stats output, paired with the JSON key they fill. Keeping them in a
// single slice makes the contract from doc.go visible in one place.
var statQueries = []struct {
	key, query string
}{
	{"users", `MATCH (n:User) RETURN count(n) AS n`},
	{"posts", `MATCH (n:Post) RETURN count(n) AS n`},
	{"comments", `MATCH (n:Comment) RETURN count(n) AS n`},
	{"follows", `MATCH ()-[r:FOLLOWS]->() RETURN count(r) AS n`},
	{"authored", `MATCH ()-[r:AUTHORED]->() RETURN count(r) AS n`},
	{"on", `MATCH ()-[r:ON]->() RETURN count(r) AS n`},
	{"replies", `MATCH ()-[r:REPLY_OF]->() RETURN count(r) AS n`},
	{"likes", `MATCH ()-[r:LIKED]->() RETURN count(r) AS n`},
}

// countOne runs a single count(*) Cypher query against eng and
// returns the integer result from the column named "n".
func countOne(ctx context.Context, eng *cypher.Engine, query string) (int64, error) {
	res, err := eng.RunInTx(ctx, query, nil)
	if err != nil {
		return 0, fmt.Errorf("run: %w", err)
	}
	defer func() { _ = res.Close() }()

	var n int64
	for res.Next() {
		rec := res.Record()
		v, ok := rec["n"]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case int64:
			n = x
		case int:
			n = int64(x)
		default:
			if iv, ok := jsonValue(v).(int64); ok {
				n = iv
			}
		}
	}
	if err := res.Err(); err != nil {
		return 0, fmt.Errorf("iterate: %w", err)
	}
	return n, nil
}
