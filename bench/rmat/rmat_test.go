package rmat

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
)

func TestGenerate_CountsMatchSpec(t *testing.T) {
	t.Parallel()
	loader := bulk.New(bulk.Options{Directed: true})
	spec := Spec{Scale: 6, EdgeFactor: 4, Seed: 17}
	n, m := Generate(spec, loader)
	if n != 64 || m != 256 {
		t.Fatalf("n=%d m=%d, want 64/256", n, m)
	}
	if loader.Rows() != int(m) {
		t.Fatalf("loader.Rows = %d, want %d", loader.Rows(), m)
	}
}

func TestGenerate_Deterministic(t *testing.T) {
	t.Parallel()
	la := bulk.New(bulk.Options{Directed: true})
	lb := bulk.New(bulk.Options{Directed: true})
	spec := Spec{Scale: 4, EdgeFactor: 8, Seed: 7}
	Generate(spec, la)
	Generate(spec, lb)
	if la.Rows() != lb.Rows() {
		t.Fatalf("same seed gave different row counts: %d vs %d", la.Rows(), lb.Rows())
	}
}

func TestGenerate_Defaults(t *testing.T) {
	t.Parallel()
	loader := bulk.New(bulk.Options{Directed: true})
	n, m := Generate(DefaultSpec(), loader)
	if n != 1024 || m != 16384 {
		t.Fatalf("defaults: n=%d m=%d, want 1024/16384", n, m)
	}
}
