package graph

import (
	"strconv"
	"testing"
)

// TestShardFor_MatchesFreeFunction verifies that the per-instance shardFor
// method selects the identical shard as the free mapperShardFor function for
// every supported key kind. They must agree byte-for-byte: the persistence /
// restore path and the cross-process stability guarantee both assume a single
// canonical shard placement, so the zero-alloc unsafe dispatch must not
// diverge from the reference type-switch implementation.
func TestShardFor_MatchesFreeFunction(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		m := NewMapper[string]()
		for i := 0; i < 2048; i++ {
			k := "user_" + strconv.Itoa(i)
			if got, want := m.shardFor(k), mapperShardFor(k); got != want {
				t.Fatalf("string key %q: shardFor=%d mapperShardFor=%d", k, got, want)
			}
		}
		// Empty string and unicode keys.
		for _, k := range []string{"", "ünïcødé", "💡graph"} {
			if got, want := m.shardFor(k), mapperShardFor(k); got != want {
				t.Fatalf("string key %q: shardFor=%d mapperShardFor=%d", k, got, want)
			}
		}
	})

	t.Run("int64", func(t *testing.T) {
		m := NewMapper[int64]()
		for _, k := range []int64{0, 1, -1, 255, 256, -256, 1 << 40, -(1 << 40), 1<<63 - 1, -(1 << 62)} {
			if got, want := m.shardFor(k), mapperShardFor(k); got != want {
				t.Fatalf("int64 key %d: shardFor=%d mapperShardFor=%d", k, got, want)
			}
		}
	})

	t.Run("int", func(t *testing.T) {
		m := NewMapper[int]()
		for _, k := range []int{0, 1, -1, 255, 256, -256, 1 << 40, -(1 << 40)} {
			if got, want := m.shardFor(k), mapperShardFor(k); got != want {
				t.Fatalf("int key %d: shardFor=%d mapperShardFor=%d", k, got, want)
			}
		}
	})

	t.Run("uint64", func(t *testing.T) {
		m := NewMapper[uint64]()
		for _, k := range []uint64{0, 1, 255, 256, 1 << 40, 1<<64 - 1} {
			if got, want := m.shardFor(k), mapperShardFor(k); got != want {
				t.Fatalf("uint64 key %d: shardFor=%d mapperShardFor=%d", k, got, want)
			}
		}
	})

	t.Run("uint32", func(t *testing.T) {
		m := NewMapper[uint32]()
		for _, k := range []uint32{0, 1, 255, 256, 1 << 20, 1<<32 - 1} {
			if got, want := m.shardFor(k), mapperShardFor(k); got != want {
				t.Fatalf("uint32 key %d: shardFor=%d mapperShardFor=%d", k, got, want)
			}
		}
	})

	t.Run("int8", func(t *testing.T) {
		m := NewMapper[int8]()
		for k := -128; k <= 127; k++ {
			kk := int8(k)
			if got, want := m.shardFor(kk), mapperShardFor(kk); got != want {
				t.Fatalf("int8 key %d: shardFor=%d mapperShardFor=%d", kk, got, want)
			}
		}
	})

	t.Run("16bytes", func(t *testing.T) {
		m := NewMapper[[16]byte]()
		var k [16]byte
		for i := range k {
			k[i] = byte(i * 7)
		}
		if got, want := m.shardFor(k), mapperShardFor(k); got != want {
			t.Fatalf("[16]byte key: shardFor=%d mapperShardFor=%d", got, want)
		}
	})
}

// BenchmarkShardForMethod_String confirms the zero-allocation guarantee of the
// per-instance shardFor dispatch for string keys — the property the entire
// string-keyed Cypher/LPG read path depends on. A regression (boxing the key)
// shows up immediately as allocs/op > 0.
func BenchmarkShardForMethod_String(b *testing.B) {
	m := NewMapper[string]()
	keys := make([]string, 256)
	for i := range keys {
		keys[i] = "user_" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var sink uint64
	for i := 0; i < b.N; i++ {
		sink ^= m.shardFor(keys[i&255])
	}
	if sink == 0xdeadbeef {
		b.Fatal("unreachable")
	}
}
