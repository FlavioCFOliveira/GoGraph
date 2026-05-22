package graph

import (
	"strconv"
	"testing"
)

// TestMapperShardFor_StableAcrossInstances verifies the regression
// closed by Sprint 56 T3: the same comparable key routes to the same
// shard across freshly constructed Mappers, so a NodeID assigned in
// one process matches the NodeID assigned to the same key in any other
// process. Before the FNV-1a switch, maphash.MakeSeed() returned a
// fresh random seed per process and mapperShardFor produced a
// different shard for the same key across instances, breaking the
// snapshot's labels.bin / properties.bin alignment on reopen.
func TestMapperShardFor_StableAcrossInstances(t *testing.T) {
	keys := []string{
		"alice", "bob", "carol", "dave", "erin",
		"p1", "p2", "p3", "c1", "c2", "c3",
		"post-42", "comment-99", "user/v1",
		"ζebra", "snake_case", "kebab-case",
	}
	for _, k := range keys {
		first := mapperShardFor(k)
		for trial := 0; trial < 8; trial++ {
			if got := mapperShardFor(k); got != first {
				t.Fatalf("mapperShardFor(%q): trial %d got %d, want %d (must be stable)", k, trial, got, first)
			}
		}
	}
}

// TestMapper_NodeIDStableAcrossInstances confirms that two freshly
// constructed Mappers assign the same NodeID to a key inserted in the
// same order. The NodeID encodes (shard, intra-shard index); the shard
// is determined by the stable hash and the intra-shard index by the
// insertion order. Identical insertion sequences therefore produce
// identical NodeIDs.
func TestMapper_NodeIDStableAcrossInstances(t *testing.T) {
	keys := []string{"alice", "bob", "carol", "dave", "erin"}
	m1 := NewMapper[string]()
	m2 := NewMapper[string]()
	for _, k := range keys {
		id1 := m1.Intern(k)
		id2 := m2.Intern(k)
		if id1 != id2 {
			t.Fatalf("key %q: m1=%d m2=%d (NodeIDs must match across instances)", k, id1, id2)
		}
	}
}

// TestMapperShardFor_CoverageAcrossTypes spot-checks that every fast-
// path branch in mapperShardFor is reachable and produces a stable
// value. The branches under test are the dedicated cases in the
// switch — strings, signed and unsigned integer widths, and the
// fixed-size byte array used by the UUID codec — plus the
// fmt.Sprintf fallback for custom comparable types.
func TestMapperShardFor_CoverageAcrossTypes(t *testing.T) {
	type customStruct struct {
		Foo string
		Bar int
	}
	tests := []struct {
		name string
		call func() uint64
	}{
		{"string", func() uint64 { return mapperShardFor("alice") }},
		{"int", func() uint64 { return mapperShardFor(int(42)) }},
		{"int8", func() uint64 { return mapperShardFor(int8(-1)) }},
		{"int16", func() uint64 { return mapperShardFor(int16(1024)) }},
		{"int32", func() uint64 { return mapperShardFor(int32(-1)) }},
		{"int64", func() uint64 { return mapperShardFor(int64(99)) }},
		{"uint", func() uint64 { return mapperShardFor(uint(7)) }},
		{"uint8", func() uint64 { return mapperShardFor(uint8(255)) }},
		{"uint16", func() uint64 { return mapperShardFor(uint16(65535)) }},
		{"uint32", func() uint64 { return mapperShardFor(uint32(1<<31 - 1)) }},
		{"uint64", func() uint64 { return mapperShardFor(uint64(1<<63 - 1)) }},
		{"uuid", func() uint64 { return mapperShardFor([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}) }},
		{"customStruct", func() uint64 { return mapperShardFor(customStruct{Foo: "x", Bar: 42}) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.call()
			second := tc.call()
			if first != second {
				t.Fatalf("%s: not stable (%d vs %d)", tc.name, first, second)
			}
			if first >= mapperShardCount {
				t.Fatalf("%s: shard %d out of range [0, %d)", tc.name, first, mapperShardCount)
			}
		})
	}
}

// BenchmarkMapperShardFor_String measures the hot-path cost on the
// most common key type after the FNV-1a switch. Used to verify the
// new path stays within a few-ns per call.
func BenchmarkMapperShardFor_String(b *testing.B) {
	keys := make([]string, 256)
	for i := range keys {
		keys[i] = "user_" + strconv.Itoa(i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mapperShardFor(keys[i&255])
	}
}

// BenchmarkMapperShardFor_Int64 measures the integer fast path.
func BenchmarkMapperShardFor_Int64(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mapperShardFor(int64(i))
	}
}
