package server

// F6 (#1838) measurement + wire-identity harness for the Bolt RECORD encode
// path.
//
// The engine hands each cell to the serving goroutine already boxed as an
// expr.Value (Result.ValueAt returns expr.Value; materialised rows live in a
// []expr.Value). That upstream box is intrinsic. What is NOT intrinsic is the
// SECOND box minted by exprValueToPackstream, which does `return int64(x)` /
// `return float64(x)` — re-boxing the scalar into a fresh packstream.Value
// (any) — plus the []packstream.Value→any box that proto.encodeRecord adds when
// it hands the whole row to WriteValue. A 60k-row x 3-scalar-column projection
// therefore mints ~240k short-lived boxes per page purely to shuffle each cell
// between interfaces before writeValue immediately unboxes it again.
//
// encodeRecordFast is the production fast path (session.go): for an all-scalar
// row it writes cells straight through the Encoder's typed writers, and for any
// other row it falls back to the exact proto.EncodeResponse encoding. The
// benchmarks below compare the current production path against the pre-#1838
// baseline (exprToPackstream + proto.EncodeResponse), and the identity test
// proves the two produce byte-for-byte identical wire output.
//
// Layer: short (no build tag). Benchmarks plus one identity test.

import (
	"bytes"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/bolt/proto"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// buildFastRow mirrors fillRowRawScalar's decision exactly (it shares the same
// isFastScalar predicate and exprToPackstream fallback) but reads from a value
// slice so it is usable without a *cypher.Result. It produces the []packstream.
// Value that the streaming path feeds to encodeRecordFast.
func buildFastRow(src []expr.Value, boltMajor uint8) []packstream.Value {
	dst := make([]packstream.Value, len(src))
	for _, v := range src {
		if !isFastScalar(v) {
			for i := range src {
				dst[i] = exprToPackstream(src[i], boltMajor)
			}
			return dst
		}
	}
	for i := range src {
		dst[i] = src[i]
	}
	return dst
}

// numericRows builds n rows of three non-tiny numeric columns, reproducing the
// F6 scenario. Values are deliberately outside the tiny-int static cache
// ([0,256)) so every int64 box is a real heap allocation, matching the
// "non-tiny int64/float64" wording of the finding.
func numericRows(n int) [][]expr.Value {
	rows := make([][]expr.Value, n)
	for i := range rows {
		rows[i] = []expr.Value{
			expr.IntegerValue(1_000_000 + int64(i)),
			expr.FloatValue(3.14159 * float64(i+1)),
			expr.IntegerValue(9_000_000_000 + int64(i)),
		}
	}
	return rows
}

// encodeBaselineRow reproduces the pre-#1838 production path: fill a reused
// []packstream.Value via exprToPackstream, then encode the Record through
// proto.EncodeResponse (which boxes the slice and re-dispatches each cell).
func encodeBaselineRow(enc *packstream.Encoder, buf *bytes.Buffer, rowBuf []packstream.Value, rec *proto.Record, raw []expr.Value, major uint8) {
	for i, cell := range raw {
		rowBuf[i] = exprToPackstream(cell, major)
	}
	rec.Data = rowBuf
	buf.Reset()
	enc.Reset(buf)
	if err := proto.EncodeResponse(enc, rec); err != nil {
		panic(err)
	}
	if err := enc.Flush(); err != nil {
		panic(err)
	}
}

// encodeFastRow reproduces the #1838 production path: raw-scalar fill (a plain
// interface copy, no re-box) followed by encodeRecordFast.
func encodeFastRow(enc *packstream.Encoder, buf *bytes.Buffer, rowBuf []packstream.Value, raw []expr.Value, major uint8) {
	// Mirror fillRowRawScalar's all-scalar branch (numericRows is all-scalar).
	for i := range raw {
		rowBuf[i] = raw[i]
	}
	buf.Reset()
	enc.Reset(buf)
	if err := encodeRecordFast(enc, rowBuf); err != nil {
		panic(err)
	}
	if err := enc.Flush(); err != nil {
		panic(err)
	}
}

// BenchmarkBoltRecordEncode measures the per-row fill+encode cost (3 numeric
// cells), isolating the per-cell boxing. baseline is the pre-#1838 path; fast is
// the current production path.
func BenchmarkBoltRecordEncode(b *testing.B) {
	const major = 5
	rows := numericRows(4096)

	b.Run("baseline", func(b *testing.B) {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		rowBuf := make([]packstream.Value, 3)
		var rec proto.Record
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			encodeBaselineRow(enc, &buf, rowBuf, &rec, rows[i%len(rows)], major)
		}
	})

	b.Run("fast", func(b *testing.B) {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		rowBuf := make([]packstream.Value, 3)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			encodeFastRow(enc, &buf, rowBuf, rows[i%len(rows)], major)
		}
	})
}

// BenchmarkBoltRecordEncodePage encodes a full 60k-row x 3-numeric-column page
// per op, directly quantifying the ~240k-box figure from the finding.
func BenchmarkBoltRecordEncodePage(b *testing.B) {
	const (
		major   = 5
		pageLen = 60_000
	)
	rows := numericRows(pageLen)

	b.Run("baseline", func(b *testing.B) {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		rowBuf := make([]packstream.Value, 3)
		var rec proto.Record
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, raw := range rows {
				encodeBaselineRow(enc, &buf, rowBuf, &rec, raw, major)
			}
		}
	})

	b.Run("fast", func(b *testing.B) {
		var buf bytes.Buffer
		enc := packstream.NewEncoder(&buf)
		rowBuf := make([]packstream.Value, 3)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, raw := range rows {
				encodeFastRow(enc, &buf, rowBuf, raw, major)
			}
		}
	})
}

