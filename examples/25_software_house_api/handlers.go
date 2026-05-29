package main

import "net/http"

// The full implementations of these handlers — Cypher execution, the
// deterministic seed, and the stats counters — are delivered in task T4
// (handlers + JSON output + query catalogue). The stubs below let the
// server build and exercise its startup/shutdown lifecycle (task T2)
// before the query layer exists, and are replaced in T4.

func (s *Server) handleQuery(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (s *Server) handleSeed(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
