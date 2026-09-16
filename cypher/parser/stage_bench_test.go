package parser

// stage_bench_test.go — the Cypher front end measured STAGE BY STAGE over the
// examples statement corpus (rmp #2843, sprint 362 parsing campaign).
//
// # What each benchmark measures
//
// [ParseStatement] runs, in this order:
//
//	guardInput + validateUnicodeEscapes   → "Guard"
//	rewriteShortestPath + applyNormalizers → "Normalize"
//	InputStream + Lexer + TokenStream.Fill + collectErrCharErrors → "Lex"
//	CypherParser.Script                    → the Parse delta
//	newVisitor + visit                     → the Visit delta
//
// [StripLiterals] is measured too ("Strip"). It is not part of ParseStatement:
// cypher/api.go calls it first, to resolve the plan-cache key, so it is the
// first thing a statement pays for on the way to the parser.
//
// The first three stages take a string and return a value, so each is measured
// in isolation and its number is that stage's whole cost.
//
// Lex, parse and visit cannot be measured in isolation, because each consumes
// an object the previous stage built and neither a filled token stream nor a
// parse tree can be re-consumed. Measuring them from a reused object would
// charge zero allocation for the tokens and the tree, which is most of what
// they cost. They are therefore measured as CUMULATIVE PREFIXES — Lex,
// LexParse, LexParseVisit — and the per-stage cost is the difference between
// consecutive prefixes. Every prefix does the real work from scratch on every
// iteration, so no number is borrowed.
//
// "Full" is [ParseStatement] itself: the control the prefixes must add up to.
//
// # Input to each stage
//
// Lex, LexParse and LexParseVisit are fed the NORMALIZED text, because that is
// what the real pipeline hands the lexer. Normalization is done once, outside
// the timer.
//
// # ATN warm-up
//
// The generated ANTLR lexer and parser share one ATN per grammar for the whole
// process, and its DFA is filled lazily the first time a token path is seen.
// A first parse is therefore far more expensive than a steady-state one, and
// it is a PROCESS-level cost, not a per-statement one. Every benchmark here
// warms the statement through the full pipeline before resetting the timer, so
// what is reported is steady state. The first-parse cost is a separate
// question this laboratory does not answer.
//
// # Running it
//
//	scripts/parse-lab.sh
//
// which is the reproducible form of:
//
//	go test -run=^$ -bench='^BenchmarkFrontEndStage$' -benchmem \
//	    -benchtime=100ms -count=8 ./cypher/parser/

import (
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen"
)

// Sinks. Assigned from every benchmark body so the compiler cannot delete the
// work being measured.
var (
	sinkErr    error
	sinkString string
	sinkBool   bool
	sinkAny    any
	sinkMap    map[string]string
	sinkTokens *antlr.CommonTokenStream
	sinkTree   gen.IScriptContext
)

// lexStage reproduces the lexing block of [ParseStatement] verbatim: an input
// stream, a lexer with the package's own error listener, a token stream filled
// eagerly, and the ERRCHAR promotion pass over the filled tokens.
func lexStage(q string) *antlr.CommonTokenStream {
	lexErrListener := &errorListener{}
	input := antlr.NewInputStream(q)
	lexer := gen.NewCypherLexer(input)
	lexer.RemoveErrorListeners()
	lexer.AddErrorListener(lexErrListener)
	stream := antlr.NewCommonTokenStream(lexer, antlr.TokenDefaultChannel)
	stream.Fill()
	collectErrCharErrors(lexErrListener, stream)
	return stream
}

// parseStage reproduces the parsing block of [ParseStatement] over an already
// filled token stream.
func parseStage(stream *antlr.CommonTokenStream) gen.IScriptContext {
	parseErrListener := &errorListener{}
	p := gen.NewCypherParser(stream)
	p.RemoveErrorListeners()
	p.AddErrorListener(parseErrListener)
	p.BuildParseTrees = true
	tree, _ := recoverParseScript(p)
	return tree
}

// normalizeStage reproduces the rewriting block of [ParseStatement].
func normalizeStage(q string) string {
	q, _ = rewriteShortestPath(q)
	return applyNormalizers(q)
}

// BenchmarkFrontEndStage reports ns/op, B/op and allocs/op for every front-end
// stage on every statement of the examples corpus.
func BenchmarkFrontEndStage(b *testing.B) {
	corpus := loadCorpus(b)

	stages := []struct {
		name string
		// run receives the raw statement text and its normalized form.
		run func(raw, norm string)
	}{
		{"Strip", func(raw, _ string) {
			sinkString, sinkMap, sinkBool = StripLiterals(raw)
		}},
		{"Guard", func(raw, _ string) {
			sinkErr = guardInput(raw)
			sinkErr = validateUnicodeEscapes(raw)
		}},
		{"Normalize", func(raw, _ string) {
			sinkString = normalizeStage(raw)
		}},
		{"Lex", func(_, norm string) {
			sinkTokens = lexStage(norm)
		}},
		{"LexParse", func(_, norm string) {
			sinkTree = parseStage(lexStage(norm))
		}},
		{"LexParseVisit", func(_, norm string) {
			sinkAny = newVisitor().visit(parseStage(lexStage(norm)))
		}},
		{"Full", func(raw, _ string) {
			var q any
			q, _, sinkErr = ParseStatement(raw)
			sinkAny = q
		}},
	}

	for _, st := range stages {
		b.Run(st.name, func(b *testing.B) {
			for _, s := range corpus {
				norm := normalizeStage(s.Text)
				b.Run(benchName(s.ID), func(b *testing.B) {
					// Warm the shared ATN for this statement's token paths, and
					// fail rather than benchmark an error path.
					if _, err := Parse(s.Text); err != nil {
						b.Fatalf("%s (%s): %v", s.ID, s.Source, err)
					}
					st.run(s.Text, norm)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						st.run(s.Text, norm)
					}
				})
			}
		})
	}
}
