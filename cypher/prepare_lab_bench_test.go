package cypher

// prepare_lab_bench_test.go — statement preparation measured COLD and WARM over
// the examples statement corpus (rmp #2843, sprint 362 parsing campaign).
//
// # Why two paths
//
// A statement costs two entirely different things depending on whether the plan
// cache has seen it:
//
//   - COLD — the text is not in the cache. [Engine.parseAndAnalyse] resolves the
//     key with [parser.StripLiterals], misses, and compiles: parse, sema, IR
//     translation, and the memos [Engine.compilePlanCacheEntry] builds.
//   - WARM — the text is in the cache. The same key resolution runs, and the
//     lookup hits. Nothing is parsed.
//
// Reporting one number for "parsing a statement" would hide which of these a
// workload actually pays, so both are measured here, statement by statement.
//
// # What each arm runs
//
//   - PrepareCold — [parser.StripLiterals] then [Engine.compilePlanCacheEntry],
//     which is the miss path's work. It omits the cache probe that missed and
//     the [planBuildGroup] bookkeeping around it; the probe's own cost is what
//     PrepareWarm measures, minus the strip.
//   - PrepareWarm — [Engine.parseAndAnalyse] itself, with the entry already
//     published, so it is exactly strip + a hitting cache lookup.
//   - Sema — [sema.Analyse] + [sema.MapToBolt] + [sema.CollectParamNames] over
//     an AST parsed once, outside the timer: the pass the engine runs on the
//     parser's output, completing the front-end stage ladder that
//     cypher/parser/stage_bench_test.go starts.
//
// compilePlanCacheEntry publishes into the LRU on every iteration, which after
// the first is a hitting loadOrStore that returns the existing entry and drops
// the one just built. The build itself is done in full every time, so the
// number is a genuine cold compile, not an amortised one.
//
// # Running it
//
//	scripts/parse-lab.sh
//
// which is the reproducible form of:
//
//	go test -run=^$ -bench='^BenchmarkPrepare|^BenchmarkStageSema$' -benchmem \
//	    -benchtime=100ms -count=8 ./cypher/

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/cypher/sema"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labCorpusPath is cypher/parser/testdata/examples-corpus.json seen from this
// package's directory. One corpus serves both halves of the laboratory.
const labCorpusPath = "parser/testdata/examples-corpus.json"

type labStatement struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

func loadLabCorpus(tb testing.TB) []labStatement {
	tb.Helper()
	raw, err := os.ReadFile(labCorpusPath)
	if err != nil {
		tb.Fatalf("read corpus %s: %v", labCorpusPath, err)
	}
	var f struct {
		Statements []labStatement `json:"statements"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		tb.Fatalf("decode corpus %s: %v", labCorpusPath, err)
	}
	if len(f.Statements) == 0 {
		tb.Fatalf("corpus %s is empty", labCorpusPath)
	}
	return f.Statements
}

func labBenchName(id string) string { return strings.ReplaceAll(id, "/", "-") }

// newLabEngine builds an engine over an empty graph. The corpus is never
// executed here — only compiled — so the graph's contents do not enter the
// measurement. They are not entirely absent from it: compilePlanCacheEntry
// consults the index manager to infer parameter types, and an empty graph has
// no indexes, which is stated as a limit of the method.
func newLabEngine() *Engine {
	return NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
}

// Sinks. Assigned from every benchmark body so the compiler cannot delete the
// work being measured.
var (
	labSinkEntry *planCacheEntry
	labSinkErr   error
	labSinkMap   map[string]string
	labSinkSema  *sema.SemanticError
	labSinkNames []string
)

// BenchmarkPrepareCold measures a plan-cache MISS: key resolution plus a full
// compilation, per corpus statement.
func BenchmarkPrepareCold(b *testing.B) {
	for _, s := range loadLabCorpus(b) {
		b.Run(labBenchName(s.ID), func(b *testing.B) {
			e := newLabEngine()
			// Warm the process-wide ANTLR ATN for this statement's token paths,
			// and fail rather than benchmark a failing compilation.
			key := s.Text
			if stripped, _, ok := parser.StripLiterals(key); ok {
				key = stripped
			}
			if _, err := e.compilePlanCacheEntry(key); err != nil {
				b.Fatalf("%s (%s): %v", s.ID, s.Source, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := s.Text
				if stripped, hoisted, ok := parser.StripLiterals(k); ok {
					k, labSinkMap = stripped, hoisted
				}
				labSinkEntry, labSinkErr = e.compilePlanCacheEntry(k)
			}
		})
	}
}

// BenchmarkPrepareWarm measures a plan-cache HIT: key resolution plus the
// lookup, per corpus statement.
func BenchmarkPrepareWarm(b *testing.B) {
	for _, s := range loadLabCorpus(b) {
		b.Run(labBenchName(s.ID), func(b *testing.B) {
			e := newLabEngine()
			if _, _, err := e.parseAndAnalyse(s.Text); err != nil {
				b.Fatalf("%s (%s): %v", s.ID, s.Source, err)
			}
			if e.cache.Len() == 0 {
				b.Fatalf("%s: nothing published, the warm arm would be a cold arm", s.ID)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				labSinkEntry, labSinkMap, labSinkErr = e.parseAndAnalyse(s.Text)
			}
		})
	}
}

// BenchmarkStageSema measures the scope-analysis pass the engine runs on the
// parser's output, over an AST parsed once outside the timer.
func BenchmarkStageSema(b *testing.B) {
	for _, s := range loadLabCorpus(b) {
		b.Run(labBenchName(s.ID), func(b *testing.B) {
			var node ast.Query
			node, _, labSinkErr = parser.ParseStatement(s.Text)
			if labSinkErr != nil {
				b.Fatalf("%s (%s): %v", s.ID, s.Source, labSinkErr)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				labSinkSema = sema.MapToBolt(sema.Analyse(node))
				labSinkNames = sema.CollectParamNames(node)
			}
		})
	}
}

// TestPrepareLabCorpusCompiles is the laboratory's own gate: every corpus
// statement must compile to a plan-cache entry, otherwise BenchmarkPrepareCold
// would be measuring a failure.
func TestPrepareLabCorpusCompiles(t *testing.T) {
	e := newLabEngine()
	for _, s := range loadLabCorpus(t) {
		key := s.Text
		if stripped, _, ok := parser.StripLiterals(key); ok {
			key = stripped
		}
		if _, err := e.compilePlanCacheEntry(key); err != nil {
			t.Errorf("%s (%s) does not compile: %v", s.ID, s.Source, err)
		}
	}
}
