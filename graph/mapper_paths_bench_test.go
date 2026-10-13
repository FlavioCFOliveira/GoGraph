package graph

// mapper_paths_bench_test.go — the Mapper's lookup, intern and NodeID→key paths,
// measured for rmp #3121 (the reverse table's storage layout). Each benchmark
// builds its mapper outside the timed region.
//
// Layer: short (benchmarks run only with -bench).

import (
	"strconv"
	"testing"
)

const benchMapperKeys = 1 << 17

func benchMapperKeysOf(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = "__cx_" + strconv.FormatUint(uint64(i)+1, 16)
	}
	return keys
}

func benchMapperFilled(n int) (*Mapper[string], []string, []NodeID) {
	m := NewMapper[string]()
	keys := benchMapperKeysOf(n)
	ids := make([]NodeID, n)
	for i, k := range keys {
		ids[i] = m.Intern(k)
	}
	return m, keys, ids
}

func BenchmarkMapperPaths_Lookup(b *testing.B) {
	m, keys, _ := benchMapperFilled(benchMapperKeys)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := m.Lookup(keys[i&(benchMapperKeys-1)]); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkMapperPaths_InternExisting(b *testing.B) {
	m, keys, _ := benchMapperFilled(benchMapperKeys)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Intern(keys[i&(benchMapperKeys-1)])
	}
}

func BenchmarkMapperPaths_InternNew(b *testing.B) {
	keys := benchMapperKeysOf(benchMapperKeys)
	b.ReportAllocs()
	b.ResetTimer()
	var m *Mapper[string]
	for i := 0; i < b.N; i++ {
		j := i & (benchMapperKeys - 1)
		if j == 0 {
			b.StopTimer()
			m = NewMapper[string]()
			b.StartTimer()
		}
		_ = m.Intern(keys[j])
	}
}

func BenchmarkMapperPaths_Resolve(b *testing.B) {
	m, _, ids := benchMapperFilled(benchMapperKeys)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A stride co-prime with the table walks the ids out of order.
		if _, ok := m.Resolve(ids[(i*7919)&(benchMapperKeys-1)]); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkMapperPaths_ResolveParallel(b *testing.B) {
	m, _, ids := benchMapperFilled(benchMapperKeys)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, ok := m.Resolve(ids[(i*7919)&(benchMapperKeys-1)]); !ok {
				b.Fatal("miss")
			}
			i++
		}
	})
}

func BenchmarkMapperPaths_Walk(b *testing.B) {
	m, _, _ := benchMapperFilled(benchMapperKeys)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		m.Walk(func(NodeID, string) bool { n++; return true })
		if n != benchMapperKeys {
			b.Fatal("short walk")
		}
	}
}

func BenchmarkMapperPaths_AppendIDs(b *testing.B) {
	m, _, _ := benchMapperFilled(benchMapperKeys)
	buf := make([]NodeID, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cur MapperCursor
		n := 0
		for !cur.Done() {
			buf, cur = m.AppendIDs(buf[:0], cur, 1024)
			n += len(buf)
		}
		if n != benchMapperKeys {
			b.Fatal("short scan")
		}
	}
}

// benchMapperRefilled interns benchMapperKeys keys, releases every one of them,
// and interns as many new keys: the shape a large rolled-back creation followed
// by new work leaves (rmp #3121). It returns the new keys' ids.
func benchMapperRefilled() (*Mapper[string], []NodeID) {
	m, keys, ids := benchMapperFilled(benchMapperKeys)
	for i, k := range keys {
		m.Unplace(k, ids[i])
	}
	fresh := make([]NodeID, benchMapperKeys)
	for i := range fresh {
		fresh[i] = m.Intern("__cx_new_" + strconv.Itoa(i))
	}
	return m, fresh
}

func BenchmarkMapperPaths_ResolveAfterRelease(b *testing.B) {
	m, ids := benchMapperRefilled()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := m.Resolve(ids[(i*7919)&(benchMapperKeys-1)]); !ok {
			b.Fatal("miss")
		}
	}
}

func BenchmarkMapperPaths_WalkAfterRelease(b *testing.B) {
	m, _ := benchMapperRefilled()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		m.Walk(func(NodeID, string) bool { n++; return true })
		if n != benchMapperKeys {
			b.Fatal("short walk")
		}
	}
}
