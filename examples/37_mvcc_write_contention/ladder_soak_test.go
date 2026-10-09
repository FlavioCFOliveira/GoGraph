//go:build soak

package main

// ladder_soak_test.go — the soak layer of phase 6 (rmp #2934): every arm at 256
// and 1024 goroutines, the horizon capacity cliff (L09), the full-size parallel
// count (L17) and the structural self-conflict streak gate.
//
// The self-conflict streak gate (checkDisjoint) does NOT detect the absence of
// rmp #2932: built against 43c69dbe, which predates it, the longest sessionless
// self-conflict streak measured 13-28 ms (0 with GOMAXPROCS=2) against the 2 s
// retry budget; at HEAD it measured 19.5 ms at 256 and 82.6 ms at 1024
// goroutines. It stays as a guard against retry stalls; #2932's regression is
// covered by its own tests (graph/mvcc/publish_convoy_test.go).
//
// Run: go test -tags soak -run TestLadderSoak ./examples/37_mvcc_write_contention/

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/synclatency"
)

func TestLadderSoak(t *testing.T) {
	lc := ladderConfig{levels: []int{256, 1024}, totalOps: 4096, soak: true, seed: 1,
		syncLatency: synclatency.ForTest(t)}
	runLadder(t, &lc)
}
