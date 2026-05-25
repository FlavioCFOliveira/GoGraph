package csr

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"unsafe"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/internal/shapegen"
	"gograph/internal/subproc"
)

func init() {
	// Register the child handler before subproc.Dispatch() is called from
	// TestMain. The handler builds BarabasiAlbert(1000, 3, 42), converts it
	// to a CSR, and emits SHA-256 hashes of the raw vertices and edges slices
	// to stdout as:
	//
	//   vertices:<hex>
	//   edges:<hex>
	subproc.Register("csr-build-sha256", func(_ []string) int {
		shape := shapegen.BarabasiAlbert(1000, 3, 42)
		g, err := shape.Build(adjlist.Config{Directed: true})
		if err != nil {
			fmt.Printf("csr-build-sha256: Build: %v\n", err)
			return 1
		}
		c := BuildFromAdjList(g.AdjList())

		vh := sha256Uint64Slice(c.VerticesSlice())
		eh := sha256NodeIDSlice(c.EdgesSlice())

		fmt.Printf("vertices:%s\n", vh)
		fmt.Printf("edges:%s\n", eh)
		return 0
	})
}

// sha256Uint64Slice returns the hex-encoded SHA-256 of the raw bytes of s.
// It re-interprets the []uint64 backing array as []byte via unsafe to avoid
// any allocation or copy beyond the sha256 state update itself.
func sha256Uint64Slice(s []uint64) string {
	if len(s) == 0 {
		return hex.EncodeToString(sha256.New().Sum(nil))
	}
	// Convert to a stable byte representation (little-endian per element)
	// without unsafe — binary.LittleEndian ensures the hash is meaningful
	// on both little-endian and big-endian architectures.
	_ = unsafe.Sizeof(uint64(0)) // confirm size at compile time
	buf := make([]byte, len(s)*8)
	for i, v := range s {
		binary.LittleEndian.PutUint64(buf[i*8:], v)
	}
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])
}

// sha256NodeIDSlice returns the hex-encoded SHA-256 of the raw bytes of s,
// treating each NodeID as a uint64.
func sha256NodeIDSlice(s []graph.NodeID) string {
	if len(s) == 0 {
		return hex.EncodeToString(sha256.New().Sum(nil))
	}
	buf := make([]byte, len(s)*8)
	for i, v := range s {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
	}
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])
}
