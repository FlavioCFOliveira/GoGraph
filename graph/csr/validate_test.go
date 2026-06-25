package csr

// validate_test.go — regression gate for the 2026-06-25 reliability audit
// finding #1762: FromArrays accepts caller-supplied arrays and a malformed
// snapshot used to panic with an opaque "index out of range" deep inside
// BuildReverse/LiveMask/IsSymmetric. CSR.Validate now gives untrusting callers
// a typed ErrMalformedCSR at the boundary. FromArrays itself stays the
// zero-copy, no-validation fast path (verified well-formed inputs pass).

import (
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

func TestCSR_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		vertices []uint64
		edges    []graph.NodeID
		weights  []int64
		order    uint64
		size     uint64
		wantErr  bool
	}{
		{
			name:     "well-formed 0->1,1->0",
			vertices: []uint64{0, 1, 2},
			edges:    []graph.NodeID{1, 0},
			order:    2, size: 2,
			wantErr: false,
		},
		{
			name:     "empty graph",
			vertices: []uint64{0},
			edges:    nil,
			order:    0, size: 0,
			wantErr: false,
		},
		{
			name:     "out-of-range destination (auditor repro)",
			vertices: []uint64{0, 1, 1},
			edges:    []graph.NodeID{5},
			order:    2, size: 1,
			wantErr: true,
		},
		{
			name:     "non-monotonic offsets",
			vertices: []uint64{0, 2, 1},
			edges:    []graph.NodeID{0, 1},
			order:    2, size: 2,
			wantErr: true,
		},
		{
			name:     "vertices[last] != len(edges)",
			vertices: []uint64{0, 5},
			edges:    []graph.NodeID{0},
			order:    1, size: 1,
			wantErr: true,
		},
		{
			name:     "weights length mismatch",
			vertices: []uint64{0, 1, 2},
			edges:    []graph.NodeID{1, 0},
			weights:  []int64{1},
			order:    2, size: 2,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := FromArrays(tc.vertices, tc.edges, tc.weights, tc.order, tc.size)
			err := c.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want a malformed error")
				}
				if !errors.Is(err, ErrMalformedCSR) {
					t.Fatalf("Validate() = %v, want errors.Is(ErrMalformedCSR)", err)
				}
			} else if err != nil {
				t.Fatalf("Validate() = %v, want nil (well-formed)", err)
			}
		})
	}
}
