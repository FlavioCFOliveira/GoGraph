package main

// ladder_frontier.go — the frontier counters the ladder reads from lpg.MVCCStats.
//
// They are read through this one function so the negative controls of
// README.md ("Phase 6 — concurrency ladder") can build the ladder against an
// engine revision that predates the counters (43c69dbe, before rmp #2932): the
// control replaces only this file, with one that returns zeros, and the rest of
// the ladder compiles unchanged.

import "github.com/FlavioCFOliveira/GoGraph/graph/lpg"

// frontierOf returns the commits allocated but not finished, the out-of-order and
// helped publications, and the sessions waiting for the frontier.
func frontierOf(st *lpg.MVCCStats) (inFlight, outOfOrder, helped uint64, waiting int64) {
	return st.InFlightCommits, st.OutOfOrderPublications, st.HelpedPublications, st.SessionsWaiting
}
