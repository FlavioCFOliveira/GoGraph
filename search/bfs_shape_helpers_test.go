package search

// bfs_shape_helpers_test.go — shared helpers for the Sprint 62
// shape-based BFS tests (Tasks 559, 561, 562, 564, 567, 570, 574, 577).
//
// These are internal to the test binary; they are not exported and
// carry no direct test functions.

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// defaultCfg returns an adjlist.Config with defaults suitable for
// shape tests. Generators that fix cfg.Directed or cfg.Multigraph
// inside Build override these fields themselves.
func defaultCfg() adjlist.Config {
	return adjlist.Config{}
}

// itoa converts a non-negative int to its decimal string
// representation. Used for t.Run subtest names throughout the
// Sprint-62 shape test files.
func itoa(n int) string { return fmt.Sprintf("%d", n) }
