package server

// columnar_wire_identity_test.go — Bolt RECORD wire byte-identity guard for the
// late-materialisation columnar scalar-property projection (#1704 P2, #1823).
//
// The columnar read path boxes projected scalars only at the API boundary
// ([cypher.Result.ValueAt]). This test drives the ACTUAL Bolt RECORD wire encoder
// (fillRowRawScalar → encodeRecordFast, the #1838 streaming path) over two
// value-equivalent queries — one that takes the columnar path, one forced onto
// the row-at-a-time path — and asserts the emitted wire bytes are byte-for-byte
// identical for every row. It complements the API-level Record/ValueAt identity
// test in package cypher by proving the guarantee end-to-end at the wire.

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// encodeAllRecords drains res and returns the concatenated Bolt RECORD wire bytes
// produced by the streaming encoder for every row, using the exact
// fillRowRawScalar → encodeRecordFast path a live PULL uses.
func encodeAllRecords(t *testing.T, res *cypher.Result, boltMajor uint8) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := packstream.NewEncoder(&buf)
	width := len(res.Columns())
	dst := make([]packstream.Value, width)
	for res.Next() {
		fillRowRawScalar(dst, res, boltMajor)
		if err := encodeRecordFast(enc, dst); err != nil {
			t.Fatalf("encodeRecordFast: %v", err)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Result.Err: %v", err)
	}
	return buf.Bytes()
}

func TestColumnarProjection_BoltWireByteIdentity(t *testing.T) {
	build := func() *lpg.Graph[string, float64] {
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		add := func(key string, v lpg.PropertyValue, set bool) {
			if err := g.AddNode(key); err != nil {
				t.Fatalf("AddNode(%s): %v", key, err)
			}
			if set {
				if err := g.SetNodeProperty(key, "val", v); err != nil {
					t.Fatalf("SetNodeProperty(%s): %v", key, err)
				}
			}
		}
		add("a", lpg.Int64Value(256), true)
		add("b", lpg.Int64Value(math.MinInt64), true)
		add("c", lpg.Float64Value(math.NaN()), true)
		add("d", lpg.Float64Value(math.Inf(-1)), true)
		add("e", lpg.BoolValue(true), true)
		add("f", lpg.StringValue("héllo 😀"), true)
		add("g", lpg.PropertyValue{}, false) // absent → NULL
		add("h", lpg.DateValue(time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)), true)
		return g
	}

	for _, boltMajor := range []uint8{4, 5} {
		eng := cypher.NewEngine(build())
		colRes, err := eng.Run(context.Background(), "MATCH (n) RETURN n.val AS v", nil)
		if err != nil {
			t.Fatalf("columnar Run: %v", err)
		}
		colBytes := encodeAllRecords(t, colRes, boltMajor)
		_ = colRes.Close()

		engRow := cypher.NewEngine(build())
		rowRes, err := engRow.Run(context.Background(), "MATCH (n) RETURN coalesce(n.val) AS v", nil)
		if err != nil {
			t.Fatalf("row Run: %v", err)
		}
		rowBytes := encodeAllRecords(t, rowRes, boltMajor)
		_ = rowRes.Close()

		if !bytes.Equal(colBytes, rowBytes) {
			t.Fatalf("Bolt v%d RECORD wire bytes differ:\n columnar=% x\n row-path=% x", boltMajor, colBytes, rowBytes)
		}
		if len(colBytes) == 0 {
			t.Fatalf("Bolt v%d: no RECORD bytes produced", boltMajor)
		}
	}
}