// BenchmarkBoltRecordEncodeParallel models many concurrent streamers, each with
// its own encoder+buffer (as the real per-connection serve loop has). It
// surfaces the GC pressure the finding attributes to sustained concurrency.
func BenchmarkBoltRecordEncodeParallel(b *testing.B) {
	const major = 5
	rows := numericRows(4096)

	b.Run("baseline", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			var buf bytes.Buffer
			enc := packstream.NewEncoder(&buf)
			rowBuf := make([]packstream.Value, 3)
			var rec proto.Record
			i := 0
			for pb.Next() {
				encodeBaselineRow(enc, &buf, rowBuf, &rec, rows[i%len(rows)], major)
				i++
			}
		})
	})

	b.Run("fast", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			var buf bytes.Buffer
			enc := packstream.NewEncoder(&buf)
			rowBuf := make([]packstream.Value, 3)
			i := 0
			for pb.Next() {
				encodeFastRow(enc, &buf, rowBuf, rows[i%len(rows)], major)
				i++
			}
		})
	})
}

// TestRecordEncodeWireIdentity proves the #1838 fast path emits wire bytes
// byte-for-byte identical to the pre-#1838 baseline (exprToPackstream +
// proto.EncodeResponse) across representative scalar values, scalar edge cases,
// and rows that must take the composite/Null fallback path.
func TestRecordEncodeWireIdentity(t *testing.T) {
	const major = 5

	rows := map[string][]expr.Value{
		// Scalar rows (fast path).
		"empty":            {},
		"tiny_ints":        {expr.IntegerValue(0), expr.IntegerValue(127), expr.IntegerValue(-16)},
		"boundary_255_256": {expr.IntegerValue(255), expr.IntegerValue(256), expr.IntegerValue(-17)},
		"negative_ints":    {expr.IntegerValue(-1), expr.IntegerValue(-128), expr.IntegerValue(-129)},
		"int_widths":       {expr.IntegerValue(200), expr.IntegerValue(30000), expr.IntegerValue(2_000_000_000)},
		"int64_bounds":     {expr.IntegerValue(math.MinInt64), expr.IntegerValue(math.MaxInt64), expr.IntegerValue(math.MinInt32)},
		"floats":           {expr.FloatValue(0), expr.FloatValue(math.Copysign(0, -1)), expr.FloatValue(3.141592653589793)},
		"float_specials":   {expr.FloatValue(math.NaN()), expr.FloatValue(math.Inf(1)), expr.FloatValue(math.Inf(-1))},
		"strings":          {expr.StringValue(""), expr.StringValue("hello"), expr.StringValue("héllo — 世界")},
		"bools":            {expr.BoolValue(true), expr.BoolValue(false)},
		"mixed_scalar":     {expr.IntegerValue(-42), expr.FloatValue(2.5), expr.StringValue("x"), expr.BoolValue(true)},
		"long_string":      {expr.StringValue(string(make([]byte, 300)))},

		// Rows forcing the baseline fallback (a non-fast-scalar cell present).
		// These are limited to deterministically-ordered composites (lists are
		// ordered; a temporal Struct has ordered fields; Null is a single byte).
		// Map-bearing values (Node/Relationship/Map) are intentionally excluded:
		// PackStream maps are unordered and Go randomises map iteration per
		// range, so both the baseline and the fast path (whose fallback branch is
		// the identical WriteValue call, see encodeRecordFast) emit map keys in a
		// run-varying order — comparing two independent conversions would flake on
		// order, not correctness. Map/Node RECORD encoding through this same
		// choke point is covered by the Bolt e2e and openCypher TCK driver
		// round-trips, which decode order-insensitively.
		"null_only":     {expr.Null},
		"null_mixed":    {expr.IntegerValue(1), expr.Null, expr.StringValue("y")},
		"list_value":    {expr.ListValue{expr.IntegerValue(1), expr.IntegerValue(2)}},
		"nested_list":   {expr.ListValue{expr.ListValue{expr.StringValue("a")}, expr.IntegerValue(-9)}},
		"temporal_date": {expr.NewDate(2026, 7, 17)},
		"list_and_int":  {expr.ListValue{expr.IntegerValue(1)}, expr.IntegerValue(2_000_000)},
	}

	for name, src := range rows {
		t.Run(name, func(t *testing.T) {
			// Baseline: exprToPackstream every cell, encode via EncodeResponse.
			baseRow := make([]packstream.Value, len(src))
			for i, cell := range src {
				baseRow[i] = exprToPackstream(cell, major)
			}
			var baseBuf bytes.Buffer
			baseEnc := packstream.NewEncoder(&baseBuf)
			if err := proto.EncodeResponse(baseEnc, &proto.Record{Data: baseRow}); err != nil {
				t.Fatalf("baseline encode: %v", err)
			}
			if err := baseEnc.Flush(); err != nil {
				t.Fatalf("baseline flush: %v", err)
			}

			// Fast path: raw-or-convert fill (buildFastRow) then encodeRecordFast.
			fastRow := buildFastRow(src, major)
			var fastBuf bytes.Buffer
			fastEnc := packstream.NewEncoder(&fastBuf)
			if err := encodeRecordFast(fastEnc, fastRow); err != nil {
				t.Fatalf("fast encode: %v", err)
			}
			if err := fastEnc.Flush(); err != nil {
				t.Fatalf("fast flush: %v", err)
			}

			if !bytes.Equal(baseBuf.Bytes(), fastBuf.Bytes()) {
				t.Fatalf("wire bytes differ\n baseline: % x\n fast:     % x", baseBuf.Bytes(), fastBuf.Bytes())
			}
		})
	}
}
