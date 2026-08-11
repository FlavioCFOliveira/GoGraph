package lpg

import (
	"encoding/binary"
	"math"
	"runtime"
	"runtime/debug"
	"testing"
)

// propbag_encoding_spike_test.go — what a byte-stream property bag would cost
// (rmp #2408 groundwork, sprint 339).
//
// The memory audit of 2026-08-11 measured GoGraph spending 146.66 B on one
// 9-character node property against Memgraph's 19.45 B, a 7.54x gap and the
// worst ratio in the audit. A heap profile attributes GoGraph's figure to the
// nodePropShards map entry (101.06 B/node), propBag's pairs slice (33.03) and
// the interface box inside PropertyValue (18.35) — with 17.30 B being the
// string the caller actually asked to store.
//
// Memgraph stores a vertex's properties as ONE contiguous byte buffer of
// self-describing records, read at src/storage/v2/property_store.cpp:99-205 of
// v3.9.0:
//
//	metadata byte: 0bTTTT_IIPP  — 4-bit type, 2-bit id width, 2-bit payload width
//	then the property id in 1/2/4/8 bytes, narrowest that fits
//	then the payload, whose shape depends on the type
//	BOOL carries its value in the payload-width field and occupies NO payload
//
// So {sid:"p00000001", name:"person-1", age:25} encodes to 26 bytes there.
//
// This is a SPIKE, not an implementation: it measures what the same encoding
// would cost in Go for the shapes the audit measured, so the decision to adopt
// it rests on a number taken in this project rather than on Memgraph's own
// claim of "approximately 10 times less memory". It deliberately implements
// only the four scalar kinds the audit's shapes use; String, Int64, Float64 and
// Bool. Time, Bytes and List would keep a boxed side path.
//
// Run it explicitly — it is a benchmark so it stays out of `make ci`:
//
//	go test -run '^$' -bench BenchmarkPropBagRepresentation -benchtime 1x ./graph/lpg/

// spikeEncode appends one Memgraph-shaped record for (key, val) to buf.
// Reports false for a kind this spike does not model.
func spikeEncode(buf []byte, key PropertyKeyID, val PropertyValue) ([]byte, bool) {
	// width returns the narrowest of 1/2/4/8 bytes that holds v, and its
	// 2-bit selector, exactly as Memgraph's Size enum does.
	width := func(v uint64) (n int, sel byte) {
		switch {
		case v <= math.MaxUint8:
			return 1, 0
		case v <= math.MaxUint16:
			return 2, 1
		case v <= math.MaxUint32:
			return 4, 2
		default:
			return 8, 3
		}
	}
	put := func(b []byte, v uint64, n int) []byte {
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], v)
		return append(b, tmp[:n]...)
	}

	idN, idSel := width(uint64(key))

	switch val.Kind() {
	case PropBool:
		bv, _ := val.Bool()
		// The value rides in the payload-width field: no payload bytes.
		pay := byte(0)
		if bv {
			pay = 3
		}
		buf = append(buf, byte(PropBool)<<4|idSel<<2|pay)
		return put(buf, uint64(key), idN), true

	case PropInt64:
		iv, _ := val.Int64()
		// Zig-zag so that small negatives stay narrow, which Memgraph gets
		// from storing signed types directly at each width.
		zz := uint64(iv<<1) ^ uint64(iv>>63)
		vN, vSel := width(zz)
		buf = append(buf, byte(PropInt64)<<4|idSel<<2|vSel)
		buf = put(buf, uint64(key), idN)
		return put(buf, zz, vN), true

	case PropFloat64:
		fv, _ := val.Float64()
		buf = append(buf, byte(PropFloat64)<<4|idSel<<2)
		buf = put(buf, uint64(key), idN)
		return put(buf, math.Float64bits(fv), 8), true

	case PropString:
		sv, _ := val.String()
		lN, lSel := width(uint64(len(sv)))
		buf = append(buf, byte(PropString)<<4|idSel<<2|lSel)
		buf = put(buf, uint64(key), idN)
		buf = put(buf, uint64(len(sv)), lN)
		return append(buf, sv...), true
	}
	return buf, false
}

func spikeLiveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// BenchmarkPropBagRepresentation reports bytes of LIVE HEAP per node for the
// current propBag and for the byte-stream encoding, holding the same
// properties. It is a memory measurement wearing a benchmark's clothes: run it
// with -benchtime 1x, and read the reported b_per_node metric, not ns/op.
func BenchmarkPropBagRepresentation(b *testing.B) {
	const n = 200_000

	shapes := []struct {
		name string
		set  func(i int) []struct {
			k PropertyKeyID
			v PropertyValue
		}
	}{
		{"1prop_string9", func(i int) []struct {
			k PropertyKeyID
			v PropertyValue
		} {
			return []struct {
				k PropertyKeyID
				v PropertyValue
			}{{1, StringValue(spikeSid(i))}}
		}},
		{"3prop_str_str_int", func(i int) []struct {
			k PropertyKeyID
			v PropertyValue
		} {
			return []struct {
				k PropertyKeyID
				v PropertyValue
			}{
				{1, StringValue(spikeSid(i))},
				{2, StringValue(spikeName(i))},
				{3, Int64Value(int64(18 + i%60))},
			}
		}},
	}

	for _, sh := range shapes {
		b.Run(sh.name+"/propBag", func(b *testing.B) {
			for range b.N {
				before := spikeLiveHeap()
				bags := make([]propBag, n)
				for i := range n {
					for _, p := range sh.set(i) {
						bags[i].set(p.k, p.v)
					}
				}
				after := spikeLiveHeap()
				b.ReportMetric(float64(after-before)/float64(n), "B_per_node")
				runtime.KeepAlive(bags)
			}
		})
		b.Run(sh.name+"/byteStream", func(b *testing.B) {
			for range b.N {
				before := spikeLiveHeap()
				bufs := make([][]byte, n)
				for i := range n {
					var buf []byte
					for _, p := range sh.set(i) {
						var ok bool
						buf, ok = spikeEncode(buf, p.k, p.v)
						if !ok {
							b.Fatalf("spikeEncode refused kind %v", p.v.Kind())
						}
					}
					// Trim to exactly what was written: append's growth
					// doubles, and the real implementation would size the
					// buffer once, so the slack is not part of the design.
					exact := make([]byte, len(buf))
					copy(exact, buf)
					bufs[i] = exact
				}
				after := spikeLiveHeap()
				b.ReportMetric(float64(after-before)/float64(n), "B_per_node")
				runtime.KeepAlive(bufs)
			}
		})
	}
}

func spikeSid(i int) string  { return "p" + spikePad(i, 8) }
func spikeName(i int) string { return "person-" + spikePad(i, 1) }

func spikePad(i, w int) string {
	d := []byte{}
	for v := i; v > 0; v /= 10 {
		d = append([]byte{byte('0' + v%10)}, d...)
	}
	if len(d) == 0 {
		d = []byte{'0'}
	}
	for len(d) < w {
		d = append([]byte{'0'}, d...)
	}
	return string(d)
}
