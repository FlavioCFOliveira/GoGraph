package cypher

// rowctx_pool_bench_test.go — evidence for #2188 (RowContext Tier A): the pooled
// per-row binding map must be sized to the query's actual schema width.
//
// acquireRowCtx pools a recycled expr.RowContext and clear()s it per row. The pool
// handed out a map pre-sized to rowCtxPoolMaxSchema (16) regardless of how many
// variables the query actually binds, and Go's map clear is O(capacity), not O(len) —
// so a one-variable query paid to clear a 16-entry map on every row.
//
// The round-3 audit isolated and attributed that cost
// (docs/audit-2026-07-26-streams/s05-runtime.md F3):
//
//	map cap 16 + clear   67.2 ns/op   <- what the pool did
//	map cap 16, no clear 39.5 ns/op
//	map cap  1 + clear   46.0 ns/op
//	map cap  1, no clear 44.6 ns/op
//
// i.e. ~28 ns/row was the clear() of an over-sized map, and ~39 ns is the map
// machinery itself, which only positional binding removes (task #2210).
//
// These benchmarks measure the real acquire → populate → release cycle at each schema
// width, so the fix is measured on the actual code path rather than on a model of it.
//
//	go test -run=^$ -bench='BenchmarkRowCtxPool' -benchmem -count=8 ./cypher/
//
// Layer: short.

import (
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// rowCtxBenchSchema builds a schema binding width variables to columns 0..width-1, and
// a matching row of boxed integers.
func rowCtxBenchSchema(width int) (map[string]int, exec.Row) {
	schema := make(map[string]int, width)
	row := make(exec.Row, width)
	for i := 0; i < width; i++ {
		schema["v"+strconv.Itoa(i)] = i
		// Start above 255 so the box is a real heap value, not a shared small-int.
		row[i] = expr.IntegerValue(int64(1000 + i))
	}
	return schema, row
}

// benchRowCtxCycle measures one acquire → populate → release cycle at the given schema
// width — exactly what one row of a filtered scan pays for its variable bindings.
//
// It populates through the same loop populateRowCtx runs for a plain scalar column (no
// path/VLE/edge metadata and no graph), which is the common case and the one the audit
// measured; the point of the benchmark is the map's acquire/clear/write cost, and that
// is independent of which branch fills each entry.
func benchRowCtxCycle(b *testing.B, width int) {
	schema, row := rowCtxBenchSchema(width)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := acquireRowCtx(len(schema))
		for name, col := range schema {
			p.ctx[name] = row[col]
		}
		// Read every binding back, as an expression over the row would.
		for name := range schema {
			if _, ok := p.ctx[name]; !ok {
				b.Fatalf("binding %q missing", name)
			}
		}
		releaseRowCtx(p)
	}
}

func BenchmarkRowCtxPool_Width1(b *testing.B)  { benchRowCtxCycle(b, 1) }
func BenchmarkRowCtxPool_Width2(b *testing.B)  { benchRowCtxCycle(b, 2) }
func BenchmarkRowCtxPool_Width4(b *testing.B)  { benchRowCtxCycle(b, 4) }
func BenchmarkRowCtxPool_Width8(b *testing.B)  { benchRowCtxCycle(b, 8) }
func BenchmarkRowCtxPool_Width16(b *testing.B) { benchRowCtxCycle(b, 16) }

// BenchmarkRowCtxPool_Width32 is above rowCtxPoolMaxSchema, so it bypasses the pool
// entirely and must be unaffected by the class change.
func BenchmarkRowCtxPool_Width32(b *testing.B) { benchRowCtxCycle(b, 32) }
