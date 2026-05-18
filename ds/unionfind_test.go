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
