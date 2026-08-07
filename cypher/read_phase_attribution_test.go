package cypher

// read_phase_attribution_test.go — the drift guard for the prefix transcription in
// read_phase_attribution_bench_test.go (rmp #2292).
//
// runReadPrefix transcribes [Engine.runRead] so a benchmark can stop after any phase.
// A transcription that drifts from the path it transcribes measures a path nobody runs,
// and it would drift SILENTLY: the benchmark would still produce plausible numbers.
// This test makes the full prefix answer the same question the production path answers,
// on the shapes the benchmark uses, so a divergence fails the suite instead of quietly
// invalidating an attribution.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestReadPhasePrefixMatchesRunRead(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id, bal: $b})", map[string]expr.Value{
			"id": expr.IntegerValue(int64(i)),
			"b":  expr.IntegerValue(int64(i * 10)),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	cases := []struct {
		name   string
		query  string
		params map[string]expr.Value
	}{
		{
			name:   "point lookup (the benchmark's reader)",
			query:  phaseReadQuery,
			params: map[string]expr.Value{"id": expr.IntegerValue(3)},
		},
		{
			name:  "label scan count",
			query: "MATCH (n:Acct) RETURN count(n) AS c",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every prefix short of the full one must succeed without executing, so a
			// phase boundary that started returning an error would be caught rather
			// than silently reported as a fast phase.
			for _, p := range []readPhase{phaseParse, phaseSnapshot, phaseBuild} {
				if err := eng.runReadPrefix(ctx, p, tc.query, tc.params); err != nil {
					t.Fatalf("prefix phase %d: %v", p, err)
				}
			}
			// The full prefix must agree with the production path, which is what makes
			// it a transcription rather than a different query.
			if err := eng.runReadPrefix(ctx, phaseFull, tc.query, tc.params); err != nil {
				t.Fatalf("full prefix: %v", err)
			}
			res, err := eng.Run(ctx, tc.query, tc.params)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var want []string
			for res.Next() {
				want = append(want, renderRecord(res.Record()))
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Run drain: %v", err)
			}
			if len(want) == 0 {
				t.Fatalf("the production path returned no rows, so this case cannot " +
					"detect drift — fix the fixture, not the assertion")
			}
			got := collectPrefixRows(t, eng, tc.query, tc.params)
			if len(got) != len(want) {
				t.Fatalf("prefix returned %d rows, Run returned %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d: prefix %s, Run %s", i, got[i], want[i])
				}
			}
		})
	}
}

// collectPrefixRows re-runs the full prefix and captures its rows. runReadPrefix
// discards them by design — a benchmark must not pay to accumulate them — so the rows
// are gathered here through the same path with a collecting drain.
func collectPrefixRows(t *testing.T, eng *Engine, query string, params map[string]expr.Value) []string {
	t.Helper()
	ctx := context.Background()
	entry, err := eng.parseAndAnalyse(query)
	if err != nil {
		t.Fatalf("parseAndAnalyse: %v", err)
	}
	queryReg := newNowAwareRegistry(eng.reg, time.Now())
	snap := eng.g.BeginRead()
	defer eng.g.EndRead(snap)
	op, cols, err := eng.buildReadPhysical(ctx, entry, entry.plan, params, queryReg, nil, snap)
	if err != nil {
		t.Fatalf("buildReadPhysical: %v", err)
	}
	rs := exec.Run(ctx, op, cols)
	r := newResultWithLimit(rs, cols, nil, nil, nil, eng.maxResultRows, eng.maxResultBytes)
	r.globalMem = eng.globalMem
	r.notifications = entry.notifications
	r.materialize()
	var out []string
	for r.Next() {
		out = append(out, renderRecord(r.Record()))
	}
	if err := r.Err(); err != nil {
		t.Fatalf("prefix drain: %v", err)
	}
	return out
}

// renderRecord flattens a record to a deterministic string so two paths' rows can be
// compared without depending on map iteration order. A record is a map of column name to
// value, and this test cares only whether the two paths agree, not about typed equality
// semantics — which the rest of the suite covers exhaustively.
func renderRecord(rec exec.Record) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('|')
		}
		fmt.Fprintf(&sb, "%s=%v", k, rec[k])
	}
	return sb.String()
}
