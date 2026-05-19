package ds

import (
	"math/rand/v2"
	"testing"
)

func TestUnionFind_Singletons(t *testing.T) {
	t.Parallel()
	u := New[int]()
	for i := 0; i < 10; i++ {
		u.MakeSet(i)
	}
	for i := 0; i < 10; i++ {
		for j := i + 1; j < 10; j++ {
			if u.Connected(i, j) {
				t.Fatalf("%d and %d should be in different sets", i, j)
			}
		}
	}
}

// TestUnionFindSlice_BasicUnion exercises the slice-backed variant
// against the same axioms as the map-backed [UnionFind].
func TestUnionFindSlice_BasicUnion(t *testing.T) {
	t.Parallel()
	u := NewSlice(8)
	if !u.Union(0, 1) {
		t.Fatal("Union(0,1) should return true on first merge")
	}
	if u.Union(0, 1) {
		t.Fatal("Union(0,1) should return false on a no-op merge")
	}
	if !u.Union(1, 2) {
		t.Fatal("Union(1,2) should return true on first merge")
	}
	if !u.Connected(0, 2) {
		t.Fatal("0 and 2 must be connected after a-b-c chain")
	}
	if u.Connected(0, 5) {
		t.Fatal("0 and 5 are in disjoint sets")
	}
	if u.Len() != 8 {
		t.Fatalf("Len = %d, want 8", u.Len())
	}
}

// TestUnionFindSlice_AgreesWithMap fuzzes the slice variant against
// the generic map variant on random unions over a bounded universe.
func TestUnionFindSlice_AgreesWithMap(t *testing.T) {
	t.Parallel()
	const n = 256
	const ops = 4 * n
	r := rand.New(rand.NewPCG(83, 89)) //nolint:gosec // deterministic
	ref := New[int]()
	for i := 0; i < n; i++ {
		ref.MakeSet(i)
	}
	got := NewSlice(n)
	for i := 0; i < ops; i++ {
		a := r.IntN(n)
		b := r.IntN(n)
		ref.Union(a, b)
		got.Union(a, b)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if ref.Connected(i, j) != got.Connected(i, j) {
				t.Fatalf("disagreement on (%d, %d): ref=%v got=%v", i, j,
					ref.Connected(i, j), got.Connected(i, j))
			}
		}
	}
}

// BenchmarkUnionFindSlice_1M measures the slice-backed variant on
// 1M Union operations, the Kruskal-MST hot path. Task #140 targets
// >5x improvement over the map-backed generic variant.
func BenchmarkUnionFindSlice_1M(b *testing.B) {
	const n = 1 << 20
	r := rand.New(rand.NewPCG(91, 97)) //nolint:gosec // deterministic
	pairs := make([][2]int, n)
	for i := 0; i < n; i++ {
		pairs[i] = [2]int{r.IntN(n), r.IntN(n)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u := NewSlice(n)
		for _, p := range pairs {
			u.Union(p[0], p[1])
		}
	}
}

// BenchmarkUnionFindGeneric_1M is the map-backed baseline used to
// establish the slice variant's expected speedup.
func BenchmarkUnionFindGeneric_1M(b *testing.B) {
	const n = 1 << 20
	r := rand.New(rand.NewPCG(91, 97)) //nolint:gosec // deterministic
	pairs := make([][2]int, n)
	for i := 0; i < n; i++ {
		pairs[i] = [2]int{r.IntN(n), r.IntN(n)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		u := New[int]()
		for j := 0; j < n; j++ {
			u.MakeSet(j)
		}
		for _, p := range pairs {
			u.Union(p[0], p[1])
		}
	}
}

func TestUnionFind_BasicUnion(t *testing.T) {
	t.Parallel()
	u := New[string]()
	u.Union("a", "b")
	u.Union("b", "c")
	if !u.Connected("a", "c") {
		t.Fatalf("a and c should be connected")
	}
	if u.Find("a") != u.Find("c") {
		t.Fatalf("Find drift after Union")
	}
	if u.Union("a", "c") {
		t.Fatalf("Union of already-merged sets must return false")
	}
}

func TestUnionFind_NaiveReference(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(13, 1)) //nolint:gosec // deterministic test RNG
	const n = 128
	u := New[int]()
	naive := make([]int, n)
	for i := range naive {
		naive[i] = i
	}
	for i := 0; i < 256; i++ {
		a := r.IntN(n)
		b := r.IntN(n)
		u.Union(a, b)
		ra := naive[a]
		rb := naive[b]
		if ra != rb {
			for k, v := range naive {
				if v == ra {
					naive[k] = rb
				}
			}
		}
	}
	for a := 0; a < n; a++ {
		for b := a + 1; b < n; b++ {
			want := naive[a] == naive[b]
			got := u.Connected(a, b)
			if want != got {
				t.Fatalf("Connected(%d,%d) = %v, want %v", a, b, got, want)
			}
		}
	}
}

func BenchmarkUnionFind_OneMillionOps(b *testing.B) {
	for n := 0; n < b.N; n++ {
		u := New[int]()
		r := rand.New(rand.NewPCG(uint64(n), 1)) //nolint:gosec // deterministic benchmark RNG
		const universe = 100_000
		for i := 0; i < 1_000_000; i++ {
			u.Union(r.IntN(universe), r.IntN(universe))
		}
	}
}
